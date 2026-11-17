package vmlifecycle

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func fixture() (Record, time.Time) {
	n := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return Record{Key: Key{"allocation-1", 7}, Phase: Reserved, ExpiresAt: n.Add(time.Hour)}, n
}
func obs(r Record, p Presence, id string) Observation {
	return Observation{Key: r.Key, Presence: p, ResourceID: id}
}
func step(t *testing.T, r Record, o Observation, n time.Time, c bool) Decision {
	t.Helper()
	d, e := Plan(r, o, n, c)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestLifecycle(t *testing.T) {
	r, n := fixture()
	d := step(t, r, Observation{}, n, false)
	if d.Action != Launch || d.Next.Phase != LaunchPending {
		t.Fatal(d)
	}
	r = d.Next
	d = step(t, r, Observation{}, n, false)
	if d.Action != Inspect || !d.Next.HoldsCapacity() {
		t.Fatal(d)
	}
	r = step(t, r, obs(r, Present, "vm-1"), n, false).Next
	if r.Phase != Running {
		t.Fatal(r)
	}
	d = step(t, r, obs(r, Present, "vm-1"), n, true)
	if d.Action != Destroy || d.Target != "vm-1" || !d.Next.HoldsCapacity() {
		t.Fatal(d)
	}
	r = d.Next
	d = step(t, r, obs(r, Absent, ""), n, false)
	if d.Action != Revoke || !d.Next.HoldsCapacity() {
		t.Fatal(d)
	}
	r = d.Next
	d = step(t, r, obs(r, FencedAbsent, ""), n, false)
	if d.Next.HoldsCapacity() || d.Next.Phase != Released {
		t.Fatal(d)
	}
	if got := step(t, d.Next, Observation{}, n, true); got.Action != NoAction {
		t.Fatal(got)
	}
}
func TestUncertainLaunchAndExpiryNeverBlindlyRelaunch(t *testing.T) {
	r, n := fixture()
	r = step(t, r, Observation{}, n, false).Next
	for _, p := range []Presence{Unknown, Absent, FencedAbsent} {
		d := step(t, r, obs(r, p, ""), n, false)
		if d.Action == Launch {
			t.Fatal("relaunch", d)
		}
		if p != FencedAbsent && !d.Next.HoldsCapacity() {
			t.Fatal("premature release", d)
		}
	}
	for _, phase := range []Phase{Reserved, LaunchPending, Running, CleanupPending} {
		r.Phase = phase
		r.ResourceID = ""
		if phase == Running {
			r.ResourceID = "vm-1"
		}
		d := step(t, r, Observation{}, r.ExpiresAt, false)
		if d.Next.Phase != CleanupPending || d.Action != Inspect || !d.Next.HoldsCapacity() {
			t.Fatal(d)
		}
	}
}
func TestOwnershipAndInvalidInputsFailClosed(t *testing.T) {
	r, n := fixture()
	r.Phase = Running
	r.ResourceID = "vm-1"
	cases := []Observation{obs(r, Present, "vm-2"), {Key: Key{"other", 7}, Presence: Absent}, {Key: Key{r.Key.Allocation, 6}, Presence: FencedAbsent}, obs(r, Absent, "vm-1"), obs(r, Present, ""), {Presence: Unobserved, ResourceID: "vm-1"}, {Presence: "bogus"}}
	for _, o := range cases {
		if _, e := Plan(r, o, n, false); e == nil {
			t.Fatalf("accepted %#v", o)
		}
	}
	bad := []Record{{}, {Key: r.Key, Phase: "bogus", ExpiresAt: r.ExpiresAt}, {Key: r.Key, Phase: Running, ExpiresAt: r.ExpiresAt}}
	for _, x := range bad {
		if _, e := Plan(x, Observation{}, n, false); !errors.Is(e, ErrInvalid) {
			t.Fatal(x, e)
		}
	}
	released := r
	released.Phase = Released
	released.ResourceID = ""
	if _, e := Plan(released, obs(released, Present, "orphan"), n, false); !errors.Is(e, ErrEvidence) {
		t.Fatal(e)
	}
}
func TestReservedResourceCannotBeOverwritten(t *testing.T) {
	r, n := fixture()
	for _, p := range []Presence{Present, Unknown, FencedAbsent} {
		o := obs(r, p, "")
		if p == Present {
			o.ResourceID = "vm-1"
		}
		if _, e := Plan(r, o, n, false); e == nil {
			t.Fatal(p)
		}
	}
}

// This fake demonstrates the required orchestration order, not durable storage
// or actual provider fencing. No provider implementation is shipped.
type fakeStore struct {
	mu sync.Mutex
	r  Record
}

func (s *fakeStore) cas(old, next Record) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !reflect.DeepEqual(s.r, old) {
		return false
	}
	s.r = next
	return true
}
func TestCASWinnerOnlyRequestsLaunchAndRestartInspects(t *testing.T) {
	r, n := fixture()
	s := fakeStore{r: r}
	a := step(t, r, Observation{}, n, false)
	b := a
	var wg sync.WaitGroup
	launches := make(chan bool, 2)
	for _, d := range []Decision{a, b} {
		wg.Add(1)
		go func(d Decision) {
			defer wg.Done()
			if s.cas(r, d.Next) {
				launches <- true
			}
		}(d)
	}
	wg.Wait()
	close(launches)
	count := 0
	for range launches {
		count++
	}
	if count != 1 {
		t.Fatal(count)
	}
	// Crash after the persist but before dispatch: no launch is retried. A real
	// controller must inspect/revoke and require new admission for another lease.
	d := step(t, s.r, Observation{}, n, false)
	if d.Action != Inspect {
		t.Fatal(d)
	}
}
func FuzzPlanInvariants(f *testing.F) {
	f.Add(uint8(0), uint8(0), false)
	f.Fuzz(func(t *testing.T, phase, presence uint8, cancel bool) {
		r, n := fixture()
		phases := []Phase{Reserved, LaunchPending, Running, CleanupPending, Released}
		ps := []Presence{Unobserved, Present, Absent, FencedAbsent, Unknown}
		r.Phase = phases[int(phase)%len(phases)]
		if r.Phase == Running {
			r.ResourceID = "vm-1"
		}
		p := ps[int(presence)%len(ps)]
		o := obs(r, p, "")
		if p == Unobserved {
			o = Observation{}
		}
		if p == Present {
			o.ResourceID = "vm-1"
		}
		d, e := Plan(r, o, n, cancel)
		if e != nil {
			return
		}
		if d.Action == Launch && (r.Phase != Reserved || cancel) {
			t.Fatal("unsafe launch")
		}
		if d.Next.Phase == Released && r.Phase != Released && p != FencedAbsent {
			t.Fatal("unfenced release")
		}
		if d.Action == Destroy && (p != Present || d.Target != o.ResourceID || d.Next.Phase != CleanupPending) {
			t.Fatal("unsafe destroy")
		}
	})
}

