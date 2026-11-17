package localvmintent

// Everything in this file is a test-only model. A fake source is not authenticated
// evidence, and a successful comparison is not admission or replay protection.
import (
	"context"
	"errors"
	"testing"
	"time"
)

type evidenceKey struct {
	repository string
	run, job   int64
}
type observation struct {
	binding        Candidate
	observed       time.Time
	state          string
	currentAttempt int64
	checkoutMode   string
	mergeSHA       string
}
type evidenceSource interface {
	resolve(context.Context, evidenceKey) (observation, error)
}
type fakeSource struct {
	value     observation
	err       error
	requested evidenceKey
	calls     int
}

func (f *fakeSource) resolve(ctx context.Context, k evidenceKey) (observation, error) {
	f.requested = k
	f.calls++
	if err := ctx.Err(); err != nil {
		return observation{}, err
	}
	return f.value, f.err
}

var errRelationship = errors.New("test model: evidence relationship rejected")

func compareModel(ctx context.Context, i Intent, src evidenceSource, now time.Time, maxAge time.Duration) error {
	c, err := i.Candidate()
	if err != nil {
		return err
	}
	if src == nil || maxAge <= 0 || now.IsZero() {
		return errRelationship
	}
	o, err := src.resolve(ctx, evidenceKey{c.Repository, c.RunID, c.JobID})
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if o.observed.IsZero() || o.observed.After(now) || now.Sub(o.observed) > maxAge {
		return errRelationship
	}
	if o.state != "queued" || o.currentAttempt != c.Attempt || o.binding != c {
		return errRelationship
	}
	switch o.checkoutMode {
	case "head":
		if c.CheckoutSHA != c.HeadSHA || o.mergeSHA != "" {
			return errRelationship
		}
	case "synthetic-merge":
		if !shaPattern.MatchString(o.mergeSHA) || o.mergeSHA != c.CheckoutSHA || c.CheckoutSHA == c.HeadSHA {
			return errRelationship
		}
	default:
		return errRelationship
	}
	return nil
}
func fixture() (Intent, *fakeSource, time.Time) {
	c := candidate()
	i, _ := New(c)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return i, &fakeSource{value: observation{binding: c, observed: now.Add(-time.Second), state: "queued", currentAttempt: 1, checkoutMode: "synthetic-merge", mergeSHA: c.CheckoutSHA}}, now
}
func TestRelationshipModelRejectsMismatches(t *testing.T) {
	cases := map[string]func(*observation){
		"repository": func(o *observation) { o.binding.Repository = "Other/repo" },
		"run":        func(o *observation) { o.binding.RunID++ }, "job": func(o *observation) { o.binding.JobID++ },
		"attempt": func(o *observation) { o.binding.Attempt++ }, "superseded": func(o *observation) { o.currentAttempt++ },
		"head":              func(o *observation) { o.binding.HeadSHA = o.binding.WorkflowSHA },
		"checkout":          func(o *observation) { o.binding.CheckoutSHA = o.binding.HeadSHA },
		"workflow revision": func(o *observation) { o.binding.WorkflowSHA = o.binding.HeadSHA },
		"workflow path":     func(o *observation) { o.binding.WorkflowPath = ".github/workflows/other.yml" },
		"completed":         func(o *observation) { o.state = "completed" }, "canceled": func(o *observation) { o.state = "canceled" },
		"running": func(o *observation) { o.state = "in_progress" }, "unknown state": func(o *observation) { o.state = "" },
		"stale":              func(o *observation) { o.observed = o.observed.Add(-time.Hour) },
		"future":             func(o *observation) { o.observed = o.observed.Add(time.Hour) },
		"missing timestamp":  func(o *observation) { o.observed = time.Time{} },
		"merge mismatch":     func(o *observation) { o.mergeSHA = o.binding.HeadSHA },
		"missing merge":      func(o *observation) { o.mergeSHA = "" },
		"implicit mode":      func(o *observation) { o.checkoutMode = "" },
		"head mode mismatch": func(o *observation) { o.checkoutMode = "head"; o.mergeSHA = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			i, s, now := fixture()
			mutate(&s.value)
			if compareModel(context.Background(), i, s, now, time.Minute) == nil {
				t.Fatal("accepted mismatch")
			}
		})
	}
}
func TestRelationshipModelSourceBoundaryAndReplayLimitation(t *testing.T) {
	i, s, now := fixture()
	for n := 0; n < 2; n++ {
		if err := compareModel(context.Background(), i, s, now, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately demonstrates that pure comparison cannot consume an admission.
	if s.calls != 2 || s.requested != (evidenceKey{"Josh-Archer/DeskQuest", 1, 2}) {
		t.Fatal("source lookup binding lost")
	}
	s.err = errors.New("source unavailable")
	if compareModel(context.Background(), i, s, now, time.Minute) == nil {
		t.Fatal("source failure ignored")
	}
	s.err = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if compareModel(ctx, i, s, now, time.Minute) == nil {
		t.Fatal("cancellation ignored")
	}
	if compareModel(context.Background(), Intent{}, s, now, time.Minute) == nil {
		t.Fatal("zero intent accepted")
	}
	for _, age := range []time.Duration{0, -1} {
		if compareModel(context.Background(), i, s, now, age) == nil {
			t.Fatal("invalid freshness policy")
		}
	}
}
func TestRelationshipModelExplicitHeadCheckout(t *testing.T) {
	_, s, now := fixture()
	s.value.binding.CheckoutSHA = s.value.binding.HeadSHA
	s.value.checkoutMode = "head"
	s.value.mergeSHA = ""
	i, _ := New(s.value.binding)
	if err := compareModel(context.Background(), i, s, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	s.value.mergeSHA = s.value.binding.HeadSHA
	if compareModel(context.Background(), i, s, now, time.Minute) == nil {
		t.Fatal("ambiguous checkout accepted")
	}
}

// A source can finish successfully while cancellation races with its return.
type cancelingSource struct {
	value  observation
	cancel context.CancelFunc
}

func (s cancelingSource) resolve(context.Context, evidenceKey) (observation, error) {
	s.cancel()
	return s.value, nil
}
func TestRelationshipModelBoundaryAndCancellationAfterLookup(t *testing.T) {
	i, s, now := fixture()
	if compareModel(context.Background(), i, nil, now, time.Minute) == nil {
		t.Fatal("nil source accepted")
	}
	if compareModel(context.Background(), i, s, time.Time{}, time.Minute) == nil {
		t.Fatal("zero clock accepted")
	}
	s.value.observed = now.Add(-time.Minute)
	if err := compareModel(context.Background(), i, s, now, time.Minute); err != nil {
		t.Fatalf("exact age boundary rejected: %v", err)
	}
	s.value.observed = s.value.observed.Add(-time.Nanosecond)
	if compareModel(context.Background(), i, s, now, time.Minute) == nil {
		t.Fatal("expired boundary accepted")
	}
	s.value.observed = now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := compareModel(ctx, i, cancelingSource{s.value, cancel}, now, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-lookup cancellation ignored: %v", err)
	}
}
