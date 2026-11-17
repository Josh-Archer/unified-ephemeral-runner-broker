// Package gputelemetry decodes server-fetched whole-device telemetry. Parsing
// does not authenticate its source, authorize work, or reserve GPU capacity.
package gputelemetry

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

type Reason string

const (
	InvalidInput     Reason = "invalid-telemetry-input"
	UpstreamFailure  Reason = "telemetry-upstream-failure"
	MissingSeries    Reason = "missing-device-series"
	AmbiguousSeries  Reason = "ambiguous-device-series"
	StaleSample      Reason = "stale-or-future-device-sample"
	IncompleteBundle Reason = "incomplete-telemetry-bundle"
)

type Rejection struct{ Reason Reason }

func (r *Rejection) Error() string { return string(r.Reason) }
func reject(r Reason) error        { return &Rejection{r} }

// Selection is trusted server configuration, never allocation-request metadata.
// Labels must identify a whole physical GPU, not a container or aggregate.
type Selection struct{ NodeLabel, UUIDLabel, Node, UUID string }
type Sample struct {
	Labels map[string]string
	At     time.Time
	// Timestamp preserves the original numeric timestamp representation.
	Timestamp string
	// Value is approximate. Never use it for integer/range admission checks.
	Value float64
	// ValueText is the original numeric string for exact downstream validation.
	ValueText string
}

