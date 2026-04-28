package api

import (
	"bytes"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestDecodeAllocationRequestAcceptsDurationString(t *testing.T) {
	request, err := decodeAllocationRequest(bytes.NewReader([]byte(`{"pool":"lite","job_timeout":"15m"}`)))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if request.JobTimeout != 15*time.Minute {
		t.Fatalf("expected 15m timeout, got %s", request.JobTimeout)
	}
}

func TestDecodeAllocationRequestAcceptsDurationNumber(t *testing.T) {
	request, err := decodeAllocationRequest(bytes.NewReader([]byte(`{"pool":"lite","job_timeout":900000000000}`)))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if request.JobTimeout != 15*time.Minute {
		t.Fatalf("expected 900000000000 nanoseconds, got %s", request.JobTimeout)
	}
}

func TestDecodeAllocationRequestRejectsInvalidDuration(t *testing.T) {
	_, err := decodeAllocationRequest(bytes.NewReader([]byte(`{"pool":"lite","job_timeout": "nonsense"}`)))
	if err == nil {
		t.Fatal("expected invalid job_timeout to fail")
	}
}

func TestDecodeAllocationRequestParsesBackendAsString(t *testing.T) {
	request, err := decodeAllocationRequest(bytes.NewReader([]byte(`{"pool":"lite","backend":"codebuild","job_timeout":"1m"}`)))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if request.Backend == nil || *request.Backend != model.BackendCodeBuild {
		t.Fatalf("expected backend codebuild, got %#v", request.Backend)
	}
}

func TestDecodeAllocationRequestParsesLambdaBackendAsString(t *testing.T) {
	request, err := decodeAllocationRequest(bytes.NewReader([]byte(`{"pool":"lite","backend":"lambda","job_timeout":"1m"}`)))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if request.Backend == nil || *request.Backend != model.BackendLambda {
		t.Fatalf("expected backend lambda, got %#v", request.Backend)
	}
}

func TestDecodeAllocationRequestParsesTenant(t *testing.T) {
	request, err := decodeAllocationRequest(bytes.NewReader([]byte(`{"pool":"lite","tenant":"tenant-a","job_timeout":"1m"}`)))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if request.Tenant != "tenant-a" {
		t.Fatalf("expected tenant-a, got %q", request.Tenant)
	}
}

func TestDecodeAllocationRequestParsesPriorityClass(t *testing.T) {
	request, err := decodeAllocationRequest(bytes.NewReader([]byte(`{"pool":"lite","priority_class":"high","job_timeout":"1m"}`)))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if request.PriorityClass != "high" {
		t.Fatalf("expected high priority class, got %q", request.PriorityClass)
	}
}

func TestDecodeAllocationRequestCopiesLabels(t *testing.T) {
	request, err := decodeAllocationRequest(bytes.NewReader([]byte(`{"pool":"lite","labels":["a","b","c"]}`)))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(request.Labels) != 3 || request.Labels[0] != "a" || request.Labels[2] != "c" {
		t.Fatalf("unexpected labels: %#v", request.Labels)
	}
}

func TestDecodeAllocationRequestCopiesCapabilities(t *testing.T) {
	request, err := decodeAllocationRequest(bytes.NewReader([]byte(`{"pool":"lite","required_capabilities":["region:gcp-us-central1"],"excluded_capabilities":["cluster-local"]}`)))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(request.RequiredCapabilities) != 1 || request.RequiredCapabilities[0] != "region:gcp-us-central1" {
		t.Fatalf("unexpected required capabilities: %#v", request.RequiredCapabilities)
	}
	if len(request.ExcludedCapabilities) != 1 || request.ExcludedCapabilities[0] != "cluster-local" {
		t.Fatalf("unexpected excluded capabilities: %#v", request.ExcludedCapabilities)
	}
}
