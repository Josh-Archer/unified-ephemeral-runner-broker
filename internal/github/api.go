package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	defaultAPIURL = "https://api.github.com"
	// DefaultTokenKey is the key holding the GitHub token in github.auth.secretRef.
	DefaultTokenKey = "github_token"
	// defaultRunnerGroupID is the "Default" runner group every repository and
	// organization has.
	defaultRunnerGroupID = 1
)

// TokenFunc returns the GitHub token the broker authenticates with.
type TokenFunc func(ctx context.Context) (string, error)

// SecretReader is the subset of runtime.SecretReader used to load the token.
type SecretReader interface {
	ReadSecret(ctx context.Context, name string) (map[string]string, error)
}

// TokenFromSecret reads key from the Kubernetes secret name on every call, so
// rotated tokens are picked up without a restart.
func TokenFromSecret(reader SecretReader, name, key string) TokenFunc {
	if strings.TrimSpace(key) == "" {
		key = DefaultTokenKey
	}
	return func(ctx context.Context) (string, error) {
		if reader == nil || strings.TrimSpace(name) == "" {
			return "", errors.New("github.auth.secretRef is not configured")
		}
		data, err := reader.ReadSecret(ctx, name)
		if err != nil {
			return "", fmt.Errorf("read github token secret %s: %w", name, err)
		}
		token := strings.TrimSpace(data[key])
		if token == "" {
			return "", fmt.Errorf("github token secret %s has no %q", name, key)
		}
		return token, nil
	}
}

// Client is a minimal GitHub REST client for runner registration and run
// lookups. The token never leaves the broker.
type Client struct {
	baseURL string
	token   TokenFunc
	http    *http.Client

	rateMu        sync.Mutex
	rateRemaining int
	rateReset     time.Time
	rateKnown     bool

	apiCalls atomic.Int64
}

// RateLimit reports the last X-RateLimit-Remaining/Reset seen on a response.
func (c *Client) RateLimit() (remaining int, reset time.Time, ok bool) {
	if c == nil {
		return 0, time.Time{}, false
	}
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	return c.rateRemaining, c.rateReset, c.rateKnown
}

func (c *Client) recordRateLimit(header http.Header) {
	raw := strings.TrimSpace(header.Get("X-RateLimit-Remaining"))
	if raw == "" {
		return
	}
	remaining, err := strconv.Atoi(raw)
	if err != nil {
		return
	}
	var reset time.Time
	if secs, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Reset")), 10, 64); err == nil && secs > 0 {
		reset = time.Unix(secs, 0).UTC()
	}
	c.rateMu.Lock()
	c.rateRemaining, c.rateReset, c.rateKnown = remaining, reset, true
	c.rateMu.Unlock()
}

func NewClient(token TokenFunc, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{baseURL: defaultAPIURL, token: token, http: httpClient}
}

// WithBaseURL points the client at another API root (tests, GHES).
func (c *Client) WithBaseURL(base string) *Client {
	c.baseURL = strings.TrimRight(base, "/")
	return c
}

// JITConfig is a single-use runner registration returned by GitHub.
type JITConfig struct {
	RunnerID int64
	Encoded  string
}

// GenerateJITConfig registers an ephemeral runner on scope and returns its
// just-in-time configuration. The runner can run exactly one job and needs no
// further GitHub credentials.
func (c *Client) GenerateJITConfig(ctx context.Context, scope model.GitHubScope, runnerGroup, name string, labels []string) (JITConfig, error) {
	base, err := scopePath(scope)
	if err != nil {
		return JITConfig{}, err
	}
	groupID := int64(defaultRunnerGroupID)
	if strings.TrimSpace(runnerGroup) != "" && scope.Type == model.GitHubScopeOrganization {
		groupID, err = c.runnerGroupID(ctx, base, runnerGroup)
		if err != nil {
			return JITConfig{}, err
		}
	}
	body := map[string]any{
		"name":            name,
		"runner_group_id": groupID,
		"labels":          labels,
		"work_folder":     "_work",
	}
	var out struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := c.do(ctx, http.MethodPost, base+"/actions/runners/generate-jitconfig", body, &out, http.StatusCreated); err != nil {
		return JITConfig{}, fmt.Errorf("generate jit config: %w", err)
	}
	if out.EncodedJITConfig == "" {
		return JITConfig{}, errors.New("generate jit config: empty encoded_jit_config")
	}
	return JITConfig{RunnerID: out.Runner.ID, Encoded: out.EncodedJITConfig}, nil
}

// DeleteRunner removes a runner registration. A runner that is already gone
// is not an error.
func (c *Client) DeleteRunner(ctx context.Context, scope model.GitHubScope, runnerID int64) error {
	base, err := scopePath(scope)
	if err != nil {
		return err
	}
	err = c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/actions/runners/%d", base, runnerID), nil, nil, http.StatusNoContent)
	var status *StatusError
	if errors.As(err, &status) && status.Code == http.StatusNotFound {
		return nil
	}
	return err
}

