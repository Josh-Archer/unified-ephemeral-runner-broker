package scaleset

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestParseMessageBatch(t *testing.T) {
	body := `{
		"messageId": 7,
		"messageType": "RunnerScaleSetJobMessages",
		"body": "[{\"messageType\":\"JobAvailable\",\"runnerRequestId\":11,\"ownerName\":\"octo\",\"repositoryName\":\"home\",\"jobId\":\"99\",\"requestLabels\":[\"uecb-shared\"]},{\"messageType\":\"JobCompleted\",\"runnerRequestId\":11,\"result\":\"succeeded\",\"runnerName\":\"runner-a\"}]",
		"statistics": {"totalAssignedJobs": 2, "totalRunningJobs": 1}
	}`
	msg, err := ParseMessage(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if msg.MessageID != 7 || msg.Statistics.TotalAssignedJobs != 2 || len(msg.JobAvailableMessages) != 1 || len(msg.JobCompletedMessages) != 1 {
		t.Fatalf("message=%+v available=%d completed=%d", msg, len(msg.JobAvailableMessages), len(msg.JobCompletedMessages))
	}
	if msg.JobAvailableMessages[0].RepositoryName != "home" || msg.JobAvailableMessages[0].RunnerRequestID != 11 {
		t.Fatalf("job=%+v", msg.JobAvailableMessages[0])
	}
	if _, err := ParseMessage(strings.NewReader(`{"messageId":1,"messageType":"Other","body":"","statistics":{}}`)); err == nil {
		t.Fatal("expected unsupported message type")
	}
}

func TestHTTPSessionPollsMessageQueueOnly(t *testing.T) {
	const token = "super-secret-token"
	var mu sync.Mutex
	var paths []string
	var caps []string
	var auths []string
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		caps = append(caps, r.Header.Get(HeaderScaleSetMaxCapacity))
		auths = append(auths, r.Header.Get("Authorization"))
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		if strings.Contains(r.URL.Path, "runs") || strings.Contains(r.URL.Path, "workflow") || strings.Contains(r.URL.Path, "/repos/") {
			t.Errorf("queued-run scan path %s", r.URL.Path)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/queue" && r.URL.Query().Get("lastMessageId") == "":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"messageId": 7,
				"messageType": "RunnerScaleSetJobMessages",
				"body": "[{\"messageType\":\"JobAssigned\",\"runnerRequestId\":3,\"repositoryName\":\"home\",\"ownerName\":\"octo\",\"jobId\":\"15\"}]",
				"statistics": {"totalAssignedJobs": 2}
			}`)
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && r.URL.Path == "/queue/7":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	session := Session{
		SessionID:               "sess-1",
		MessageQueueURL:         srv.URL + "/queue",
		MessageQueueAccessToken: token,
		Statistics:              &Statistic{},
	}
	client := NewHTTPSession(session, srv.Client())
	msg, err := client.GetMessage(context.Background(), 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if msg == nil || msg.MessageID != 7 || msg.Statistics.TotalAssignedJobs != 2 || len(msg.JobAssignedMessages) != 1 {
		t.Fatalf("message=%+v", msg)
	}
	if err := client.DeleteMessage(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	empty, err := client.GetMessage(context.Background(), 7, 4)
	if err != nil || empty != nil {
		t.Fatalf("empty=%v err=%v", empty, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(paths, ",") != "/queue,/queue/7,/queue" {
		t.Fatalf("paths=%v", paths)
	}
	if caps[0] != "4" || auths[0] != "Bearer "+token {
		t.Fatalf("caps=%v auths=%v", caps, auths)
	}
	if queries[0] != "" || !strings.Contains(queries[2], "lastMessageId=7") {
		t.Fatalf("queries=%v", queries)
	}
	if _, err := client.AcquireJobs(context.Background(), []int64{1}); !errors.Is(err, ErrAcquireNotConfigured) {
		t.Fatalf("acquire err=%v", err)
	}
}

func TestHTTPSessionRefreshesExpiredTokenOnce(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer next-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	client := NewHTTPSession(Session{
		SessionID:               "sess-1",
		MessageQueueURL:         srv.URL + "/queue",
		MessageQueueAccessToken: "old-token",
		Statistics:              &Statistic{},
	}, srv.Client())
	client.Refresh = func(context.Context, Session) (Session, error) {
		return Session{
			SessionID:               "sess-1",
			MessageQueueURL:         srv.URL + "/queue",
			MessageQueueAccessToken: "next-token",
			Statistics:              &Statistic{},
		}, nil
	}
	msg, err := client.GetMessage(context.Background(), 0, 1)
	if err != nil || msg != nil {
		t.Fatalf("msg=%v err=%v", msg, err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want one refresh", calls)
	}
	if client.Session().MessageQueueAccessToken != "next-token" {
		t.Fatal("session token was not replaced")
	}
}

func TestParseSessionHidesSecretValues(t *testing.T) {
	const token = "super-secret-token"
	_, err := ParseSession(`{"sessionId":"s","messageQueueAccessToken":"` + token + `"}`)
	if err == nil {
		t.Fatal("expected invalid session")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the token: %v", err)
	}
	session, err := ParseSession(`{
		"sessionId":"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"messageQueueUrl":"https://pipelines.example/queue",
		"messageQueueAccessToken":"` + token + `",
		"statistics":{"totalAssignedJobs":0}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if session.Statistics == nil || session.MessageQueueAccessToken != token {
		t.Fatalf("session=%+v", session)
	}
}

func TestHTTPClientsFromSecret(t *testing.T) {
	pools := []model.PoolConfig{{
		Name:     "full",
		ScaleSet: model.PoolScaleSetConfig{Name: "uecb-shared", ID: 4},
	}}
	raw := `{"sessionId":"s","messageQueueUrl":"https://example.test/q","messageQueueAccessToken":"tok","statistics":{"totalAssignedJobs":1}}`
	clients, err := HTTPClientsFromSecret(pools, map[string]string{"uecb-shared": raw}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if clients["uecb-shared"].Session().Statistics.TotalAssignedJobs != 1 {
		t.Fatalf("clients=%v", clients)
	}
	if _, err := HTTPClientsFromSecret(pools, map[string]string{}, nil, nil); err == nil {
		t.Fatal("expected missing key")
	} else if strings.Contains(err.Error(), "tok") {
		t.Fatalf("error leaked a token: %v", err)
	}
}
