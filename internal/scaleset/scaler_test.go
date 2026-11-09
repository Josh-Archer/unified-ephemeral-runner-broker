package scaleset

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDemandScalerObserveDoesNotAllocate(t *testing.T) {
	var allocated int
	var decisions []Decision
	scaler, err := NewDemandScaler(DemandConfig{
		Pool:         "full",
		ScaleSetName: "uecb-shared",
		Mode:         ModeObserve,
		Inventory: func(context.Context) (int, int, error) {
			return 1, 0, nil
		},
		Allocate: func(context.Context) error {
			allocated++
			return nil
		},
		Acquire: func(context.Context, []int64) error {
			t.Fatal("observe mode acquired a job")
			return nil
		},
		OnDecision: func(d Decision) { decisions = append(decisions, d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := &Message{
		MessageID:  3,
		Statistics: &Statistic{TotalAssignedJobs: 4},
		JobAvailableMessages: []*JobAvailable{{
			JobMessageBase: JobMessageBase{MessageType: MessageTypeJobAvailable, RunnerRequestID: 9},
		}},
	}
	if err := scaler.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if allocated != 0 {
		t.Fatalf("allocated=%d", allocated)
	}
	if len(decisions) != 1 || decisions[0].Delta != 3 || decisions[0].Desired != 4 || decisions[0].Running != 1 {
		t.Fatalf("decision=%+v", decisions)
	}
}

func TestDemandScalerAllocatesDeficitAndIsIdempotent(t *testing.T) {
	running := 1
	allocating := 1
	var allocated int
	scaler, err := NewDemandScaler(DemandConfig{
		Pool: "full",
		Mode: ModeAllocate,
		Inventory: func(context.Context) (int, int, error) {
			return running, allocating, nil
		},
		Allocate: func(context.Context) error {
			allocated++
			allocating++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := &Message{MessageID: 1, Statistics: &Statistic{TotalAssignedJobs: 5}}
	if err := scaler.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	// 5 assigned - 1 running - 1 allocating = 3.
	if allocated != 3 {
		t.Fatalf("allocated=%d, want 3", allocated)
	}
	if err := scaler.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if allocated != 3 {
		t.Fatalf("replay allocated=%d, want 3", allocated)
	}
}

func TestDemandScalerCapacityAcksAndRetriesOnEmptyPoll(t *testing.T) {
	var allocated int
	scaler, err := NewDemandScaler(DemandConfig{
		Pool: "full",
		Mode: ModeAllocate,
		Inventory: func(context.Context) (int, int, error) {
			return allocated, 0, nil
		},
		Allocate: func(context.Context) error {
			if allocated >= 1 {
				return ErrCapacity
			}
			allocated++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := &Message{MessageID: 2, Statistics: &Statistic{TotalAssignedJobs: 3}}
	if err := scaler.Scale(context.Background(), msg); err != nil {
		t.Fatalf("capacity should ack, err=%v", err)
	}
	if allocated != 1 {
		t.Fatalf("allocated=%d", allocated)
	}
	// Empty poll reapplies the cached statistics and places another runner
	// once capacity is available. The second call still hits the limit.
	if err := scaler.Scale(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if allocated != 1 {
		t.Fatalf("empty poll allocated=%d", allocated)
	}
}

func TestDemandScalerHardErrorDoesNotCountAsSuccess(t *testing.T) {
	var decisions []Decision
	scaler, err := NewDemandScaler(DemandConfig{
		Pool: "full",
		Mode: ModeAllocate,
		Inventory: func(context.Context) (int, int, error) {
			return 0, 0, nil
		},
		Allocate: func(context.Context) error {
			return errors.New("store down")
		},
		OnDecision: func(d Decision) { decisions = append(decisions, d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = scaler.Scale(context.Background(), &Message{MessageID: 1, Statistics: &Statistic{TotalAssignedJobs: 1}})
	if err == nil || err.Error() != "store down" {
		t.Fatalf("err=%v", err)
	}
	if len(decisions) != 1 || decisions[0].Allocated != 0 {
		t.Fatalf("decisions=%+v", decisions)
	}
}

func TestDemandScalerAcquireOnlyInAllocateMode(t *testing.T) {
	var got []int64
	scaler, err := NewDemandScaler(DemandConfig{
		Pool: "full",
		Mode: ModeAllocate,
		Inventory: func(context.Context) (int, int, error) {
			return 1, 0, nil
		},
		Allocate: func(context.Context) error { return nil },
		Acquire: func(_ context.Context, ids []int64) error {
			got = append(got, ids...)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := &Message{
		MessageID:  1,
		Statistics: &Statistic{TotalAssignedJobs: 1},
		JobAvailableMessages: []*JobAvailable{{
			JobMessageBase: JobMessageBase{RunnerRequestID: 44},
		}},
	}
	if err := scaler.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 44 {
		t.Fatalf("acquired=%v", got)
	}
	// Desired is already covered, so acquire still happens before the count check.
}

func TestDemandScalerRejectsBadConfig(t *testing.T) {
	if _, err := NewDemandScaler(DemandConfig{Mode: ModeAllocate}); err == nil {
		t.Fatal("expected pool and callbacks")
	}
	if _, err := NewDemandScaler(DemandConfig{Pool: "full", Mode: "watch", Inventory: func(context.Context) (int, int, error) {
		return 0, 0, nil
	}}); err == nil {
		t.Fatal("expected bad mode")
	}
}

func TestRunStopsSiblingsOnError(t *testing.T) {
	good := &fakeClient{session: testSession(0), blockAfter: true}
	bad := &fakeClient{session: testSession(0), getErr: errors.New("boom")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Run(ctx, []Target{
		{ScaleSetID: 1, MaxRunners: 1, Client: good, Scaler: &scriptScaler{}},
		{ScaleSetID: 2, MaxRunners: 1, Client: bad, Scaler: &scriptScaler{}},
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v", err)
	}
}