// WorkflowRun is what the broker needs to know about who a run is for.
type WorkflowRun struct {
	// HeadRepository is where the run's code comes from; for a pull request
	// from a fork it differs from the run's repository.
	HeadRepository string
	// Actor is the user whose event (push, pull request, ...) created the run.
	Actor string
	// TriggeringActor started this attempt; it differs from Actor on re-runs.
	TriggeringActor string
}

// WorkflowJob is the queued-job subset needed by the broker reconciler.
type WorkflowJob struct {
	ID     int64    `json:"id"`
	RunID  int64    `json:"run_id"`
	Status string   `json:"status"`
	Labels []string `json:"labels"`
}

// ListRepositories returns repositories visible to the configured owner or
// organization installation. Repository scopes return their single repo.
func (c *Client) ListRepositories(ctx context.Context, scope model.GitHubScope) ([]string, error) {
	if scope.Type == model.GitHubScopeRepository {
		repo, err := scope.ForRepository("")
		if err != nil {
			return nil, err
		}
		return []string{repo.Owner + "/" + repo.Repository}, nil
	}
	owner := strings.TrimSpace(scope.Owner)
	if scope.Type == model.GitHubScopeOrganization {
		owner = strings.TrimSpace(scope.Organization)
	}
	if owner == "" {
		return nil, errors.New("GitHub owner scope is empty")
	}
	// A GitHub App installation token sees its private repositories only
	// through /installation/repositories; /users/{owner}/repos lists public
	// repositories alone, which silently dropped private repos such as
	// a private home-lab repository from the queued-job reconcile (home#4157). Other
	// tokens (PATs) get 401/403/404 here and use the owner listing below.
	if repos, ok, err := c.listInstallationRepositories(ctx, owner); ok {
		return repos, err
	}
	path := "/orgs/" + url.PathEscape(owner) + "/repos"
	var repos []string
	for page := 1; page <= MaxRepositoryPages; page++ {
		var out []struct {
			FullName string `json:"full_name"`
		}
		query := "type=all"
		err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s?%s&per_page=100&page=%d", path, query, page), nil, &out, http.StatusOK)
		if page == 1 && scope.Type == model.GitHubScopeOwner {
			var status *StatusError
			if errors.As(err, &status) && status.Code == http.StatusNotFound {
				// Owner scope also supports a personal account; GitHub exposes
				// those repositories through /users/{username}/repos.
				path = "/users/" + url.PathEscape(owner) + "/repos"
				query = "type=owner"
				err = c.do(ctx, http.MethodGet, fmt.Sprintf("%s?%s&per_page=100&page=%d", path, query, page), nil, &out, http.StatusOK)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("list repositories for %s: %w", owner, err)
		}
		for _, repo := range out {
			if strings.TrimSpace(repo.FullName) != "" {
				repos = append(repos, repo.FullName)
			}
		}
		if len(out) < 100 {
			return repos, nil
		}
	}
	log.Printf("github: repository listing for %s stopped at the %d-page cap; later repositories were skipped", owner, MaxRepositoryPages)
	return repos, fmt.Errorf("list repositories for %s: %w (%d pages)", owner, ErrPageCapReached, MaxRepositoryPages)
}

// listInstallationRepositories lists the repositories of the token's GitHub
// App installation that belong to owner. ok is false when the token is not an
// installation token, so the caller falls back to the owner listing.
func (c *Client) listInstallationRepositories(ctx context.Context, owner string) (repos []string, ok bool, err error) {
	for page := 1; page <= MaxRepositoryPages; page++ {
		var out struct {
			Repositories []struct {
				FullName string `json:"full_name"`
				Archived bool   `json:"archived"`
			} `json:"repositories"`
		}
		err := c.do(ctx, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), nil, &out, http.StatusOK)
		if err != nil {
			var status *StatusError
			if page == 1 && errors.As(err, &status) && (status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden || status.Code == http.StatusNotFound) {
				return nil, false, nil
			}
			return nil, true, fmt.Errorf("list installation repositories for %s: %w", owner, err)
		}
		for _, repo := range out.Repositories {
			if repo.Archived {
				// Archived repositories cannot have queued workflow runs.
				continue
			}
			name := strings.TrimSpace(repo.FullName)
			if repoOwner, _, found := strings.Cut(name, "/"); found && strings.EqualFold(repoOwner, owner) {
				repos = append(repos, name)
			}
		}
		if len(out.Repositories) < 100 {
			return repos, true, nil
		}
	}
	log.Printf("github: installation repository listing for %s stopped at the %d-page cap; later repositories were skipped", owner, MaxRepositoryPages)
	return repos, true, fmt.Errorf("list installation repositories for %s: %w (%d pages)", owner, ErrPageCapReached, MaxRepositoryPages)
}

