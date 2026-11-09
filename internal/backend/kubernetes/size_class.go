package kubernetes

import (
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// sizeClassLabels prefers the labels the allocation will register. Callers
// that only set RequestedLabels still select a class.
func sizeClassLabels(request model.AllocationRequest, allocation model.AllocationStatus) []string {
	if len(request.Labels) > 0 {
		return request.Labels
	}
	if allocation.Request != nil && len(allocation.Request.Labels) > 0 {
		return allocation.Request.Labels
	}
	return allocation.RequestedLabels
}

// applySizeClassResources overlays the class onto the runner container and any
// container named dind. A missing dind sidecar is left absent: the class does
// not invent one. Fields the class leaves empty keep the pod template's values.
func applySizeClassResources(spec map[string]any, runnerContainer string, class model.SizeClassConfig) {
	applyContainerResources(spec, "containers", runnerContainer, class.Runner)
	applyContainerResources(spec, "initContainers", runnerContainer, class.Runner)
	applyContainerResources(spec, "containers", dindContainerName, class.Dind)
	applyContainerResources(spec, "initContainers", dindContainerName, class.Dind)
}

func applyContainerResources(spec map[string]any, field, name string, resources model.ContainerResources) {
	if resources.Empty() || strings.TrimSpace(name) == "" {
		return
	}
	list, _ := spec[field].([]any)
	for _, raw := range list {
		container, ok := raw.(map[string]any)
		if !ok || container["name"] != name {
			continue
		}
		overlayContainerResources(container, resources)
	}
}

func overlayContainerResources(container map[string]any, resources model.ContainerResources) {
	current, _ := container["resources"].(map[string]any)
	if current == nil {
		current = map[string]any{}
	}
	if !resources.Requests.Empty() {
		current["requests"] = mergeQuantities(current["requests"], resources.Requests)
	}
	if !resources.Limits.Empty() {
		current["limits"] = mergeQuantities(current["limits"], resources.Limits)
	}
	container["resources"] = current
}

func mergeQuantities(existing any, quantity model.ResourceQuantity) map[string]any {
	out := map[string]any{}
	if values, ok := existing.(map[string]any); ok {
		for key, value := range values {
			out[key] = value
		}
	}
	set := func(key, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			out[key] = value
		}
	}
	set("cpu", quantity.CPU)
	set("memory", quantity.Memory)
	set("ephemeral-storage", quantity.EphemeralStorage)
	return out
}
