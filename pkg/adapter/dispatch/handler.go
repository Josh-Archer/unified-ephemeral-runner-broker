package dispatch

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
)

// cleanupTimeout bounds post-Launch cleanup when the request context is
// already canceled (broker client timeout). Cleanup must still release the
// reserved slot; Adapter methods return ctx.Err() for a canceled context.
const cleanupTimeout = 5 * time.Second

// HandlerOptions configures NewHandler. Empty paths use PathDispatch,
// PathCleanup, PathHealth, and PathCapacity. Token, when set, requires
// Authorization: Bearer. The paths must be distinct.
type HandlerOptions struct {
	Token        string
	DispatchPath string
	CleanupPath  string
	HealthPath   string
	CapacityPath string
}

type routePaths struct {
	dispatch string
	cleanup  string
	health   string
	capacity string
}

// Handler is an http.Handler that maps the external-dispatch contract onto
// an adapter.Adapter.
type Handler struct {
	adapter adapter.Adapter
	token   string
	paths   routePaths
}

// NewHandler returns the v1 HTTP mapping for impl.
// A nil adapter or duplicate paths serves 500 with a JSON error.
func NewHandler(impl adapter.Adapter, opts HandlerOptions) http.Handler {
	paths := routePaths{
		dispatch: cleanPath(firstPath(opts.DispatchPath, PathDispatch)),
		cleanup:  cleanPath(firstPath(opts.CleanupPath, PathCleanup)),
		health:   cleanPath(firstPath(opts.HealthPath, PathHealth)),
		capacity: cleanPath(firstPath(opts.CapacityPath, PathCapacity)),
	}
	if impl == nil {
		return errorHandler(http.StatusInternalServerError, errors.New("dispatch handler has no adapter"))
	}
	if !distinctPaths(paths) {
		return errorHandler(http.StatusInternalServerError, errors.New("dispatch handler paths must be distinct"))
	}
	return &Handler{
		adapter: impl,
		token:   strings.TrimSpace(opts.Token),
		paths:   paths,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !authorized(r, h.token) {
		writeError(w, http.StatusUnauthorized, errors.New("unauthorized"))
		return
	}
	switch cleanPath(r.URL.Path) {
	case h.paths.dispatch:
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.handleDispatch(w, r)
	case h.paths.cleanup:
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.handleCleanup(w, r)
	case h.paths.health:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.handleHealth(w, r)
	case h.paths.capacity:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.handleCapacity(w, r)
	default:
		writeError(w, http.StatusNotFound, errors.New("not found"))
	}
}

func (h *Handler) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var body Request
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Action != "" && body.Action != "dispatch" {
		writeError(w, http.StatusBadRequest, errors.New("unsupported action"))
		return
	}
	if strings.TrimSpace(body.AllocationID) == "" {
		writeError(w, http.StatusBadRequest, errors.New("allocation_id is required"))
		return
	}
	request := reservationRequest(r, body)
	reservation, err := h.adapter.Reserve(r.Context(), request)
	if err != nil {
		writeAdapterError(w, err)
		return
	}
	launch, err := h.adapter.Launch(r.Context(), adapter.LaunchRequest{
		Reservation: reservation,
		Warm:        strings.EqualFold(strings.TrimSpace(body.LaunchMode), "warm"),
		Metadata:    cloneMetadata(request.Metadata),
	})
	if err != nil {
		if cleanupErr := h.releaseReservation(r.Context(), reservation, "launch failed"); cleanupErr != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("launch failed: %w; cleanup failed: %v", err, cleanupErr))
			return
		}
		writeAdapterError(w, err)
		return
	}
	response := responseFrom(body, reservation, launch)
	if strings.TrimSpace(response.RunnerLabel) == "" {
		emptyLabel := errors.New("launch returned an empty runner label")
		if cleanupErr := h.releaseReservation(r.Context(), reservation, emptyLabel.Error()); cleanupErr != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("%w; cleanup failed: %v", emptyLabel, cleanupErr))
			return
		}
		writeError(w, http.StatusBadGateway, emptyLabel)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// releaseReservation runs Cleanup on a context that outlives a canceled
// request so a broker client timeout cannot strand the reserved slot.
func (h *Handler) releaseReservation(parent context.Context, reservation adapter.Reservation, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
	defer cancel()
	return h.adapter.Cleanup(ctx, adapter.CleanupRequest{
		Reservation: reservation,
		State:       adapter.StateFailed,
		Reason:      reason,
	})
}

