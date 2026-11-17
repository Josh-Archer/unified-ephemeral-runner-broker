// Package vmlifecycle plans conservative disposable-VM lifecycle transitions.
// It performs no I/O and is not wired into the broker. A decision is neither
// authorization nor a reservation. See docs/local-vm-lifecycle.md before reuse.
package vmlifecycle

import (
	"errors"
	"math"
	"regexp"
	"time"
)

type Phase string

const (
	Reserved       Phase = "reserved"
	LaunchPending  Phase = "launch-pending"
	Running        Phase = "running"
	CleanupPending Phase = "cleanup-pending"
	Released       Phase = "released"
)

type Action string

const (
	NoAction Action = "none"
	Launch   Action = "launch"
	Inspect  Action = "inspect"
	Destroy  Action = "destroy"
	Revoke   Action = "revoke"
)

// Key must be assigned by the durable controller, not by PR metadata. Fence is
// monotonic ownership generation, never a substitute for provider enforcement.
type Key struct {
	Allocation string
	Fence      uint64
}
type Record struct {
	// Revision is a durable action-claim version, not a provider fencing token.
	Revision   uint64
	Key        Key
	Phase      Phase
	ExpiresAt  time.Time
	ResourceID string
}
type Presence string

const (
	Unobserved   Presence = "unobserved"
	Present      Presence = "present"
	Absent       Presence = "absent"
	FencedAbsent Presence = "fenced-absent"
	Unknown      Presence = "unknown"
)

// Observation must come from an authenticated authoritative provider lookup for
// exactly Key. A cached inventory, request timeout, or empty API page is not
// evidence of absence. Multiple matches must be represented as Unknown.
// FencedAbsent additionally attests that this generation is permanently revoked,
// all in-flight launch operations are quiescent, and no resource remains. Mere
// absence can never release capacity while a delayed launch may still complete.
type Observation struct {
	Key        Key
	Presence   Presence
	ResourceID string
}

// Decision.Next must be atomically persisted with a compare-and-swap on the
// complete previous record BEFORE executing Action. Revision changes for every
// action so only one claimant from the same snapshot can win. This does not
// serialize later reconciliations: Inspect, Revoke and Destroy must be idempotent
// and provider-fenced even when repeated or overlapping.
// Target is populated only for destruction of a verified owned resource.
type Decision struct {
	Next   Record
	Action Action
	Target string
}

var ErrInvalid = errors.New("invalid VM lifecycle input")
var ErrRevision = errors.New("VM lifecycle action revision exhausted")
var ErrEvidence = errors.New("VM lifecycle evidence does not match ownership")
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func (r Record) HoldsCapacity() bool { return r.Phase != Released }

// Plan never retries Launch after it has been durably requested. After an
// uncertain launch, fenced absence releases the failed allocation rather
// than creating a second VM. A new allocation requires fresh admission.
// Cancel or expiry requests cleanup; capacity stays held until confirmed fenced-absent.
func Plan(r Record, o Observation, now time.Time, cancel bool) (Decision, error) {
	fail := func(err error) (Decision, error) { return Decision{}, err }
	if !idPattern.MatchString(r.Key.Allocation) || r.Key.Fence == 0 || now.IsZero() || r.ExpiresAt.IsZero() {
		return fail(ErrInvalid)
	}
	switch r.Phase {
	case Reserved, LaunchPending, Running, CleanupPending, Released:
	default:
		return fail(ErrInvalid)
	}
	if (r.Phase == Reserved || r.Phase == Released) && r.ResourceID != "" {
		return fail(ErrInvalid)
	}
	if r.Phase == Running && r.ResourceID == "" {
		return fail(ErrInvalid)
	}
	if o.Presence == "" {
		o.Presence = Unobserved
	}
	switch o.Presence {
	case Unobserved, Unknown, Present, Absent, FencedAbsent:
	default:
		return fail(ErrInvalid)
	}
	if o.Presence == Unobserved {
		if o.Key != (Key{}) || o.ResourceID != "" {
			return fail(ErrEvidence)
		}
	} else {
		if o.Key != r.Key {
			return fail(ErrEvidence)
		}
		if (o.Presence == Present) != (o.ResourceID != "") {
			return fail(ErrEvidence)
		}
		if o.Presence == Present && r.ResourceID != "" && o.ResourceID != r.ResourceID {
			return fail(ErrEvidence)
		}
	}
	d := Decision{Next: r, Action: NoAction}
	if r.Phase == Released {
		// A later resource means release evidence was insufficient or the provider
		// violated fencing. Do not silently forget the orphan or destroy it here.
		if o.Presence == Present || o.Presence == Unknown {
			return fail(ErrEvidence)
		}
		return d, nil
	}
	stopping := cancel || !now.Before(r.ExpiresAt) || r.Phase == CleanupPending
	if r.Phase == Reserved && !stopping {
		// A reservation must be clean before the first launch request. Evidence of
		// an existing or uncertain resource must be reconciled, never overwritten.
		if o.Presence != Unobserved && o.Presence != Absent {
			return fail(ErrEvidence)
		}
		d.Next.Phase = LaunchPending
		d.Action = Launch
		return claim(d)
	}
	if stopping {
		d.Next.Phase = CleanupPending
	}
	switch o.Presence {
	case Unobserved, Unknown:
		d.Action = Inspect
	case Absent:
		d.Next.Phase = CleanupPending
		d.Action = Revoke
	case FencedAbsent:
		d.Next.Phase = Released
		d.Next.ResourceID = ""
	case Present:
		d.Next.ResourceID = o.ResourceID
		if stopping {
			d.Action = Destroy
			d.Target = o.ResourceID
		} else {
			d.Next.Phase = Running
		}
	}
	return claim(d)
}

func claim(d Decision) (Decision, error) {
	if d.Action != NoAction {
		if d.Next.Revision == math.MaxUint64 {
			return Decision{}, ErrRevision
		}
		d.Next.Revision++
	}
	return d, nil
}
