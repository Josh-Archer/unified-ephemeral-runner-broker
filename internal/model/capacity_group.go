package model

import (
	"sort"
	"strings"
)

// CapacityGroup is a set of backends that share one scheduling cap.
// An empty Key means the backend is not grouped with any peer.
type CapacityGroup struct {
	Key     string
	Cap     int
	Members []BackendName
}

// ResolveCapacityGroup finds backends that share scheduling capacity with name.
// Backends with the same non-empty capacityGroup setting are grouped together.
// Otherwise kubernetes backends that share required node placement and a
// required pod anti-affinity topology key are grouped, with a derived cap of
// one runner per pinned hostname when that topology key is kubernetes.io/hostname.
func ResolveCapacityGroup(cfg BrokerConfig, name BackendName) CapacityGroup {
	defs := backendDefinitions(cfg)
	target, ok := defs[name]
	if !ok {
		return CapacityGroup{Members: []BackendName{name}}
	}
	key, derivedCap := groupKeyAndCap(target)
	if key == "" {
		return CapacityGroup{Members: []BackendName{name}}
	}
	members := make([]BackendName, 0, len(defs))
	cap := 0
	for member, def := range defs {
		memberKey, memberDerived := groupKeyAndCap(def)
		if memberKey != key {
			continue
		}
		members = append(members, member)
		if def.CapacityGroupMax > 0 && (cap == 0 || def.CapacityGroupMax < cap) {
			cap = def.CapacityGroupMax
		}
		if memberDerived > 0 && (derivedCap == 0 || memberDerived < derivedCap) {
			derivedCap = memberDerived
		}
	}
	if cap == 0 {
		cap = derivedCap
	}
	sort.Slice(members, func(i, j int) bool {
		return members[i] < members[j]
	})
	if len(members) == 0 {
		members = []BackendName{name}
	}
	return CapacityGroup{Key: key, Cap: cap, Members: members}
}

func backendDefinitions(cfg BrokerConfig) map[BackendName]BackendConfig {
	defs := map[BackendName]BackendConfig{}
	for _, pool := range cfg.Pools {
		for name, backend := range pool.Backends {
			if _, exists := defs[name]; exists {
				continue
			}
			defs[name] = backend
		}
	}
	return defs
}

func groupKeyAndCap(cfg BackendConfig) (string, int) {
	if name := strings.TrimSpace(cfg.CapacityGroup); name != "" {
		cap := cfg.CapacityGroupMax
		if cap <= 0 {
			cap = derivedHostnameCap(cfg)
		}
		return "named:" + name, cap
	}
	place, hosts, topo := placementAndAntiAffinity(cfg)
	if place == "" || topo == "" {
		return "", 0
	}
	cap := 0
	if topo == "kubernetes.io/hostname" && len(hosts) > 0 {
		cap = len(hosts)
	}
	if cfg.CapacityGroupMax > 0 {
		cap = cfg.CapacityGroupMax
	}
	return "place:" + place + "|topo:" + topo, cap
}

func derivedHostnameCap(cfg BackendConfig) int {
	_, hosts, topo := placementAndAntiAffinity(cfg)
	if topo == "kubernetes.io/hostname" && len(hosts) > 0 {
		return len(hosts)
	}
	return 0
}

func placementAndAntiAffinity(cfg BackendConfig) (place string, hosts []string, topo string) {
	if cfg.Kubernetes == nil {
		return "", nil, ""
	}
	spec := cfg.Kubernetes.PodSpec
	hosts = hostnamePins(spec)
	place = placementFingerprint(spec)
	topo = requiredAntiAffinityTopology(spec)
	return place, hosts, topo
}

func hostnamePins(spec map[string]any) []string {
	seen := map[string]struct{}{}
	var hosts []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		hosts = append(hosts, value)
	}
	if spec == nil {
		return nil
	}
	add(asString(spec["nodeName"]))
	if selector := asMap(spec["nodeSelector"]); selector != nil {
		add(asString(selector["kubernetes.io/hostname"]))
	}
	for _, expr := range requiredMatchExpressions(spec) {
		if asString(expr["key"]) != "kubernetes.io/hostname" || !strings.EqualFold(asString(expr["operator"]), "In") {
			continue
		}
		for _, value := range asStringList(expr["values"]) {
			add(value)
		}
	}
	sort.Strings(hosts)
	return hosts
}

func placementFingerprint(spec map[string]any) string {
	if spec == nil {
		return ""
	}
	var parts []string
	if nodeName := asString(spec["nodeName"]); nodeName != "" {
		parts = append(parts, "nodeName="+nodeName)
	}
	if selector := asMap(spec["nodeSelector"]); selector != nil {
		keys := make([]string, 0, len(selector))
		for key := range selector {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, "selector:"+key+"="+asString(selector[key]))
		}
	}
	for _, expr := range requiredMatchExpressions(spec) {
		key := asString(expr["key"])
		op := asString(expr["operator"])
		values := asStringList(expr["values"])
		sort.Strings(values)
		parts = append(parts, "affinity:"+key+"|"+op+"|"+strings.Join(values, ","))
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func requiredMatchExpressions(spec map[string]any) []map[string]any {
	affinity := asMap(spec["affinity"])
	if affinity == nil {
		return nil
	}
	nodeAffinity := asMap(affinity["nodeAffinity"])
	if nodeAffinity == nil {
		return nil
	}
	required := asMap(nodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"])
	if required == nil {
		return nil
	}
	var exprs []map[string]any
	for _, term := range asList(required["nodeSelectorTerms"]) {
		termMap := asMap(term)
		if termMap == nil {
			continue
		}
		for _, expr := range asList(termMap["matchExpressions"]) {
			if exprMap := asMap(expr); exprMap != nil {
				exprs = append(exprs, exprMap)
			}
		}
	}
	return exprs
}

func requiredAntiAffinityTopology(spec map[string]any) string {
	affinity := asMap(spec["affinity"])
	if affinity == nil {
		return ""
	}
	anti := asMap(affinity["podAntiAffinity"])
	if anti == nil {
		return ""
	}
	seen := map[string]struct{}{}
	var keys []string
	for _, term := range asList(anti["requiredDuringSchedulingIgnoredDuringExecution"]) {
		termMap := asMap(term)
		if termMap == nil {
			continue
		}
		key := asString(termMap["topologyKey"])
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func asMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	return nil
}

func asList(value any) []any {
	if value == nil {
		return nil
	}
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}

func asString(value any) string {
	if value == nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func asStringList(value any) []string {
	list := asList(value)
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if text := asString(item); text != "" {
			out = append(out, text)
		}
	}
	return out
}