// MaxQueuedRunPages caps the queued-run pages read per repository per
// reconcile pass so a deep backlog cannot exhaust the API budget.
const MaxQueuedRunPages = 10

// MaxRepositoryPages caps the repository pages read for one owner (10k repos).
const MaxRepositoryPages = 100

// MaxRunnerPages caps the self-hosted runner pages read for one scope (5k runners).
const MaxRunnerPages = 50

// MaxWorkflowJobPages caps the job pages read for one workflow run.
const MaxWorkflowJobPages = 10

// ErrPageCapReached is wrapped by list calls that stopped at their page cap.
// The results returned alongside it are valid but incomplete.
var ErrPageCapReached = errors.New("page cap reached; results truncated")

// APICalls reports how many GitHub API requests this client has issued.
func (c *Client) APICalls() int64 {
	if c == nil {
		return 0
	}
	return c.apiCalls.Load()
}

// ListQueuedWorkflowRuns returns ids of queued workflow runs in one repository
// created at or after since (zero means no bound). In-progress runs are not
// scanned: their queued jobs are rare and scanning them dominates API usage.
func (c *Client) ListQueuedWorkflowRuns(ctx context.Context, repository string, since time.Time) ([]int64, error) {
	base, err := repositoryPath(repository)
	if err != nil {
		return nil, err
	}
	var ids []int64
	truncated := false
	seen := map[int64]struct{}{}
	created := ""
	if !since.IsZero() {
		created = "&created=" + url.QueryEscape(">="+since.UTC().Format(time.RFC3339))
	}
	for _, status := range []string{"queued"} {
		for page := 1; page <= MaxQueuedRunPages; page++ {
			var out struct {
				WorkflowRuns []struct {
					ID int64 `json:"id"`
				} `json:"workflow_runs"`
			}
			path := fmt.Sprintf("%s/actions/runs?status=%s%s&per_page=100&page=%d", base, status, created, page)
			if err := c.do(ctx, http.MethodGet, path, nil, &out, http.StatusOK); err != nil {
				return nil, fmt.Errorf("list %s workflow runs for %s: %w", status, repository, err)
			}
			for _, run := range out.WorkflowRuns {
				if run.ID <= 0 {
					continue
				}
				if _, ok := seen[run.ID]; ok {
					continue
				}
				seen[run.ID] = struct{}{}
				ids = append(ids, run.ID)
			}
			if len(out.WorkflowRuns) < 100 {
				break
			}
			if page == MaxQueuedRunPages {
				truncated = true
			}
		}
	}
	if truncated {
		return ids, fmt.Errorf("list queued workflow runs for %s: %w (%d pages)", repository, ErrPageCapReached, MaxQueuedRunPages)
	}
	return ids, nil
}

// ListWorkflowJobs returns the jobs for one workflow run, following at most
// MaxWorkflowJobPages pages; a longer run returns the jobs read so far with an
// error wrapping ErrPageCapReached.
func (c *Client) ListWorkflowJobs(ctx context.Context, repository string, runID int64) ([]WorkflowJob, error) {
	base, err := repositoryPath(repository)
	if err != nil {
		return nil, err
	}
	if runID <= 0 {
		return nil, errors.New("workflow run id must be positive")
	}
	var jobs []WorkflowJob
	for page := 1; page <= MaxWorkflowJobPages; page++ {
		var out struct {
			Jobs []WorkflowJob `json:"jobs"`
		}
		path := fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100&page=%d", base, runID, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &out, http.StatusOK); err != nil {
			return nil, fmt.Errorf("list workflow jobs for %s run %d: %w", repository, runID, err)
		}
		for i := range out.Jobs {
			if out.Jobs[i].RunID == 0 {
				out.Jobs[i].RunID = runID
			}
		}
		jobs = append(jobs, out.Jobs...)
		if len(out.Jobs) < 100 {
			return jobs, nil
		}
	}
	return jobs, fmt.Errorf("list workflow jobs for %s run %d: %w (%d pages)", repository, runID, ErrPageCapReached, MaxWorkflowJobPages)
}

func repositoryPath(repository string) (string, error) {
	owner, repo, ok := strings.Cut(strings.TrimSpace(repository), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("invalid repository %q", repository)
	}
	return fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo)), nil
}

