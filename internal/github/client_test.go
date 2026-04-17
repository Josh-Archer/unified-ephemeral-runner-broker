package github

import "testing"

func TestBuildGenerateJITConfigRequest(t *testing.T) {
	request := BuildGenerateJITConfigRequest("runner-a", "uecb-lite", []string{"self-hosted", "uecb"})

	if request.Name != "runner-a" {
		t.Fatalf("unexpected name: %s", request.Name)
	}

	if request.RunnerGroup != "uecb-lite" {
		t.Fatalf("unexpected runner group: %s", request.RunnerGroup)
	}

	if request.WorkFolder != "_work" {
		t.Fatalf("unexpected work folder: %s", request.WorkFolder)
	}

	if len(request.Labels) != 2 || request.Labels[1] != "uecb" {
		t.Fatalf("unexpected labels: %#v", request.Labels)
	}
}
