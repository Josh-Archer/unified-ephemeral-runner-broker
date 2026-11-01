package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type secrets map[string]map[string]string

func (s secrets) ReadSecret(_ context.Context, name string) (map[string]string, error) {
	if v, ok := s[name]; ok {
		return v, nil
	}
	return nil, errors.New("not found")
}

func TestClientRegistersRunnersAndChecksRuns(t *testing.T) {
	var jitBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /repos/owner/questkit/actions/runners/generate-jitconfig":
			_ = json.NewDecoder(r.Body).Decode(&jitBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"runner":{"id":42},"encoded_jit_config":"abc"}`))
		case "DELETE /repos/owner/questkit/actions/runners/42":
			w.WriteHeader(http.StatusNoContent)
		case "DELETE /repos/owner/questkit/actions/runners/43":
			w.WriteHeader(http.StatusNotFound)
		case "GET /orgs/org/actions/runner-groups":
			_, _ = w.Write([]byte(`{"runner_groups":[{"id":1,"name":"Default"},{"id":7,"name":"uecb-full"}]}`))
		case "POST /orgs/org/actions/runners/generate-jitconfig":
			_ = json.NewDecoder(r.Body).Decode(&jitBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"runner":{"id":5},"encoded_jit_config":"org"}`))
		case "GET /repos/owner/questkit/actions/runs/9":
			_, _ = w.Write([]byte(`{"head_repository":{"full_name":"attacker/questkit"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer server.Close()

	client := NewClient(TokenFromSecret(secrets{"gh": {"github_token": "tok"}}, "gh", ""), nil).WithBaseURL(server.URL)
	repo := model.GitHubScope{Type: model.GitHubScopeRepository, Owner: "owner", Repository: "questkit"}

	jit, err := client.GenerateJITConfig(context.Background(), repo, "", "runner-1", []string{"uecb-shared"})
	if err != nil || jit.RunnerID != 42 || jit.Encoded != "abc" {
		t.Fatalf("jit = %+v, %v", jit, err)
	}
	if jitBody["name"] != "runner-1" || jitBody["runner_group_id"].(float64) != 1 || jitBody["work_folder"] != "_work" {
		t.Fatalf("jit request = %+v", jitBody)
	}
	if err := client.DeleteRunner(context.Background(), repo, 42); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteRunner(context.Background(), repo, 43); err != nil {
		t.Fatalf("an already removed runner is fine: %v", err)
	}

	org := model.GitHubScope{Type: model.GitHubScopeOrganization, Organization: "org"}
	if jit, err := client.GenerateJITConfig(context.Background(), org, "uecb-full", "r", nil); err != nil || jit.RunnerID != 5 || jitBody["runner_group_id"].(float64) != 7 {
		t.Fatalf("org jit = %+v, %v, body %+v", jit, err, jitBody)
	}

	head, err := client.WorkflowRunHeadRepository(context.Background(), "owner/questkit", 9)
	if err != nil || head != "attacker/questkit" {
		t.Fatalf("head = %q, %v", head, err)
	}
	if _, err := client.WorkflowRunHeadRepository(context.Background(), "owner/questkit", 10); err == nil {
		t.Fatal("a missing run must be an error")
	}
	if _, err := client.GenerateJITConfig(context.Background(), model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "owner"}, "", "r", nil); err == nil {
		t.Fatal("an unresolved owner scope must be rejected")
	}

	noToken := NewClient(TokenFromSecret(secrets{}, "gh", ""), nil).WithBaseURL(server.URL)
	if _, err := noToken.WorkflowRunHeadRepository(context.Background(), "owner/questkit", 9); err == nil {
		t.Fatal("missing token must fail")
	}
}

func TestGetWorkflowRunReadsActors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"head_repository":{"full_name":"owner/repo"},"actor":{"login":"dependabot[bot]"},"triggering_actor":{"login":"owner"}}`))
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	run, err := client.GetWorkflowRun(context.Background(), "owner/repo", 1)
	if err != nil || run != (WorkflowRun{HeadRepository: "owner/repo", Actor: "dependabot[bot]", TriggeringActor: "owner"}) {
		t.Fatalf("run = %+v, %v", run, err)
	}
}

