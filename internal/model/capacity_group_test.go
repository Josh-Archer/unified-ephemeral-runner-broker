package model

import (
	"reflect"
	"testing"
)

func TestResolveCapacityGroupDerivesSharedNodeAntiAffinity(t *testing.T) {
	spec := homelab2AntiAffinitySpec("amd64")
	cfg := BrokerConfig{Pools: []PoolConfig{{
		Name: PoolFull,
		Backends: map[BackendName]BackendConfig{
			"arc": {
				Enabled: true, MaxRunners: 1,
				Kubernetes: &KubernetesBackendConfig{PodSpec: spec},
			},
			"arc-small-nodes": {
				Enabled: true, MaxRunners: 1,
				Kubernetes: &KubernetesBackendConfig{PodSpec: spec},
			},
			"kubernetes-desktop": {
				Enabled: true, MaxRunners: 10,
				Kubernetes: &KubernetesBackendConfig{PodSpec: map[string]any{
					"nodeSelector": map[string]any{"kubernetes.io/hostname": "homelabdesktop"},
				}},
			},
		},
	}}}

	group := ResolveCapacityGroup(cfg, "arc")
	if group.Key == "" || group.Cap != 1 {
		t.Fatalf("group = %+v, want derived cap 1", group)
	}
	if !reflect.DeepEqual(group.Members, []BackendName{"arc", "arc-small-nodes"}) {
		t.Fatalf("members = %v", group.Members)
	}
	if peer := ResolveCapacityGroup(cfg, "arc-small-nodes"); peer.Key != group.Key {
		t.Fatalf("sibling group %q != %q", peer.Key, group.Key)
	}
	desktop := ResolveCapacityGroup(cfg, "kubernetes-desktop")
	if desktop.Key != "" || desktop.Cap != 0 {
		t.Fatalf("desktop should not share anti-affinity cap: %+v", desktop)
	}
}

func TestResolveCapacityGroupExplicitName(t *testing.T) {
	cfg := BrokerConfig{Pools: []PoolConfig{{
		Name: PoolFull,
		Backends: map[BackendName]BackendConfig{
			"arc":                {Enabled: true, MaxRunners: 4, CapacityGroup: "homelab2", CapacityGroupMax: 1},
			"kubernetes-shared":  {Enabled: true, MaxRunners: 4, CapacityGroup: "homelab2"},
			"kubernetes-desktop": {Enabled: true, MaxRunners: 10, CapacityGroup: "desktop", CapacityGroupMax: 10},
		},
	}}}
	group := ResolveCapacityGroup(cfg, "kubernetes-shared")
	if group.Key != "named:homelab2" || group.Cap != 1 {
		t.Fatalf("group = %+v", group)
	}
	if !reflect.DeepEqual(group.Members, []BackendName{"arc", "kubernetes-shared"}) {
		t.Fatalf("members = %v", group.Members)
	}
}

func TestResolveCapacityGroupSeparatesDifferentNodePins(t *testing.T) {
	cfg := BrokerConfig{Pools: []PoolConfig{{
		Name: PoolFull,
		Backends: map[BackendName]BackendConfig{
			"amd64": {
				Enabled: true, MaxRunners: 1,
				Kubernetes: &KubernetesBackendConfig{PodSpec: homelab2AntiAffinitySpec("amd64")},
			},
			"arm64": {
				Enabled: true, MaxRunners: 1,
				Kubernetes: &KubernetesBackendConfig{PodSpec: homelab2AntiAffinitySpec("arm64")},
			},
		},
	}}}
	left := ResolveCapacityGroup(cfg, "amd64")
	right := ResolveCapacityGroup(cfg, "arm64")
	if left.Key == "" || right.Key == "" || left.Key == right.Key {
		t.Fatalf("expected distinct groups, got %q and %q", left.Key, right.Key)
	}
}

func homelab2AntiAffinitySpec(arch string) map[string]any {
	return map[string]any{
		"affinity": map[string]any{
			"nodeAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
					"nodeSelectorTerms": []any{
						map[string]any{
							"matchExpressions": []any{
								map[string]any{
									"key":      "kubernetes.io/hostname",
									"operator": "In",
									"values":   []any{"homelab2"},
								},
								map[string]any{
									"key":      "kubernetes.io/arch",
									"operator": "In",
									"values":   []any{arch},
								},
							},
						},
					},
				},
			},
			"podAntiAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": []any{
					map[string]any{"topologyKey": "kubernetes.io/hostname"},
				},
			},
		},
	}
}
