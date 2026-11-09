package dispatch

// Paths are the default routes served by NewHandler. Point the broker secret
// keys at these paths on the controller origin.
const (
	PathDispatch = "/dispatch"
	PathCleanup  = "/cleanup"
	PathHealth   = "/health"
	PathCapacity = "/capacity"

	// HeaderBackend is the broker header X-UECB-Backend.
	HeaderBackend = "X-UECB-Backend"
)

// Request is the JSON body the broker POSTs to dispatch_url.
type Request struct {
	Action            string   `json:"action"`
	Backend           string   `json:"backend"`
	AllocationID      string   `json:"allocation_id"`
	Pool              string   `json:"pool"`
	RunnerLabel       string   `json:"runner_label"`
	RunnerName        string   `json:"runner_name"`
	RunnerLabels      []string `json:"runner_labels"`
	RequestedLabels   []string `json:"requested_labels,omitempty"`
	LaunchMode        string   `json:"launch_mode,omitempty"`
	JobTimeout        string   `json:"job_timeout"`
	JobTimeoutSeconds int64    `json:"job_timeout_seconds"`
	GitHub            GitHub   `json:"github"`
}

// GitHub is the registration target included on a dispatch request.
type GitHub struct {
	ScopeType    string `json:"scope_type"`
	Organization string `json:"organization,omitempty"`
	Owner        string `json:"owner,omitempty"`
	Repository   string `json:"repository,omitempty"`
	TargetURL    string `json:"target_url"`
	RunnerGroup  string `json:"runner_group,omitempty"`
}

// Response is the JSON body returned from a successful dispatch.
type Response struct {
	RunnerLabel string            `json:"runner_label"`
	Metadata    map[string]string `json:"metadata"`
	StatusURL   string            `json:"status_url"`
	DetailsURL  string            `json:"details_url"`
	ExecutionID string            `json:"execution_id"`
}

// CleanupRequest is the JSON body the broker POSTs to cleanup_url.
type CleanupRequest struct {
	Action        string            `json:"action"`
	Backend       string            `json:"backend"`
	AllocationID  string            `json:"allocation_id"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	Pool          string            `json:"pool"`
	RunnerLabel   string            `json:"runner_label"`
	State         string            `json:"state"`
	Error         string            `json:"error,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// CapacityBody is the JSON body returned from capacity_url.
// free_slots uses the same formula as adapter.FreeSlots.
type CapacityBody struct {
	MaxRunners     int `json:"max_runners"`
	ActiveRunners  int `json:"active_runners"`
	PendingRunners int `json:"pending_runners"`
	WarmRunners    int `json:"warm_runners"`
	FreeSlots      int `json:"free_slots"`
}