func TestListsRepositoriesRunsAndJobsForQueuedReconciliation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /installation/repositories":
			w.WriteHeader(http.StatusNotFound)
		case "GET /orgs/my-org/repos":
			if r.URL.Query().Get("page") != "1" || r.URL.Query().Get("per_page") != "100" {
				t.Errorf("repository pagination query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"full_name":"my-org/home"}]`))
		case "GET /repos/my-org/home/actions/runs":
			if got := r.URL.Query().Get("created"); got != ">=2026-11-01T00:00:00Z" {
				t.Errorf("created filter = %q", got)
			}
			switch r.URL.Query().Get("status") {
			case "queued":
				w.Header().Set("X-RateLimit-Remaining", "4321")
				w.Header().Set("X-RateLimit-Reset", "1793500000")
				_, _ = w.Write([]byte(`{"workflow_runs":[{"id":12}]}`))
			default:
				t.Errorf("run status = %q", r.URL.Query().Get("status"))
				w.WriteHeader(http.StatusBadRequest)
			}
		case "GET /repos/my-org/home/actions/runs/12/jobs":
			_, _ = w.Write([]byte(`{"jobs":[{"id":99,"status":"queued","labels":["self-hosted","uecb-shared"]}]}`))
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	scope := model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "my-org"}
	repos, err := client.ListRepositories(context.Background(), scope)
	if err != nil || len(repos) != 1 || repos[0] != "my-org/home" {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	runs, err := client.ListQueuedWorkflowRuns(context.Background(), repos[0], since)
	if err != nil || len(runs) != 1 || runs[0] != 12 {
		t.Fatalf("runs=%v err=%v (in_progress runs must not be scanned)", runs, err)
	}
	if remaining, reset, ok := client.RateLimit(); !ok || remaining != 4321 || reset.Unix() != 1793500000 {
		t.Fatalf("rate limit = %d %v %v", remaining, reset, ok)
	}
	jobs, err := client.ListWorkflowJobs(context.Background(), repos[0], runs[0])
	if err != nil || len(jobs) != 1 || jobs[0].ID != 99 || jobs[0].RunID != 12 || jobs[0].Status != "queued" || len(jobs[0].Labels) != 2 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}

func TestListRepositoriesSupportsPersonalOwnerScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/installation/repositories":
			// A personal access token is not an installation token.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
		case "/orgs/personal-owner/repos":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		case "/users/personal-owner/repos":
			if r.URL.Query().Get("type") != "owner" {
				t.Errorf("user repository query=%s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"full_name":"personal-owner/project"}]`))
		default:
			t.Errorf("unexpected repository-list path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	repos, err := client.ListRepositories(context.Background(), model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "personal-owner"})
	if err != nil || len(repos) != 1 || repos[0] != "personal-owner/project" {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
}

func TestManagedRunnerLabelAPI(t *testing.T) {
	var adds []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Error("missing authentication")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/example/project/actions/runners":
			_ = json.NewEncoder(w).Encode(map[string]any{"runners": []map[string]any{{"id": 10, "name": "worker-1", "status": "online", "busy": false, "labels": []map[string]string{{"name": "Linux", "type": "read-only"}}}}})
		case "POST /repos/example/project/actions/runners/10/labels":
			var body struct {
				Labels []string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			adds = body.Labels
			_, _ = w.Write([]byte(`{"labels":[]}`))
		case "DELETE /repos/example/project/actions/runners/10/labels/shared-full":
			_, _ = w.Write([]byte(`{"labels":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	scope := model.GitHubScope{Type: model.GitHubScopeRepository, Owner: "example", Repository: "project"}
	rs, err := client.ListRunners(context.Background(), scope)
	if err != nil || len(rs) != 1 || rs[0].Name != "worker-1" {
		t.Fatalf("list %+v %v", rs, err)
	}
	if err := client.AddRunnerLabels(context.Background(), scope, 10, []string{"shared-full"}); err != nil || len(adds) != 1 {
		t.Fatalf("add %v %v", adds, err)
	}
	for _, label := range []string{"shared-full", "already-removed"} {
		if err := client.RemoveRunnerLabel(context.Background(), scope, 10, label); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListQueuedWorkflowRunsCapsPages(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		page := r.URL.Query().Get("page")
		runs := make([]string, 100)
		for i := range runs {
			runs[i] = fmt.Sprintf(`{"id":%s%03d}`, page, i+1)
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[` + strings.Join(runs, ",") + `]}`))
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	runs, err := client.ListQueuedWorkflowRuns(context.Background(), "o/r", time.Time{})
	if !errors.Is(err, ErrPageCapReached) || pages != MaxQueuedRunPages || len(runs) != MaxQueuedRunPages*100 {
		t.Fatalf("pages=%d runs=%d err=%v, want truncated results with ErrPageCapReached", pages, len(runs), err)
	}
	if got := client.APICalls(); got != int64(MaxQueuedRunPages) {
		t.Fatalf("APICalls=%d, want %d", got, MaxQueuedRunPages)
	}
}

func TestListWorkflowJobsCapsPages(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		page := r.URL.Query().Get("page")
		jobs := make([]string, 100)
		for i := range jobs {
			jobs[i] = fmt.Sprintf(`{"id":%s%03d,"status":"queued"}`, page, i+1)
		}
		_, _ = w.Write([]byte(`{"jobs":[` + strings.Join(jobs, ",") + `]}`))
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	jobs, err := client.ListWorkflowJobs(context.Background(), "o/r", 7)
	if !errors.Is(err, ErrPageCapReached) || pages != MaxWorkflowJobPages || len(jobs) != MaxWorkflowJobPages*100 {
		t.Fatalf("pages=%d jobs=%d err=%v, want truncated results with ErrPageCapReached", pages, len(jobs), err)
	}
	if jobs[0].RunID != 7 {
		t.Fatalf("run id not backfilled: %+v", jobs[0])
	}
}

func TestListWorkflowJobsShortRunIsNotTruncated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[{"id":1,"status":"queued"}]}`))
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	jobs, err := client.ListWorkflowJobs(context.Background(), "o/r", 7)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
}

func fullPageServer(t *testing.T, key string, pages *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/installation/repositories" && key != "repositories" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		*pages++
		page := r.URL.Query().Get("page")
		items := make([]string, 100)
		for i := range items {
			items[i] = fmt.Sprintf(`{"id":%s%03d,"full_name":"o/r%s-%d","name":"n%s-%d"}`, page, i+1, page, i, page, i)
		}
		body := "[" + strings.Join(items, ",") + "]"
		if key != "" {
			body = `{"` + key + `":` + body + `}`
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestListRepositoriesCapsPages(t *testing.T) {
	pages := 0
	server := fullPageServer(t, "", &pages)
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	repos, err := client.ListRepositories(context.Background(), model.GitHubScope{Type: model.GitHubScopeOrganization, Organization: "o"})
	if !errors.Is(err, ErrPageCapReached) || pages != MaxRepositoryPages || len(repos) != MaxRepositoryPages*100 {
		t.Fatalf("pages=%d repos=%d err=%v", pages, len(repos), err)
	}
}

func TestListRunnersCapsPages(t *testing.T) {
	pages := 0
	server := fullPageServer(t, "runners", &pages)
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	runners, err := client.ListRunners(context.Background(), model.GitHubScope{Type: model.GitHubScopeOrganization, Organization: "o"})
	if !errors.Is(err, ErrPageCapReached) || pages != MaxRunnerPages || len(runners) != MaxRunnerPages*100 {
		t.Fatalf("pages=%d runners=%d err=%v", pages, len(runners), err)
	}
}

// home#4157: with a GitHub App installation token, /users/{owner}/repos lists
// only public repositories, so the private Josh-Archer/home was never
// reconciled (the deep pass ended after the 44 public repos). The installation
// listing includes private repositories.
func TestListRepositoriesIncludesPrivateInstallationRepositories(t *testing.T) {
	ownerListed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/installation/repositories":
			if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
				t.Errorf("installation query=%s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"total_count":3,"repositories":[
				{"full_name":"Josh-Archer/public-tool","private":false},
				{"full_name":"Josh-Archer/home","private":true},
				{"full_name":"someone-else/shared","private":false}]}`))
		case "/orgs/Josh-Archer/repos", "/users/Josh-Archer/repos":
			ownerListed = true
			// What GitHub returns to an installation token: public repos only.
			_, _ = w.Write([]byte(`[{"full_name":"Josh-Archer/public-tool"}]`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	repos, err := client.ListRepositories(context.Background(), model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "josh-archer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0] != "Josh-Archer/public-tool" || repos[1] != "Josh-Archer/home" {
		t.Fatalf("repos=%v, want the public and the private Josh-Archer repository only", repos)
	}
	if ownerListed {
		t.Fatal("owner listing used although the installation listing succeeded")
	}
}

func TestListRepositoriesInstallationErrorIsNotMaskedByFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/installation/repositories" {
			t.Errorf("unexpected fallback to %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	if _, err := client.ListRepositories(context.Background(), model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "o"}); err == nil {
		t.Fatal("want the installation listing error")
	}
}

func TestListInstallationRepositoriesCapsPages(t *testing.T) {
	pages := 0
	server := fullPageServer(t, "repositories", &pages)
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	repos, err := client.ListRepositories(context.Background(), model.GitHubScope{Type: model.GitHubScopeOrganization, Organization: "o"})
	if !errors.Is(err, ErrPageCapReached) || pages != MaxRepositoryPages || len(repos) != MaxRepositoryPages*100 {
		t.Fatalf("pages=%d repos=%d err=%v", pages, len(repos), err)
	}
}