// Decode selects exactly one node/UUID series from an instant vector. Other
// devices may coexist, but every series must have both identity labels. Limits
// on HTTP body size and authenticated transport belong to the caller.
func Decode(body []byte, s Selection, now time.Time, maxAge time.Duration) (Sample, error) {
	bad := func(r Reason) (Sample, error) { return Sample{}, reject(r) }
	if s.NodeLabel == "" || s.UUIDLabel == "" || s.NodeLabel == s.UUIDLabel || s.Node == "" || s.UUID == "" || now.IsZero() || maxAge <= 0 {
		return bad(InvalidInput)
	}
	if err := uniqueJSON(body); err != nil {
		return bad(InvalidInput)
	}
	if err := exactSchema(body); err != nil {
		return bad(InvalidInput)
	}
	var response struct {
		Status    string            `json:"status"`
		Error     json.RawMessage   `json:"error"`
		ErrorType json.RawMessage   `json:"errorType"`
		Warnings  []json.RawMessage `json:"warnings"`
		Infos     []json.RawMessage `json:"infos"`
		Data      struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric    map[string]string `json:"metric"`
				Value     []json.RawMessage `json:"value"`
				Histogram json.RawMessage   `json:"histogram"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil {
		return bad(InvalidInput)
	}
	if response.Status != "success" || len(response.Error) > 0 || len(response.ErrorType) > 0 || len(response.Warnings) > 0 || len(response.Infos) > 0 {
		return bad(UpstreamFailure)
	}
	if response.Data.ResultType != "vector" {
		return bad(InvalidInput)
	}
	var selected Sample
	count := 0
	for _, r := range response.Data.Result {
		if r.Metric[s.NodeLabel] == "" || r.Metric[s.UUIDLabel] == "" || len(r.Value) != 2 || len(r.Histogram) > 0 {
			return bad(InvalidInput)
		}
		var value string
		if json.Unmarshal(r.Value[1], &value) != nil {
			return bad(InvalidInput)
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return bad(InvalidInput)
		}
		at, err := timestamp(r.Value[0])
		if err != nil {
			return bad(InvalidInput)
		}
		if r.Metric[s.NodeLabel] != s.Node || r.Metric[s.UUIDLabel] != s.UUID {
			continue
		}
		count++
		if count > 1 {
			return bad(AmbiguousSeries)
		}
		if at.IsZero() || at.After(now) || at.Before(now.Add(-maxAge)) {
			return bad(StaleSample)
		}
		selected = Sample{Labels: r.Metric, At: at, Timestamp: string(r.Value[0]), Value: v, ValueText: value}
	}
	if count == 0 {
		return bad(MissingSeries)
	}
	return selected, nil
}

func timestamp(raw []byte) (time.Time, error) {
	// JSON must contain a number, not a quoted timestamp. Exact rational parsing
	// prevents float rounding from making future/stale samples appear fresh.
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return time.Time{}, reject(InvalidInput)
	}
	// Bound exponent expansion before exact rational parsing.
	if i := strings.IndexAny(string(raw), "eE"); i >= 0 {
		exponent, err := strconv.Atoi(string(raw[i+1:]))
		if err != nil || exponent < -20 || exponent > 20 {
			return time.Time{}, reject(InvalidInput)
		}
	}
	approximate, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(approximate) || math.IsInf(approximate, 0) || approximate < 0 || approximate > 1e10 || len(raw) > 64 {
		return time.Time{}, reject(InvalidInput)
	}
	r, ok := new(big.Rat).SetString(string(raw))
	if !ok || r.Sign() < 0 {
		return time.Time{}, reject(InvalidInput)
	}
	r.Mul(r, big.NewRat(1000000000, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return time.Time{}, reject(InvalidInput)
	}
	return time.Unix(0, r.Num().Int64()).UTC(), nil
}

// Reject duplicate keys at every level rather than accepting last-key-wins
// identity/status fields. Also reject concatenated documents.
func uniqueJSON(body []byte) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return reject(InvalidInput)
				}
				seen[key] = true
				if err = walk(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return reject(InvalidInput)
		}
		_, err = d.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return reject(InvalidInput)
	}
	return nil
}

// Metric identifies mandatory, separately queried inputs. Names express a
// contract, not actual exporter metric names or default PromQL expressions.
type Metric string

const (
	MemoryTotal      Metric = "memory-total-bytes"
	MemoryUsed       Metric = "memory-used-bytes"
	SliceCapacity    Metric = "slice-capacity"
	SliceUsed        Metric = "slice-used"
	SlicePending     Metric = "slice-pending"
	DeviceErrors     Metric = "device-errors"
	ProtectedLatency Metric = "protected-latency"
	ProtectedErrors  Metric = "protected-errors"
)

var required = []Metric{MemoryTotal, MemoryUsed, SliceCapacity, SliceUsed, SlicePending, DeviceErrors, ProtectedLatency, ProtectedErrors}

// DecodeBundle requires each independent input, preserving each sample's time
// and labels. It deliberately does not create a gpuadmission.Observation:
// interpreting units/error health, resolving protected workloads and reading
// authoritative durable reservations remain explicit server integration work.
func DecodeBundle(inputs map[Metric][]byte, s Selection, now time.Time, maxAge time.Duration) (map[Metric]Sample, error) {
	if len(inputs) != len(required) {
		return nil, reject(IncompleteBundle)
	}
	out := make(map[Metric]Sample, len(required))
	for _, m := range required {
		body, ok := inputs[m]
		if !ok {
			return nil, reject(IncompleteBundle)
		}
		sample, err := Decode(body, s, now, maxAge)
		if err != nil {
			return nil, err
		}
		out[m] = sample
	}
	return out, nil
}

// Schema keys are case-sensitive; metric label names remain untouched.
func exactSchema(body []byte) error {
	object := func(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return nil, reject(InvalidInput)
		}
		for key := range fields {
			found := false
			for _, a := range allowed {
				if key == a {
					found = true
					break
				}
			}
			if !found {
				return nil, reject(InvalidInput)
			}
		}
		return fields, nil
	}
	top, err := object(body, "status", "error", "errorType", "warnings", "infos", "data")
	if err != nil {
		return err
	}
	data, err := object(top["data"], "resultType", "result")
	if err != nil {
		return err
	}
	var rows []json.RawMessage
	if json.Unmarshal(data["result"], &rows) != nil {
		return reject(InvalidInput)
	}
	for _, row := range rows {
		if _, err := object(row, "metric", "value", "histogram"); err != nil {
			return err
		}
	}
	return nil
}
