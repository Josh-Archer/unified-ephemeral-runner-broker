package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	service        *Service
	allowedIssuers []string
	expectedAud    string
	allowAnon      bool
	requests       *prometheus.CounterVec
}

func NewServer(service *Service, allowedIssuers []string, expectedAud string, allowAnon bool) *Server {
	return &Server{
		service:        service,
		allowedIssuers: allowedIssuers,
		expectedAud:    expectedAud,
		allowAnon:      allowAnon,
		requests: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_http_requests_total",
			Help: "HTTP requests handled by the broker.",
		}, []string{"route", "method", "status"}),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/allocations", s.handleAllocations)
	mux.HandleFunc("/v1/allocations/", s.handleAllocationByID)
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", s.handleHealth)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAllocations(w http.ResponseWriter, r *http.Request) {
	if err := s.authorize(r); err != nil {
		s.writeError(w, http.StatusUnauthorized, err)
		return
	}

	switch r.Method {
	case http.MethodPost:
		var request model.AllocationRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			s.writeError(w, http.StatusBadRequest, err)
			return
		}
		allocation, err := s.service.Allocate(r.Context(), request)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err)
			return
		}
		s.writeJSON(w, http.StatusCreated, allocation)
	default:
		s.writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleAllocationByID(w http.ResponseWriter, r *http.Request) {
	if err := s.authorize(r); err != nil {
		s.writeError(w, http.StatusUnauthorized, err)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/v1/allocations/")
	if path == "" {
		s.writeError(w, http.StatusNotFound, errors.New("allocation id is required"))
		return
	}

	if strings.HasSuffix(path, "/cancel") {
		id := strings.TrimSuffix(path, "/cancel")
		id = strings.TrimSuffix(id, "/")
		if r.Method != http.MethodPost {
			s.writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		status, ok := s.service.Cancel(id)
		if !ok {
			s.writeError(w, http.StatusNotFound, errors.New("allocation not found"))
			return
		}
		s.writeJSON(w, http.StatusOK, status)
		return
	}

	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}

	status, ok := s.service.Get(path)
	if !ok {
		s.writeError(w, http.StatusNotFound, errors.New("allocation not found"))
		return
	}
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) authorize(r *http.Request) error {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		if s.allowAnon {
			return nil
		}
		return ErrMissingBearerToken
	}

	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	claims, err := ExtractClaimsUnverified(token)
	if err != nil {
		return err
	}
	return ValidateOIDCClaims(claims, s.expectedAud, s.allowedIssuers)
}

func (s *Server) writeError(w http.ResponseWriter, code int, err error) {
	s.record("error", code, http.MethodGet)
	s.writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) record(route string, status int, method string) {
	s.requests.WithLabelValues(route, method, http.StatusText(status)).Inc()
}
