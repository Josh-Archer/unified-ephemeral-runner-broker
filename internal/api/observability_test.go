package api

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestFormatLogFields(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "empty",
			fields: map[string]string{},
			want:   "",
		},
		{
			name:   "nil",
			fields: nil,
			want:   "",
		},
		{
			name: "sorted keys",
			fields: map[string]string{
				"pool":          "lite",
				"backend":       "arc",
				"allocation_id": "abc",
				"state":         "ready",
			},
			want: " allocation_id=abc backend=arc pool=lite state=ready",
		},
		{
			name: "safe tokens stay unquoted",
			fields: map[string]string{
				"fallback_reason": "saturated",
				"backend":         "arc",
			},
			want: " backend=arc fallback_reason=saturated",
		},
		{
			name: "quotes spaces equals and quotes",
			fields: map[string]string{
				"note":  "has space and = sign",
				"error": `say "hi" it's bad`,
			},
			want: " error=" + strconv.Quote(`say "hi" it's bad`) + " note=" + strconv.Quote("has space and = sign"),
		},
		{
			name: "quotes crlf and control characters",
			fields: map[string]string{
				"error": "line1\r\nline2\x00\x1b",
				"pool":  "lite",
			},
			want: " error=" + strconv.Quote("line1\r\nline2\x00\x1b") + " pool=lite",
		},
		{
			name: "quotes unsafe keys",
			fields: map[string]string{
				"bad key": "value",
				"ok":      "plain",
			},
			want: " " + strconv.Quote("bad key") + "=value ok=plain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for attempt := range 32 {
				got := formatLogFields(tt.fields)
				if got != tt.want {
					t.Fatalf("attempt %d formatLogFields() = %q, want %q", attempt, got, tt.want)
				}
				if strings.ContainsAny(got, "\r\n") {
					t.Fatalf("attempt %d formatLogFields() broke into multiple lines: %q", attempt, got)
				}
			}
		})
	}
}