func TestRepeatedActionClaimsChangeRevision(t *testing.T) {
	r, n := fixture()
	r.Phase = CleanupPending
	r.ResourceID = "vm-1"
	for _, tc := range []struct {
		presence Presence
		action   Action
	}{{Unknown, Inspect}, {Absent, Revoke}, {Present, Destroy}} {
		t.Run(string(tc.action), func(t *testing.T) {
			o := obs(r, tc.presence, "")
			if tc.presence == Present {
				o.ResourceID = "vm-1"
			}
			d := step(t, r, o, n, false)
			if d.Action != tc.action || d.Next.Revision != r.Revision+1 {
				t.Fatal(d)
			}
			s := fakeStore{r: r}
			if !s.cas(r, d.Next) || s.cas(r, d.Next) {
				t.Fatal("same snapshot claimed twice")
			}
			// A later reconciliation can claim another attempt. It must therefore be
			// idempotent/fenced at the provider; revision is not an execution mutex.
			next := step(t, d.Next, o, n, false)
			if next.Action != tc.action || next.Next.Revision != d.Next.Revision+1 {
				t.Fatal(next)
			}
		})
	}
}
func TestActionRevisionExhaustionFailsClosed(t *testing.T) {
	r, n := fixture()
	r.Revision = ^uint64(0)
	for _, tc := range []struct {
		phase  Phase
		p      Presence
		cancel bool
	}{{Reserved, Unobserved, false}, {LaunchPending, Unknown, false}, {CleanupPending, Absent, false}, {CleanupPending, Present, false}} {
		r.Phase = tc.phase
		o := obs(r, tc.p, "")
		if tc.p == Unobserved {
			o = Observation{}
		}
		if tc.p == Present {
			o.ResourceID = "vm-1"
		}
		if _, e := Plan(r, o, n, tc.cancel); !errors.Is(e, ErrRevision) {
			t.Fatalf("%+v: %v", tc, e)
		}
	}
	// Terminal no-I/O transitions do not consume a new action revision.
	r.Phase = CleanupPending
	d := step(t, r, obs(r, FencedAbsent, ""), n, false)
	if d.Next.Phase != Released || d.Next.Revision != r.Revision {
		t.Fatal(d)
	}
}
