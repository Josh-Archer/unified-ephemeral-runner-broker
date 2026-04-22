package api

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type durationRequestValue time.Duration

func (d *durationRequestValue) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		*d = 0
		return nil
	}

	if strings.HasPrefix(raw, "\"") && strings.HasSuffix(raw, "\"") {
		var encoded string
		if err := json.Unmarshal([]byte(raw), &encoded); err != nil {
			return fmt.Errorf("invalid job_timeout: %w", err)
		}
		parsed, err := time.ParseDuration(encoded)
		if err != nil {
			return fmt.Errorf("invalid job_timeout: %w", err)
		}
		*d = durationRequestValue(parsed)
		return nil
	}

	var nanos int64
	if err := json.Unmarshal([]byte(raw), &nanos); err != nil {
		return fmt.Errorf("invalid job_timeout: expected duration string (for example \"15m\") or nanosecond integer")
	}
	*d = durationRequestValue(time.Duration(nanos))
	return nil
}

func decodeAllocationRequest(reader io.Reader) (model.AllocationRequest, error) {
	var request struct {
		Pool       model.PoolName       `json:"pool"`
		Backend    *model.BackendName   `json:"backend,omitempty"`
		JobTimeout durationRequestValue `json:"job_timeout"`
		Labels     []string             `json:"labels,omitempty"`
	}

	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&request); err != nil {
		return model.AllocationRequest{}, fmt.Errorf("invalid allocation request: %w", err)
	}

	var backend *model.BackendName
	if request.Backend != nil {
		backendCopy := *request.Backend
		backend = &backendCopy
	}

	return model.AllocationRequest{
		Pool:       request.Pool,
		Backend:    backend,
		JobTimeout: time.Duration(request.JobTimeout),
		Labels:     append([]string(nil), request.Labels...),
	}, nil
}