func (h *Handler) handleCleanup(w http.ResponseWriter, r *http.Request) {
	var body CleanupRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Action != "" && body.Action != "cleanup" {
		writeError(w, http.StatusBadRequest, errors.New("unsupported action"))
		return
	}
	if strings.TrimSpace(body.AllocationID) == "" {
		writeError(w, http.StatusBadRequest, errors.New("allocation_id is required"))
		return
	}
	err := h.adapter.Cleanup(r.Context(), adapter.CleanupRequest{
		Reservation: adapter.Reservation{
			AllocationID: body.AllocationID,
			RunnerLabel:  body.RunnerLabel,
			Metadata:     cloneMetadata(body.Metadata),
		},
		State:    adapter.AllocationState(body.State),
		Reason:   body.Error,
		Metadata: cloneMetadata(body.Metadata),
	})
	if err != nil {
		writeAdapterError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	status, err := h.adapter.Health(r.Context())
	if err != nil {
		writeAdapterError(w, err)
		return
	}
	code := http.StatusOK
	if !status.Healthy {
		code = http.StatusServiceUnavailable
	}
	payload := map[string]any{
		"healthy": status.Healthy,
		"reason":  status.Reason,
	}
	if !status.Checked.IsZero() {
		payload["checked"] = status.Checked.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, code, payload)
}

func (h *Handler) handleCapacity(w http.ResponseWriter, r *http.Request) {
	status, err := h.adapter.Capacity(r.Context())
	if err != nil {
		writeAdapterError(w, err)
		return
	}
	if status.MaxRunners < 0 || status.ActiveRunners < 0 || status.PendingRunners < 0 || status.WarmRunners < 0 {
		writeError(w, http.StatusBadGateway, errors.New("capacity counters must not be negative"))
		return
	}
	writeJSON(w, http.StatusOK, CapacityBody{
		MaxRunners:     status.MaxRunners,
		ActiveRunners:  status.ActiveRunners,
		PendingRunners: status.PendingRunners,
		WarmRunners:    status.WarmRunners,
		FreeSlots:      adapter.FreeSlots(status),
	})
}

func reservationRequest(r *http.Request, body Request) adapter.ReservationRequest {
	backendName := strings.TrimSpace(body.Backend)
	if backendName == "" {
		backendName = strings.TrimSpace(r.Header.Get(HeaderBackend))
	}
	labels := body.RunnerLabels
	if len(labels) == 0 {
		labels = body.RequestedLabels
	}
	meta := map[string]string{}
	put := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			meta[key] = value
		}
	}
	put(adapter.MetadataRunnerLabel, body.RunnerLabel)
	put(adapter.MetadataRunnerName, body.RunnerName)
	put(adapter.MetadataLaunchMode, body.LaunchMode)
	put(adapter.MetadataGitHubTargetURL, body.GitHub.TargetURL)
	put(adapter.MetadataGitHubRepository, body.GitHub.Repository)
	put(adapter.MetadataGitHubOwner, body.GitHub.Owner)
	put(adapter.MetadataGitHubOrganization, body.GitHub.Organization)
	put(adapter.MetadataGitHubScopeType, body.GitHub.ScopeType)
	put(adapter.MetadataGitHubRunnerGroup, body.GitHub.RunnerGroup)
	return adapter.ReservationRequest{
		AllocationID: strings.TrimSpace(body.AllocationID),
		Pool:         adapter.PoolName(strings.TrimSpace(body.Pool)),
		Backend:      adapter.BackendName(backendName),
		Labels:       append([]string(nil), labels...),
		JobTimeout:   jobTimeout(body),
		Metadata:     meta,
	}
}

func responseFrom(body Request, reservation adapter.Reservation, launch adapter.LaunchResult) Response {
	meta := map[string]string{}
	for key, value := range reservation.Metadata {
		meta[key] = value
	}
	for key, value := range launch.Metadata {
		meta[key] = value
	}
	label := strings.TrimSpace(launch.RunnerLabel)
	if label == "" {
		label = strings.TrimSpace(reservation.RunnerLabel)
	}
	if label == "" {
		label = strings.TrimSpace(body.RunnerLabel)
	}
	statusURL := strings.TrimSpace(launch.StatusURL)
	if statusURL == "" {
		statusURL = meta[adapter.MetadataStatusURL]
	}
	detailsURL := strings.TrimSpace(launch.DetailsURL)
	if detailsURL == "" {
		detailsURL = meta[adapter.MetadataDetailsURL]
	}
	return Response{
		RunnerLabel: label,
		Metadata:    meta,
		StatusURL:   statusURL,
		DetailsURL:  detailsURL,
		ExecutionID: meta[adapter.MetadataExecutionID],
	}
}

func jobTimeout(body Request) time.Duration {
	if body.JobTimeoutSeconds > 0 {
		return time.Duration(body.JobTimeoutSeconds) * time.Second
	}
	raw := strings.TrimSpace(body.JobTimeout)
	if raw == "" {
		return 0
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func authorized(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) < len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	presented := header[len(prefix):]
	if len(presented) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

func decodeJSON(r *http.Request, dest any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(dest); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

func writeAdapterError(w http.ResponseWriter, err error) {
	if adapter.IsCapacityExhausted(err) {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeError(w, http.StatusBadGateway, err)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
}

func errorHandler(status int, err error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, status, err)
	})
}

func firstPath(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func cleanPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

func distinctPaths(paths routePaths) bool {
	seen := map[string]struct{}{}
	for _, path := range []string{paths.dispatch, paths.cleanup, paths.health, paths.capacity} {
		if _, ok := seen[path]; ok {
			return false
		}
		seen[path] = struct{}{}
	}
	return true
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
