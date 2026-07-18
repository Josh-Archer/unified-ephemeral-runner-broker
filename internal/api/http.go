package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	service        *Service
	allowedIssuers []string
	expectedAud    string
	allowAnon      bool
	oidcPolicy     model.OIDCPolicyConfig
	oidcVerifier   *OIDCVerifier
	requests       *prometheus.CounterVec
}

func NewServer(service *Service, allowedIssuers []string, expectedAud string, allowAnon bool) *Server {
	return NewServerWithPolicy(service, allowedIssuers, expectedAud, allowAnon, model.OIDCPolicyConfig{})
}

// NewServerWithPolicy constructs the API server with an OIDC repository/owner allowlist.
func NewServerWithPolicy(service *Service, allowedIssuers []string, expectedAud string, allowAnon bool, policy model.OIDCPolicyConfig) *Server {
	observer := NewPrometheusObserver(prometheus.DefaultRegisterer)
	if err := observer.Register(prometheus.DefaultRegisterer); err != nil {
		panic(err)
	}
	service.SetObserver(observer)

	return &Server{
		service:        service,
		allowedIssuers: allowedIssuers,
		expectedAud:    expectedAud,
		allowAnon:      allowAnon,
		oidcPolicy:     policy,
		oidcVerifier:   NewOIDCVerifier(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
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
	if err := prometheus.Register(s.requests); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
	return s.withRequestObservability(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.service.Health(r.Context()); err != nil {
		s.writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAllocations(w http.ResponseWriter, r *http.Request) {
	claims, err := s.authorize(r)
	if err != nil {
		if errors.Is(err, ErrOIDCPolicyDenied) {
			s.writeError(w, http.StatusForbidden, err)
			return
		}
		s.writeError(w, http.StatusUnauthorized, err)
		return
	}

	switch r.Method {
	case http.MethodPost:
		request, err := decodeAllocationRequest(r.Body)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err)
			return
		}
		ctx := withPrincipal(r.Context(), claims)
		allocation, err := s.service.Allocate(ctx, request)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err)
			return
		}
		if allocation.State == model.StatePending {
			if !allocation.RetryAfter.IsZero() {
				w.Header().Set("Retry-After", retryAfterSeconds(allocation.RetryAfter, time.Now()))
			}
			s.writeJSON(w, http.StatusAccepted, allocation)
			return
		}
		s.writeJSON(w, http.StatusCreated, allocation)
	default:
		s.writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func retryAfterSeconds(retryAfter time.Time, now time.Time) string {
	if !retryAfter.After(now) {
		return "0"
	}
	seconds := int(retryAfter.Sub(now).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}

func (s *Server) handleAllocationByID(w http.ResponseWriter, r *http.Request) {
	claims, err := s.authorize(r)
	if err != nil {
		if errors.Is(err, ErrOIDCPolicyDenied) {
			s.writeError(w, http.StatusForbidden, err)
			return
		}
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
		if err := s.authorizeAllocationAccess(claims, id); err != nil {
			s.writeOwnershipError(w, err)
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

	if strings.HasSuffix(path, "/complete") {
		id := strings.TrimSuffix(path, "/complete")
		id = strings.TrimSuffix(id, "/")
		if r.Method != http.MethodPost {
			s.writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		if err := s.authorizeAllocationAccess(claims, id); err != nil {
			s.writeOwnershipError(w, err)
			return
		}

		completion, err := decodeCompletionRequest(r.Body)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err)
			return
		}
		status, ok, err := s.service.Complete(r.Context(), id, completion)
		if err != nil {
			if errors.Is(err, ErrAllocationNotFound) {
				s.writeError(w, http.StatusNotFound, err)
				return
			}
			s.writeError(w, http.StatusBadRequest, err)
			return
		}
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

	if err := s.authorizeAllocationAccess(claims, path); err != nil {
		s.writeOwnershipError(w, err)
		return
	}
	status, ok := s.service.Get(path)
	if !ok {
		s.writeError(w, http.StatusNotFound, errors.New("allocation not found"))
		return
	}
	s.writeJSON(w, http.StatusOK, status)
}

// authorize verifies the bearer token (when present) and enforces OIDC allowlist
// policy for authenticated callers. Anonymous access is only permitted when
// allowUnauthenticated is enabled.
func (s *Server) authorize(r *http.Request) (OIDCClaims, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		if s.allowAnon {
			return OIDCClaims{}, nil
		}
		return OIDCClaims{}, ErrMissingBearerToken
	}

	if !strings.HasPrefix(authHeader, "Bearer ") {
		return OIDCClaims{}, ErrInvalidBearerToken
	}
	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if token == "" {
		return OIDCClaims{}, ErrInvalidBearerToken
	}
	claims, err := s.oidcVerifier.VerifyToken(r.Context(), token, s.expectedAud, s.allowedIssuers)
	if err != nil {
		return OIDCClaims{}, err
	}
	if err := AuthorizeOIDCPolicy(claims, s.oidcPolicy); err != nil {
		return OIDCClaims{}, err
	}
	return claims, nil
}

// authorizeAllocationAccess enforces ownership on get/cancel/complete.
// When allowUnauthenticated is enabled and the request has no subject, ownership
// checks are skipped so local/test modes and runner callbacks keep working.
func (s *Server) authorizeAllocationAccess(claims OIDCClaims, id string) error {
	if s.allowAnon && claims.Sub == "" {
		return nil
	}
	status, ok := s.service.Get(id)
	if !ok {
		return ErrAllocationNotFound
	}
	if OwnershipAllows(claims, status) {
		return nil
	}
	return ErrAllocationForbidden
}

func (s *Server) writeOwnershipError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrAllocationNotFound) {
		s.writeError(w, http.StatusNotFound, err)
		return
	}
	s.writeError(w, http.StatusForbidden, err)
}

func (s *Server) writeError(w http.ResponseWriter, code int, err error) {
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

func (s *Server) withRequestObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := correlationIDFromRequest(r)
		w.Header().Set(correlationIDHeader, correlationID)
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r.WithContext(withCorrelationID(r.Context(), correlationID)))
		s.record(routeName(r.URL.Path), recorder.status, r.Method)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func routeName(path string) string {
	switch {
	case path == "/v1/allocations":
		return "/v1/allocations"
	case strings.HasPrefix(path, "/v1/allocations/"):
		return "/v1/allocations/{id}"
	case path == "/metrics":
		return "/metrics"
	case path == "/healthz":
		return "/healthz"
	default:
		return "unknown"
	}
}