func TestCorrelationIDFromRequest(t *testing.T) {
	validMax := strings.Repeat("a", maxCorrelationIDLength)
	tooLong := strings.Repeat("a", maxCorrelationIDLength+1)

	tests := []struct {
		name   string
		set    bool
		header string
		want   string
		regen  bool
	}{
		{name: "valid", set: true, header: "abc-123_DEF.ghi:jkl", want: "abc-123_DEF.ghi:jkl"},
		{name: "valid trimmed", set: true, header: "  trace-correlation-1  ", want: "trace-correlation-1"},
		{name: "surrounding crlf stripped", set: true, header: "\r\nvalid-id\r\n", want: "valid-id"},
		{name: "max length", set: true, header: validMax, want: validMax},
		{name: "too long", set: true, header: tooLong, regen: true},
		{name: "crlf injection", set: true, header: "legit\r\nX-Injected: true", regen: true},
		{name: "lf injection", set: true, header: "legit\ninjected=1", regen: true},
		{name: "cr injection", set: true, header: "legit\rinjected", regen: true},
		{name: "control chars", set: true, header: "bad\x00id\x1b", regen: true},
		{name: "del control", set: true, header: "bad\x7fid", regen: true},
		{name: "tab", set: true, header: "bad\tid", regen: true},
		{name: "empty", set: true, header: "", regen: true},
		{name: "whitespace only", set: true, header: " \t\r\n", regen: true},
		{name: "missing", set: false, regen: true},
		{name: "space", set: true, header: "bad id", regen: true},
		{name: "equals", set: true, header: "bad=id", regen: true},
		{name: "quote", set: true, header: `bad"id`, regen: true},
		{name: "slash", set: true, header: "bad/id", regen: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/allocations", nil)
			if tt.set {
				request.Header.Set(correlationIDHeader, tt.header)
			}
			got := correlationIDFromRequest(request)
			if tt.regen {
				if !validCorrelationID(got) {
					t.Fatalf("regenerated correlation id %q is not valid", got)
				}
				if got == tt.header || got == strings.TrimSpace(tt.header) {
					t.Fatalf("expected a new correlation id, got %q", got)
				}
				if strings.ContainsAny(got, "\r\n\x00") {
					t.Fatalf("regenerated correlation id contains controls: %q", got)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("correlationIDFromRequest() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithCorrelationID(t *testing.T) {
	if got := correlationIDFromContext(context.Background()); got != "" {
		t.Fatalf("missing correlation id = %q, want empty", got)
	}

	tests := []struct {
		name string
		ids  []string
		want string
	}{
		{name: "stores id", ids: []string{"corr-123"}, want: "corr-123"},
		{name: "overwrites id", ids: []string{"first", "second"}, want: "second"},
		{name: "empty id", ids: []string{""}, want: ""},
		{name: "safe charset", ids: []string{"Az09._:-"}, want: "Az09._:-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			for _, id := range tt.ids {
				ctx = withCorrelationID(ctx, id)
			}
			if got := correlationIDFromContext(ctx); got != tt.want {
				t.Fatalf("correlation id = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithPrincipalAndApplyPrincipal(t *testing.T) {
	t.Run("withPrincipal", func(t *testing.T) {
		tests := []struct {
			name   string
			prior  *OIDCClaims
			claims OIDCClaims
			want   OIDCClaims
			ok     bool
		}{
			{
				name:   "stores claims",
				claims: OIDCClaims{Sub: "repo:acme/widgets:ref:refs/heads/main", Repository: "acme/widgets", RepositoryOwner: "acme"},
				want:   OIDCClaims{Sub: "repo:acme/widgets:ref:refs/heads/main", Repository: "acme/widgets", RepositoryOwner: "acme"},
				ok:     true,
			},
			{
				name:   "empty subject is not stored",
				claims: OIDCClaims{Repository: "acme/widgets", RepositoryOwner: "acme"},
				ok:     false,
			},
			{
				name:   "empty subject does not replace a stored principal",
				prior:  &OIDCClaims{Sub: "keep", Repository: "keep/repo", RepositoryOwner: "keep"},
				claims: OIDCClaims{Repository: "other/repo", RepositoryOwner: "other"},
				want:   OIDCClaims{Sub: "keep", Repository: "keep/repo", RepositoryOwner: "keep"},
				ok:     true,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				ctx := context.Background()
				if tt.prior != nil {
					ctx = withPrincipal(ctx, *tt.prior)
				}
				ctx = withPrincipal(ctx, tt.claims)
				got, ok := principalFromContext(ctx)
				if ok != tt.ok {
					t.Fatalf("principal present = %t, want %t", ok, tt.ok)
				}
				if ok && got != tt.want {
					t.Fatalf("principal = %+v, want %+v", got, tt.want)
				}
			})
		}
	})

	t.Run("applyPrincipal", func(t *testing.T) {
		tests := []struct {
			name   string
			status *model.AllocationStatus
			claims OIDCClaims
			want   model.AllocationStatus
		}{
			{
				name:   "nil status",
				status: nil,
				claims: OIDCClaims{Sub: "repo:acme/widgets:ref:refs/heads/main", Repository: "acme/widgets", RepositoryOwner: "acme"},
			},
			{
				name:   "empty subject does not overwrite",
				status: &model.AllocationStatus{Subject: "keep", Repository: "keep/repo", Owner: "keep"},
				claims: OIDCClaims{Repository: "other/repo", RepositoryOwner: "other"},
				want:   model.AllocationStatus{Subject: "keep", Repository: "keep/repo", Owner: "keep"},
			},
			{
				name:   "applies explicit repository and owner",
				status: &model.AllocationStatus{},
				claims: OIDCClaims{Sub: "repo:acme/widgets:ref:refs/heads/main", Repository: "acme/widgets", RepositoryOwner: "acme"},
				want:   model.AllocationStatus{Subject: "repo:acme/widgets:ref:refs/heads/main", Repository: "acme/widgets", Owner: "acme"},
			},
			{
				name:   "derives repository and owner from sub",
				status: &model.AllocationStatus{},
				claims: OIDCClaims{Sub: "repo:acme/widgets:environment:prod"},
				want:   model.AllocationStatus{Subject: "repo:acme/widgets:environment:prod", Repository: "acme/widgets", Owner: "acme"},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				applyPrincipal(tt.status, tt.claims)
				if tt.status == nil {
					return
				}
				if tt.status.Subject != tt.want.Subject || tt.status.Repository != tt.want.Repository || tt.status.Owner != tt.want.Owner {
					t.Fatalf("status subject/repository/owner = %q %q %q, want %q %q %q",
						tt.status.Subject, tt.status.Repository, tt.status.Owner,
						tt.want.Subject, tt.want.Repository, tt.want.Owner)
				}
			})
		}
	})
}

func TestLogAllocationEvent(t *testing.T) {
	tests := []struct {
		name          string
		correlationID string
		event         string
		fields        map[string]string
		want          string
	}{
		{
			name:          "sorted fields",
			correlationID: "corr-1",
			event:         "allocation_admitted",
			fields: map[string]string{
				"pool":          "lite",
				"backend":       "arc",
				"allocation_id": "abc",
			},
			want: "event=allocation_admitted correlation_id=corr-1 allocation_id=abc backend=arc pool=lite\n",
		},
		{
			name:          "escapes spaces equals quotes crlf and controls",
			correlationID: "corr-2",
			event:         "allocation_failed",
			fields: map[string]string{
				"error": "boom\r\nnext=injected \"quoted\"\x01",
				"note":  "has space and = sign",
				"pool":  "lite",
			},
			want: "event=allocation_failed correlation_id=corr-2 error=" + strconv.Quote("boom\r\nnext=injected \"quoted\"\x01") + " note=" + strconv.Quote("has space and = sign") + " pool=lite\n",
		},
		{
			name:          "quotes injected correlation id and event",
			correlationID: "ok\r\nevent=pwned",
			event:         "bad event",
			fields:        map[string]string{"reason": "still\none line"},
			want:          "event=" + strconv.Quote("bad event") + " correlation_id=" + strconv.Quote("ok\r\nevent=pwned") + " reason=" + strconv.Quote("still\none line") + "\n",
		},
		{
			name:          "empty correlation id and fields",
			correlationID: "",
			event:         "budget_skip",
			fields:        map[string]string{},
			want:          "event=budget_skip correlation_id=\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := captureLogOutput(t)
			ctx := withCorrelationID(context.Background(), tt.correlationID)
			logAllocationEvent(ctx, tt.event, tt.fields)
			got := output.String()
			if got != tt.want {
				t.Fatalf("log = %q, want %q", got, tt.want)
			}
			if strings.Count(got, "\n") != 1 || strings.Contains(got, "\r") {
				t.Fatalf("log event was not a single line: %q", got)
			}
		})
	}
}

func captureLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})
	return &buf
}
