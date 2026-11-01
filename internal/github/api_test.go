package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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
