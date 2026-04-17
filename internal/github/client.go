package github

type GenerateJITConfigRequest struct {
	Name        string   `json:"name"`
	RunnerGroup string   `json:"runner_group"`
	Labels      []string `json:"labels"`
	WorkFolder  string   `json:"work_folder"`
}

func BuildGenerateJITConfigRequest(name, runnerGroup string, labels []string) GenerateJITConfigRequest {
	return GenerateJITConfigRequest{
		Name:        name,
		RunnerGroup: runnerGroup,
		Labels:      append([]string(nil), labels...),
		WorkFolder:  "_work",
	}
}
