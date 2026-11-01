package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func (b *Backend) checkNodeGuard(ctx context.Context, guard *model.KubernetesNodeGuardConfig) error {
	if b.api == nil {
		return fmt.Errorf("node guard requires Kubernetes API")
	}
	code, body, err := b.api.Do(ctx, http.MethodGet, "/api/v1/nodes/"+url.PathEscape(guard.NodeName), nil)
	if err != nil {
		return fmt.Errorf("read guarded node: %w", err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("read guarded node: HTTP %d", code)
	}
	var node struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
			Taints        []struct {
				Effect string `json:"effect"`
			} `json:"taints"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &node); err != nil {
		return fmt.Errorf("decode guarded node: %w", err)
	}
	if node.Kind != "Node" || node.Metadata.Name != guard.NodeName || node.Spec.Unschedulable {
		return fmt.Errorf("guarded node is unavailable")
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			ready = true
		}
	}
	if !ready {
		return fmt.Errorf("guarded node is not Ready")
	}
	for key, want := range guard.RequiredLabels {
		if got, ok := node.Metadata.Labels[key]; !ok || got != want {
			return fmt.Errorf("guarded node is not admitted")
		}
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect == "NoSchedule" || taint.Effect == "NoExecute" {
			return fmt.Errorf("guarded node has a scheduling taint")
		}
	}
	return nil
}

// enforceNodeGuard preserves scheduler admission: nodeName would bypass it.
// Required labels and a metadata.name field constraint are ANDed with existing
// pod constraints, so admission revocation before scheduling also blocks pods.
func enforceNodeGuard(spec map[string]any, guard *model.KubernetesNodeGuardConfig) error {
	if name, _ := spec["nodeName"].(string); name != "" {
		return fmt.Errorf("guarded runner podSpec.nodeName bypasses scheduler admission")
	}
	selector, ok := spec["nodeSelector"].(map[string]any)
	if spec["nodeSelector"] != nil && !ok {
		return fmt.Errorf("podSpec.nodeSelector must be a mapping")
	}
	if selector == nil {
		selector = map[string]any{}
	}
	for key, value := range guard.RequiredLabels {
		if current, exists := selector[key]; exists && current != value {
			return fmt.Errorf("podSpec.nodeSelector conflicts with guarded label %s", key)
		}
		selector[key] = value
	}
	spec["nodeSelector"] = selector
	mapping := func(parent map[string]any, key string) (map[string]any, error) {
		if parent[key] == nil {
			parent[key] = map[string]any{}
		}
		value, ok := parent[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("guarded podSpec %s must be a mapping", key)
		}
		return value, nil
	}
	affinity, err := mapping(spec, "affinity")
	if err != nil {
		return err
	}
	nodeAffinity, err := mapping(affinity, "nodeAffinity")
	if err != nil {
		return err
	}
	required, err := mapping(nodeAffinity, "requiredDuringSchedulingIgnoredDuringExecution")
	if err != nil {
		return err
	}
	terms, ok := required["nodeSelectorTerms"].([]any)
	if required["nodeSelectorTerms"] != nil && !ok {
		return fmt.Errorf("nodeSelectorTerms must be a list")
	}
	if len(terms) == 0 {
		terms = []any{map[string]any{}}
	}
	for _, raw := range terms {
		term, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("nodeSelectorTerms must contain mappings")
		}
		fields, ok := term["matchFields"].([]any)
		if term["matchFields"] != nil && !ok {
			return fmt.Errorf("matchFields must be a list")
		}
		term["matchFields"] = append(fields, map[string]any{"key": "metadata.name", "operator": "In", "values": []any{guard.NodeName}})
	}
	required["nodeSelectorTerms"] = terms
	return nil
}

// nodeReadiness caches per-node Ready lookups for one inventory pass.
type nodeReadiness struct {
	b     *Backend
	cache map[string]bool
}

func (b *Backend) newNodeReadiness() *nodeReadiness {
	return &nodeReadiness{b: b, cache: map[string]bool{}}
}

// ready reports whether a pod bound to nodeName can still be running a job.
// Unscheduled pods (empty nodeName) are ready (pending). A node that is gone
// or whose Ready condition is not True is not ready. Lookup errors other than
// NotFound are treated as ready so an API blip never frees occupied slots.
func (n *nodeReadiness) ready(ctx context.Context, nodeName string) bool {
	if nodeName == "" || n.b.api == nil {
		return true
	}
	if v, ok := n.cache[nodeName]; ok {
		return v
	}
	v := n.lookup(ctx, nodeName)
	n.cache[nodeName] = v
	return v
}

func (n *nodeReadiness) lookup(ctx context.Context, nodeName string) bool {
	code, body, err := n.b.api.Do(ctx, http.MethodGet, "/api/v1/nodes/"+url.PathEscape(nodeName), nil)
	if err != nil {
		return true
	}
	if code == http.StatusNotFound {
		return false
	}
	if code != http.StatusOK {
		return true
	}
	var node struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &node); err != nil {
		return true
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" {
			return condition.Status == "True"
		}
	}
	return false
}
