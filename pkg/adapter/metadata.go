package adapter

// Well-known ReservationRequest and LaunchResult metadata keys. The HTTP
// dispatch bridge copies the broker's external-dispatch JSON into these keys
// before Reserve, and copies them back onto the dispatch response when the
// adapter leaves them on the reservation or launch metadata.
const (
	MetadataRunnerLabel        = "runner_label"
	MetadataRunnerName         = "runner_name"
	MetadataLaunchMode         = "launch_mode"
	MetadataExecutionID        = "execution_id"
	MetadataStatusURL          = "status_url"
	MetadataDetailsURL         = "details_url"
	MetadataGitHubTargetURL    = "github.target_url"
	MetadataGitHubRepository   = "github.repository"
	MetadataGitHubOwner        = "github.owner"
	MetadataGitHubOrganization = "github.organization"
	MetadataGitHubScopeType    = "github.scope_type"
	MetadataGitHubRunnerGroup  = "github.runner_group"
)