// GetWorkflowRun reads the run that a workflow job belongs to.
func (c *Client) GetWorkflowRun(ctx context.Context, repository string, runID int64) (WorkflowRun, error) {
	owner, repo, ok := strings.Cut(strings.TrimSpace(repository), "/")
	if !ok || owner == "" || repo == "" || runID <= 0 {
		return WorkflowRun{}, fmt.Errorf("invalid workflow run %s#%d", repository, runID)
	}
	type login struct {
		Login string `json:"login"`
	}
	var out struct {
		HeadRepository *struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
		Actor           *login `json:"actor"`
		TriggeringActor *login `json:"triggering_actor"`
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", url.PathEscape(owner), url.PathEscape(repo), runID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out, http.StatusOK); err != nil {
		return WorkflowRun{}, fmt.Errorf("get workflow run: %w", err)
	}
	if out.HeadRepository == nil || strings.TrimSpace(out.HeadRepository.FullName) == "" {
		return WorkflowRun{}, errors.New("get workflow run: no head_repository")
	}
	run := WorkflowRun{HeadRepository: out.HeadRepository.FullName}
	if out.Actor != nil {
		run.Actor = out.Actor.Login
	}
	if out.TriggeringActor != nil {
		run.TriggeringActor = out.TriggeringActor.Login
	}
	return run, nil
}

// WorkflowRunHeadRepository returns the repository a workflow run's code
// comes from.
func (c *Client) WorkflowRunHeadRepository(ctx context.Context, repository string, runID int64) (string, error) {
	run, err := c.GetWorkflowRun(ctx, repository, runID)
	return run.HeadRepository, err
}

func (c *Client) runnerGroupID(ctx context.Context, base, name string) (int64, error) {
	var out struct {
		RunnerGroups []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runner_groups"`
	}
	if err := c.do(ctx, http.MethodGet, base+"/actions/runner-groups?per_page=100", nil, &out, http.StatusOK); err != nil {
		return 0, fmt.Errorf("list runner groups: %w", err)
	}
	for _, group := range out.RunnerGroups {
		if strings.EqualFold(group.Name, name) {
			return group.ID, nil
		}
	}
	return 0, fmt.Errorf("runner group %q not found", name)
}

// StatusError is a non-success GitHub API response.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("github api status %d: %s", e.Code, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, want int) error {
	if c == nil || c.token == nil {
		return errors.New("github client is not configured")
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	c.apiCalls.Add(1)
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	c.recordRateLimit(resp.Header)
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != want {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &msg)
		if msg.Message == "" {
			msg.Message = strings.TrimSpace(string(data))
		}
		return &StatusError{Code: resp.StatusCode, Message: msg.Message}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode github response: %w", err)
		}
	}
	return nil
}

func scopePath(scope model.GitHubScope) (string, error) {
	switch strings.TrimSpace(scope.Type) {
	case model.GitHubScopeOrganization:
		return "/orgs/" + url.PathEscape(strings.TrimSpace(scope.Organization)), nil
	case model.GitHubScopeRepository:
		return fmt.Sprintf("/repos/%s/%s", url.PathEscape(strings.TrimSpace(scope.Owner)), url.PathEscape(strings.TrimSpace(scope.Repository))), nil
	default:
		return "", fmt.Errorf("github scope %q must be resolved to an organization or repository first", scope.Type)
	}
}

// Runner is the public identity and scheduling state of a registered worker.
type Runner struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	Status string        `json:"status"`
	Busy   bool          `json:"busy"`
	Labels []RunnerLabel `json:"labels"`
}
type RunnerLabel struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func (c *Client) ListRunners(ctx context.Context, scope model.GitHubScope) ([]Runner, error) {
	base, err := scopePath(scope)
	if err != nil {
		return nil, err
	}
	var result []Runner
	for page := 1; page <= MaxRunnerPages; page++ {
		var out struct {
			Runners []Runner `json:"runners"`
		}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/actions/runners?per_page=100&page=%d", base, page), nil, &out, http.StatusOK); err != nil {
			return nil, err
		}
		result = append(result, out.Runners...)
		if len(out.Runners) < 100 {
			return result, nil
		}
	}
	log.Printf("github: runner listing for %s stopped at the %d-page cap; later runners were skipped", base, MaxRunnerPages)
	return result, fmt.Errorf("list runners for %s: %w (%d pages)", base, ErrPageCapReached, MaxRunnerPages)
}
func (c *Client) AddRunnerLabels(ctx context.Context, scope model.GitHubScope, id int64, labels []string) error {
	base, err := scopePath(scope)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("%s/actions/runners/%d/labels", base, id), map[string]any{"labels": labels}, nil, http.StatusOK)
}
func (c *Client) RemoveRunnerLabel(ctx context.Context, scope model.GitHubScope, id int64, label string) error {
	base, err := scopePath(scope)
	if err != nil {
		return err
	}
	err = c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/actions/runners/%d/labels/%s", base, id, url.PathEscape(label)), nil, nil, http.StatusOK)
	var status *StatusError
	if errors.As(err, &status) && status.Code == http.StatusNotFound {
		return nil
	}
	return err
}
