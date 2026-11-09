package api

import (
	"sort"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// annotateSizeClass records the resolved class on the allocation. The implicit
// default (no size label and no configured default) leaves metadata unchanged.
func annotateSizeClass(classes map[string]model.SizeClassConfig, metadata map[string]string, labels []string) map[string]string {
	selection, err := model.ResolveSizeClass(labels, classes)
	if err != nil || !selection.AnnotatesPod() {
		return metadata
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["size_class"] = selection.Name
	return metadata
}

// applySizeClass resolves the job's size label and rewrites it to the
// canonical size-<class> form so the runner registers with that label.
// No size label leaves the request unchanged (class default).
func applySizeClass(classes map[string]model.SizeClassConfig, request model.AllocationRequest) (model.AllocationRequest, error) {
	selection, err := model.ResolveSizeClass(request.Labels, classes)
	if err != nil {
		return request, err
	}
	if selection.Label == "" {
		return request, nil
	}
	rewritten := make([]string, 0, len(request.Labels))
	wrote := false
	for _, label := range request.Labels {
		if model.IsSizeLabel(label) {
			if !wrote {
				rewritten = append(rewritten, selection.Label)
				wrote = true
			}
			continue
		}
		rewritten = append(rewritten, label)
	}
	request.Labels = rewritten
	return request, nil
}

// warmCandidateServesSize reports whether a pre-registered warm runner can take
// this job. A job that names size-<class> only matches a warm runner registered
// with that same label. GitHub assigns a job only when the runner's labels are
// a superset, so an unlabeled warm runner cannot run a sized job. A job with no
// size label can still take a sized warm runner: extra runner labels do not
// block assignment.
func warmCandidateServesSize(requestLabels, warmLabels []string) bool {
	wanted, explicit := canonicalSizeLabel(requestLabels)
	if !explicit {
		return true
	}
	have, warmExplicit := canonicalSizeLabel(warmLabels)
	return warmExplicit && have == wanted
}

func canonicalSizeLabel(labels []string) (string, bool) {
	var found string
	count := 0
	for _, label := range labels {
		if !model.IsSizeLabel(label) {
			continue
		}
		count++
		found = label
	}
	if count != 1 {
		return "", false
	}
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(found)), model.SizeLabelPrefix)
	if !model.ValidSizeClassName(name) {
		return "", false
	}
	return model.SizeLabelPrefix + name, true
}

// filterBackendsBySizeClass keeps only backends the class allows. An implicit
// default, and a class with no backends list, leaves the pool unchanged.
func filterBackendsBySizeClass(pool model.PoolConfig, request model.AllocationRequest, classes map[string]model.SizeClassConfig) (model.PoolConfig, error) {
	selection, err := model.ResolveSizeClass(request.Labels, classes)
	if err != nil {
		return model.PoolConfig{}, err
	}
	if !selection.Configured || len(selection.Class.Backends) == 0 {
		return pool, nil
	}
	allowed := make(map[model.BackendName]struct{}, len(selection.Class.Backends))
	for _, name := range selection.Class.Backends {
		allowed[name] = struct{}{}
	}
	if request.Backend != nil {
		if _, ok := allowed[*request.Backend]; !ok {
			return model.PoolConfig{}, model.SizeClassErrorf("pinned backend %q cannot serve runner size class %q", *request.Backend, selection.Name)
		}
	}
	filtered := pool
	filtered.Backends = make(map[model.BackendName]model.BackendConfig, len(pool.Backends))
	served := false
	for name, cfg := range pool.Backends {
		if _, ok := allowed[name]; !ok {
			continue
		}
		filtered.Backends[name] = cfg
		if cfg.Enabled && cfg.Healthy && cfg.MaxRunners > 0 {
			served = true
		}
	}
	if request.Backend != nil {
		if cfg, ok := filtered.Backends[*request.Backend]; ok && cfg.Enabled && cfg.Healthy && cfg.MaxRunners > 0 {
			return filtered, nil
		}
	}
	if !served {
		names := make([]string, 0, len(selection.Class.Backends))
		for _, name := range selection.Class.Backends {
			names = append(names, string(name))
		}
		sort.Strings(names)
		return model.PoolConfig{}, model.SizeClassErrorf("runner size class %q is not served by any enabled backend in pool %q (allows %s)", selection.Name, pool.Name, strings.Join(names, ", "))
	}
	return filtered, nil
}
