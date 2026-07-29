package backend

import (
	"reflect"
	"testing"
)

func TestCanonicalCapabilityPlatformAliases(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"  ":         "",
		"docker":     "docker",
		"GPU":        "gpu",
		"linux":      CapabilityOSLinux,
		"Windows":    CapabilityOSWindows,
		"macos":      CapabilityOSMacOS,
		"darwin":     CapabilityOSMacOS,
		"os:windows": CapabilityOSWindows,
		"amd64":      CapabilityArchAMD64,
		"x64":        CapabilityArchAMD64,
		"x86_64":     CapabilityArchAMD64,
		"arch:amd64": CapabilityArchAMD64,
		"arm64":      CapabilityArchARM64,
		"aarch64":    CapabilityArchARM64,
		"arch:arm64": CapabilityArchARM64,
		"region:aws-us-east-1": "region:aws-us-east-1",
	}

	for input, want := range cases {
		if got := CanonicalCapability(input); got != want {
			t.Errorf("CanonicalCapability(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeCapabilitiesExpandsAndDedupesPlatformAliases(t *testing.T) {
	got := NormalizeCapabilities([]string{
		"Windows",
		"os:windows",
		"arm64",
		"arch:arm64",
		"docker",
		" Docker ",
		"",
	})
	want := []string{CapabilityArchARM64, "docker", CapabilityOSWindows}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeCapabilities() = %#v, want %#v", got, want)
	}
}

func TestCapabilitySetUsesCanonicalPlatformTags(t *testing.T) {
	set := CapabilitySet([]string{"x64", "linux", "docker"})
	for _, key := range []string{CapabilityArchAMD64, CapabilityOSLinux, "docker"} {
		if _, ok := set[key]; !ok {
			t.Fatalf("expected capability set to contain %q, got %#v", key, set)
		}
	}
	if _, ok := set["x64"]; ok {
		t.Fatalf("expected bare x64 alias to be expanded, got %#v", set)
	}
}
