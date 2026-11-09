package model

import (
	"errors"
	"testing"
)

func TestResolveSizeClass(t *testing.T) {
	classes := map[string]SizeClassConfig{
		"small": {Runner: ContainerResources{Requests: ResourceQuantity{CPU: "500m", Memory: "1Gi"}}},
		"large": {Runner: ContainerResources{Requests: ResourceQuantity{CPU: "2", Memory: "4Gi", EphemeralStorage: "16Gi"}}},
	}

	implicit, err := ResolveSizeClass([]string{"self-hosted", "uecb-full"}, classes)
	if err != nil || implicit.Name != DefaultSizeClass || implicit.Configured || implicit.Label != "" || implicit.OverlaysResources() || implicit.AnnotatesPod() {
		t.Fatalf("implicit default = %+v err=%v", implicit, err)
	}

	got, err := ResolveSizeClass([]string{"UECB-full", "Size-Large"}, classes)
	if err != nil || got.Name != "large" || got.Label != "size-large" || !got.OverlaysResources() || got.Class.Runner.Requests.CPU != "2" {
		t.Fatalf("large = %+v err=%v", got, err)
	}

	if _, err = ResolveSizeClass([]string{"size-huge"}, classes); err == nil || !errors.Is(err, ErrUnknownSizeClass) || err.Error() != `unknown runner size class "huge"` {
		t.Fatalf("unknown = %v", err)
	}

	if _, err = ResolveSizeClass([]string{"size-small", "size-large"}, classes); err == nil || !errors.Is(err, ErrUnknownSizeClass) || err.Error() != `job requests more than one runner size class ("size-small", "size-large")` {
		t.Fatalf("multiple = %v", err)
	}

	if _, err = ResolveSizeClass([]string{"size-"}, nil); err == nil || !errors.Is(err, ErrUnknownSizeClass) || err.Error() != `unknown runner size class "size-"` {
		t.Fatalf("empty = %v", err)
	}

	explicitDefault, err := ResolveSizeClass([]string{"size-default"}, nil)
	if err != nil || explicitDefault.Name != "default" || explicitDefault.Configured || explicitDefault.Label != "size-default" || !explicitDefault.AnnotatesPod() {
		t.Fatalf("explicit default = %+v err=%v", explicitDefault, err)
	}

	withDefault := map[string]SizeClassConfig{
		"default": {Runner: ContainerResources{Limits: ResourceQuantity{Memory: "2Gi"}}},
	}
	configured, err := ResolveSizeClass(nil, withDefault)
	if err != nil || !configured.Configured || !configured.OverlaysResources() || configured.Label != "" {
		t.Fatalf("configured default = %+v err=%v", configured, err)
	}
}

func TestWithoutSizeLabels(t *testing.T) {
	got := WithoutSizeLabels([]string{"self-hosted", "size-large", "uecb-full"})
	if len(got) != 2 || got[0] != "self-hosted" || got[1] != "uecb-full" {
		t.Fatalf("stripped = %#v", got)
	}
	if WithoutSizeLabels(nil) != nil {
		t.Fatal("nil labels should stay nil")
	}
}

func TestValidQuantity(t *testing.T) {
	for _, value := range []string{"", "1", "500m", "1.5", "2Gi", "16Gi", "128Mi", "1000m"} {
		if !ValidQuantity(value) {
			t.Errorf("%q should be valid", value)
		}
	}
	for _, value := range []string{"0", "0Gi", "-1", "2 GB", "cpu", "1e3"} {
		if ValidQuantity(value) {
			t.Errorf("%q should be invalid", value)
		}
	}
}
