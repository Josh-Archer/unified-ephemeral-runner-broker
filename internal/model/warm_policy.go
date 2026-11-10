package model

import "strings"

// IsServerlessBackend reports job and function backends. They start per
// allocation and must not hold always-on warm runners.
func IsServerlessBackend(name BackendName) bool {
	switch name {
	case BackendCodeBuild, BackendLambda, BackendCloudRun, BackendAzureFunctions:
		return true
	default:
		return false
	}
}

// IsKubernetesFamily reports builtin ARC, builtin Kubernetes, and any pool
// backend whose driver is kubernetes. A pool may use a non-builtin name
// such as kubernetes-primary with driver: kubernetes.
func IsKubernetesFamily(name BackendName, cfg BackendConfig) bool {
	if cfg.Driver == "kubernetes" {
		return true
	}
	return name == BackendARC || name == BackendKubernetes
}

// AllowsAlwaysOnWarm reports whether warmMin/warmMax may be greater than
// zero and the broker may keep idle registered runners for this backend.
//
// ARC and Kubernetes, including driver aliases, launch a runner pod and
// register it before a job arrives. EC2 and GCE dispatch a VM through the
// external launcher. Ephemeral azure-vm does the same when secretRef points
// at that launcher.
//
// Managed azure-vm leases already-named physical machines. A warm reservation
// would count those machines twice, so it is refused. Static-label azure-vm
// (a runnerLabel and no secretRef) is one persistent label, not an ephemeral
// VM launch, so it is refused too. Serverless and desktop backends are refused.
func AllowsAlwaysOnWarm(name BackendName, cfg BackendConfig) bool {
	if IsServerlessBackend(name) || name == BackendDesktop {
		return false
	}
	if IsKubernetesFamily(name, cfg) {
		return true
	}
	switch name {
	case BackendEC2, BackendGCE:
		return true
	case BackendAzureVM:
		if cfg.AzureVM != nil {
			return false
		}
		return strings.TrimSpace(cfg.SecretRef) != ""
	default:
		return false
	}
}
