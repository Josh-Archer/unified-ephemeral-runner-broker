package fakecapacity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
)

// capacityPayload mirrors the JSON body external-dispatch capacity_url expects.
type capacityPayload struct {
	MaxRunners     int `json:"max_runners"`
	ActiveRunners  int `json:"active_runners"`
	PendingRunners int `json:"pending_runners"`
	WarmRunners    int `json:"warm_runners"`
	FreeSlots      int `json:"free_slots,omitempty"`
}

// Server is an in-process HTTP fixture for capacity_url integration tests.
// Wire Server.URL into a backend secret as capacity_url.
type Server struct {
	URL    string
	server *httptest.Server

	mu         sync.Mutex
	status     backend.CapacityStatus
	freeSlots  int // when > 0 and max is 0, publish free_slots for reconstruction
	statusCode int
	body       []byte // raw override body; when set, counters are ignored
	requireAuth string
}

// ServerOption configures a fake capacity HTTP server.
type ServerOption func(*Server)

// WithStatus sets the counters returned on successful GETs.
func WithStatus(status backend.CapacityStatus) ServerOption {
	return func(s *Server) {
		s.status = status
	}
}

// WithFreeSlotsOnly publishes free_slots without max_runners so clients that
// reconstruct max from free + used can be exercised.
func WithFreeSlotsOnly(freeSlots, active, pending, warm int) ServerOption {
	return func(s *Server) {
		s.status = backend.CapacityStatus{
			ActiveRunners:  active,
			PendingRunners: pending,
			WarmRunners:    warm,
		}
		s.freeSlots = freeSlots
	}
}

// WithStatusCode forces a non-200 HTTP status (probe failure path).
func WithStatusCode(code int) ServerOption {
	return func(s *Server) {
		s.statusCode = code
	}
}

// WithRawBody returns a fixed body (and 200 unless WithStatusCode is set).
func WithRawBody(body []byte) ServerOption {
	return func(s *Server) {
		s.body = append([]byte(nil), body...)
	}
}

// WithBearerAuth requires Authorization: Bearer <token> on each request.
func WithBearerAuth(token string) ServerOption {
	return func(s *Server) {
		s.requireAuth = token
	}
}

// NewServer starts an httptest capacity_url fixture. The server is closed via
// t.Cleanup when t is non-nil; otherwise call Close explicitly.
func NewServer(t testing.TB, opts ...ServerOption) *Server {
	s := &Server{
		status:     Free(4, 4),
		statusCode: http.StatusOK,
	}
	for _, opt := range opts {
		opt(s)
	}

	s.server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	s.URL = s.server.URL
	if t != nil {
		t.Cleanup(s.Close)
	}
	return s
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	requireAuth := s.requireAuth
	statusCode := s.statusCode
	raw := append([]byte(nil), s.body...)
	status := s.status
	freeSlots := s.freeSlots
	s.mu.Unlock()

	if requireAuth != "" {
		if got := r.Header.Get("Authorization"); got != "Bearer "+requireAuth {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	if statusCode != 0 && statusCode != http.StatusOK {
		w.WriteHeader(statusCode)
		if len(raw) > 0 {
			_, _ = w.Write(raw)
			return
		}
		_, _ = w.Write([]byte(`{"error":"capacity probe failed"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if len(raw) > 0 {
		_, _ = w.Write(raw)
		return
	}

	payload := capacityPayload{
		MaxRunners:     status.MaxRunners,
		ActiveRunners:  status.ActiveRunners,
		PendingRunners: status.PendingRunners,
		WarmRunners:    status.WarmRunners,
	}
	if freeSlots > 0 && status.MaxRunners <= 0 {
		payload.FreeSlots = freeSlots
	} else if freeSlots > 0 {
		payload.FreeSlots = freeSlots
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// SetStatus updates counters returned by successful GETs.
func (s *Server) SetStatus(status backend.CapacityStatus) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.freeSlots = 0
	s.body = nil
	if s.statusCode == 0 {
		s.statusCode = http.StatusOK
	}
}

// SetStatusCode forces the next responses to use code (use 200 to clear).
func (s *Server) SetStatusCode(code int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCode = code
}

// Close shuts down the httptest server.
func (s *Server) Close() {
	if s == nil || s.server == nil {
		return
	}
	s.server.Close()
	s.server = nil
}
