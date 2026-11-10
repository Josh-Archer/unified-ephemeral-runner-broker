package kubernetes

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestReapUnschedulablePodsDeletesStaleOwnedPods(t *testing.T) {
	now := time.Date(2026, 11, 10, 3, 10, 0, 0, time.UTC)
	stale := now.Add(-3 * time.Minute).Format(time.RFC3339)
	fresh := now.Add(-30 * time.Second).Format(time.RFC3339)
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/namespaces/arc-runners/pods": {http.StatusOK, `{
			"items":[
				{"metadata":{"name":"uecb-runner-stale","creationTimestamp":"` + stale + `","labels":{"app.kubernetes.io/managed-by":"unified-ephemeral-runner-broker","uecb.josh-archer.dev/backend":"kubernetes","uecb.josh-archer.dev/allocation-id":"stale","uecb.josh-archer.dev/pool":"full"},"annotations":{"uecb.josh-archer.dev/allocation-id":"stale","uecb.josh-archer.dev/workflow-job-id":"99","uecb.josh-archer.dev/repository":"example/project"}},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","lastTransitionTime":"` + stale + `","message":"didn't match pod anti-affinity rules"}]}},
				{"metadata":{"name":"uecb-runner-fresh","creationTimestamp":"` + fresh + `","labels":{"app.kubernetes.io/managed-by":"unified-ephemeral-runner-broker","uecb.josh-archer.dev/backend":"kubernetes","uecb.josh-archer.dev/allocation-id":"fresh"},"annotations":{"uecb.josh-archer.dev/allocation-id":"fresh"}},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","lastTransitionTime":"` + fresh + `"}]}},
				{"metadata":{"name":"uecb-runner-running","labels":{"app.kubernetes.io/managed-by":"unified-ephemeral-runner-broker","uecb.josh-archer.dev/backend":"kubernetes","uecb.josh-archer.dev/allocation-id":"running"}},"status":{"phase":"Running"}},
				{"metadata":{"name":"uecb-runner-other","labels":{"app.kubernetes.io/managed-by":"unified-ephemeral-runner-broker","uecb.josh-archer.dev/backend":"arc","uecb.josh-archer.dev/allocation-id":"other"}},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","lastTransitionTime":"` + stale + `"}]}}
			]
		}`},
		"DELETE /api/v1/namespaces/arc-runners/pods/uecb-runner-stale": {http.StatusOK, `{}`},
	}}
	reaped, err := New(newConfig(t), api, &fakeRegistrar{}).ReapUnschedulablePods(context.Background(), now, 2*time.Minute)
	if err != nil {
		t.Fatalf("ReapUnschedulablePods: %v", err)
	}
	if len(reaped) != 1 || reaped[0].AllocationID != "stale" || reaped[0].WorkflowJobID != 99 || reaped[0].Repository != "example/project" || reaped[0].Pool != model.PoolFull {
		t.Fatalf("reaped = %+v", reaped)
	}
	deleted := 0
	for _, call := range api.calls {
		if call.method != http.MethodDelete {
			continue
		}
		deleted++
		if !strings.HasSuffix(call.path, "/uecb-runner-stale") {
			t.Fatalf("unexpected delete %s", call.path)
		}
	}
	if deleted != 1 {
		t.Fatalf("deleted %d pods, want 1", deleted)
	}
}

func TestReapUnschedulablePodsUsesCreationWhenTransitionMissing(t *testing.T) {
	now := time.Date(2026, 11, 10, 3, 10, 0, 0, time.UTC)
	created := now.Add(-5 * time.Minute).Format(time.RFC3339)
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/namespaces/arc-runners/pods": {http.StatusOK, `{
			"items":[
				{"metadata":{"name":"uecb-runner-old","creationTimestamp":"` + created + `","labels":{"app.kubernetes.io/managed-by":"unified-ephemeral-runner-broker","uecb.josh-archer.dev/backend":"kubernetes","uecb.josh-archer.dev/allocation-id":"old"},"annotations":{"uecb.josh-archer.dev/allocation-id":"old"}},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable"}]}}
			]
		}`},
	}}
	reaped, err := New(newConfig(t), api, &fakeRegistrar{}).ReapUnschedulablePods(context.Background(), now, 2*time.Minute)
	if err != nil {
		t.Fatalf("ReapUnschedulablePods: %v", err)
	}
	if len(reaped) != 1 || reaped[0].AllocationID != "old" {
		t.Fatalf("reaped = %+v", reaped)
	}
}

func TestReapUnschedulablePodsTreatsNotFoundAsSuccess(t *testing.T) {
	now := time.Date(2026, 11, 10, 3, 10, 0, 0, time.UTC)
	stale := now.Add(-3 * time.Minute).Format(time.RFC3339)
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/namespaces/arc-runners/pods": {http.StatusOK, `{
			"items":[
				{"metadata":{"name":"uecb-runner-gone","creationTimestamp":"` + stale + `","labels":{"app.kubernetes.io/managed-by":"unified-ephemeral-runner-broker","uecb.josh-archer.dev/backend":"kubernetes","uecb.josh-archer.dev/allocation-id":"gone"},"annotations":{"uecb.josh-archer.dev/allocation-id":"gone"}},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","lastTransitionTime":"` + stale + `"}]}}
			]
		}`},
		"DELETE /api/v1/namespaces/arc-runners/pods/uecb-runner-gone": {http.StatusNotFound, `{"message":"not found"}`},
	}}
	reaped, err := New(newConfig(t), api, &fakeRegistrar{}).ReapUnschedulablePods(context.Background(), now, 2*time.Minute)
	if err != nil {
		t.Fatalf("ReapUnschedulablePods: %v", err)
	}
	if len(reaped) != 1 || reaped[0].AllocationID != "gone" {
		t.Fatalf("reaped = %+v", reaped)
	}
}

func TestReapUnschedulablePodsIdempotentWhenNothingMatches(t *testing.T) {
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/namespaces/arc-runners/pods": {http.StatusOK, `{"items":[]}`},
	}}
	reaped, err := New(newConfig(t), api, &fakeRegistrar{}).ReapUnschedulablePods(context.Background(), time.Now(), 2*time.Minute)
	if err != nil || len(reaped) != 0 {
		t.Fatalf("reaped=%v err=%v", reaped, err)
	}
}
