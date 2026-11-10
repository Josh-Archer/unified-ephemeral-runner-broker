package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// ReapUnschedulablePods deletes broker-owned runner pods that have stayed
// Pending with PodScheduled=False/Unschedulable longer than timeout. Only pods
// carrying this backend's ownership labels are touched. Delete 404 is treated
// as already reaped so a restart can retry safely.
func (b *Backend) ReapUnschedulablePods(ctx context.Context, now time.Time, timeout time.Duration) ([]backend.UnschedulablePod, error) {
	if b.api == nil || timeout <= 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	namespaces := map[string]struct{}{}
	for _, pool := range b.cfg.Pools {
		cfg, ok := pool.Backends[b.name]
		if !ok || !cfg.Enabled {
			continue
		}
		if ns := b.namespace(cfg); ns != "" {
			namespaces[ns] = struct{}{}
		}
	}
	if len(namespaces) == 0 {
		return nil, nil
	}
	selector := url.QueryEscape(fmt.Sprintf("%s=%s,%s=%s", labelManagedBy, labelManagedByVal, labelBackend, b.name))
	var reaped []backend.UnschedulablePod
	for namespace := range namespaces {
		code, body, err := b.api.Do(ctx, http.MethodGet, podsPath(namespace, "")+"?labelSelector="+selector, nil)
		if err != nil {
			return reaped, err
		}
		if code < 200 || code >= 300 {
			return reaped, fmt.Errorf("list runner pods in %s: status %d: %s", namespace, code, kubeMessage(body))
		}
		var list podList
		if err := json.Unmarshal(body, &list); err != nil {
			return reaped, fmt.Errorf("decode runner pods in %s: %w", namespace, err)
		}
		for _, item := range list.Items {
			if !b.ownsPod(item.Metadata.Labels, item.Metadata.Name) {
				continue
			}
			since, ok := unschedulableSince(item.Status.Phase, item.Status.Conditions, item.Metadata.CreationTimestamp)
			if !ok || since.IsZero() || now.Sub(since) < timeout {
				continue
			}
			code, body, err := b.api.Do(ctx, http.MethodDelete, podsPath(namespace, item.Metadata.Name), nil)
			switch {
			case err != nil:
				return reaped, err
			case code == http.StatusNotFound:
				// Already gone; still report so the allocation can be released.
			case code < 200 || code >= 300:
				return reaped, fmt.Errorf("delete runner pod %s/%s: status %d: %s", namespace, item.Metadata.Name, code, kubeMessage(body))
			}
			allocationID := strings.TrimSpace(item.Metadata.Annotations[annotationAllocationID])
			if allocationID == "" {
				allocationID = strings.TrimSpace(item.Metadata.Labels[labelAllocation])
			}
			var jobID int64
			if raw := strings.TrimSpace(item.Metadata.Annotations[annotationWorkflowJob]); raw != "" {
				jobID, _ = strconv.ParseInt(raw, 10, 64)
			}
			reaped = append(reaped, backend.UnschedulablePod{
				AllocationID:  allocationID,
				Pool:          model.PoolName(strings.TrimSpace(item.Metadata.Labels[labelPool])),
				Repository:    strings.TrimSpace(item.Metadata.Annotations[annotationRepository]),
				WorkflowJobID: jobID,
				Metadata: map[string]string{
					"namespace": namespace,
					"pod_name":  item.Metadata.Name,
				},
			})
		}
	}
	return reaped, nil
}

func (b *Backend) ownsPod(labels map[string]string, name string) bool {
	if labels == nil {
		return false
	}
	if labels[labelManagedBy] != labelManagedByVal {
		return false
	}
	if labels[labelBackend] != string(b.name) {
		return false
	}
	if strings.TrimSpace(labels[labelAllocation]) == "" && !strings.HasPrefix(name, "uecb-runner-") {
		return false
	}
	return true
}

func unschedulableSince(phase string, conditions []podCondition, created time.Time) (time.Time, bool) {
	if !podUnschedulable(phase, conditions) {
		return time.Time{}, false
	}
	for _, condition := range conditions {
		if condition.Type == "PodScheduled" && condition.Status == "False" && condition.Reason == "Unschedulable" && !condition.LastTransitionTime.IsZero() {
			return condition.LastTransitionTime, true
		}
	}
	if !created.IsZero() {
		return created, true
	}
	return time.Time{}, false
}
