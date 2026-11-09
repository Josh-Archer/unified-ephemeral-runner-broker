package scaleset

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClient struct {
	session      Session
	msgs         []*Message
	pos          int
	gets         []struct{ last, max int }
	deleted      []int
	deleteCtxErr error
	getErr       error
	deleteErr    error
	acquired     [][]int64
	blockAfter   bool
}

func (f *fakeClient) Session() Session { return f.session }

func (f *fakeClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*Message, error) {
	f.gets = append(f.gets, struct{ last, max int }{lastMessageID, maxCapacity})
	if f.getErr != nil {
		err := f.getErr
		f.getErr = nil
		return nil, err
	}
	if f.pos >= len(f.msgs) {
		if !f.blockAfter {
			return nil, errors.New("no more messages")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	msg := f.msgs[f.pos]
	f.pos++
	return msg, nil
}

func (f *fakeClient) DeleteMessage(ctx context.Context, messageID int) error {
	if ctx.Err() != nil {
		f.deleteCtxErr = ctx.Err()
	}
	f.deleted = append(f.deleted, messageID)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return nil
}

func (f *fakeClient) AcquireJobs(context.Context, []int64) ([]int64, error) {
	return nil, nil
}

type scriptScaler struct {
	seen   []*Message
	failOn int
	cancel context.CancelFunc
	calls  int
}

func (s *scriptScaler) Scale(ctx context.Context, msg *Message) error {
	s.calls++
	s.seen = append(s.seen, msg)
	if s.cancel != nil && msg != nil && msg.MessageID != InitialMessageID {
		s.cancel()
	}
	if s.failOn > 0 && s.calls == s.failOn {
		return errors.New("scale failed")
	}
	return nil
}

func testSession(assigned int) Session {
	return Session{
		SessionID:               "sess-1",
		MessageQueueURL:         "https://example.test/queue",
		MessageQueueAccessToken: "token",
		Statistics:              &Statistic{TotalAssignedJobs: assigned},
	}
}

func TestListenerScalesInitialStatisticsBeforePolling(t *testing.T) {
	client := &fakeClient{session: testSession(3), blockAfter: true}
	listener, err := New(client, Config{ScaleSetID: 9, MaxRunners: 4})
	if err != nil {
		t.Fatal(err)
	}
	scaler := &scriptScaler{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, scaler) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(scaler.seen) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if len(scaler.seen) == 0 || scaler.seen[0].MessageID != InitialMessageID || scaler.seen[0].Statistics.TotalAssignedJobs != 3 {
		t.Fatalf("initial message = %+v", scaler.seen)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("initial message was acked: %v", client.deleted)
	}
}

func TestListenerAcksOnlyAfterScaleSucceeds(t *testing.T) {
	client := &fakeClient{
		session: testSession(0),
		msgs: []*Message{
			{MessageID: 7, Statistics: &Statistic{TotalAssignedJobs: 2}},
			{MessageID: 8, Statistics: &Statistic{TotalAssignedJobs: 2}},
		},
	}
	listener, err := New(client, Config{ScaleSetID: 9, MaxRunners: 4})
	if err != nil {
		t.Fatal(err)
	}
	scaler := &scriptScaler{}
	err = listener.Run(context.Background(), scaler)
	if err == nil || err.Error() != "failed to get message: no more messages" {
		t.Fatalf("err=%v", err)
	}
	if len(client.deleted) != 2 || client.deleted[0] != 7 || client.deleted[1] != 8 {
		t.Fatalf("deleted=%v", client.deleted)
	}
	if client.gets[0].last != 0 || client.gets[1].last != 7 || client.gets[0].max != 4 {
		t.Fatalf("gets=%v", client.gets)
	}
}

func TestListenerDoesNotAckWhenScaleFails(t *testing.T) {
	client := &fakeClient{
		session: testSession(1),
		msgs:    []*Message{{MessageID: 4, Statistics: &Statistic{TotalAssignedJobs: 1}}},
	}
	listener, err := New(client, Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	// call 1 is the initial message, call 2 is the real message.
	scaler := &scriptScaler{failOn: 2}
	err = listener.Run(context.Background(), scaler)
	if err == nil || err.Error() != "failed to scale: scale failed" {
		t.Fatalf("err=%v", err)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("message acked after scale error: %v", client.deleted)
	}
}

func TestListenerEmptyPollIsNotAcked(t *testing.T) {
	client := &fakeClient{
		session: testSession(0),
		msgs:    []*Message{nil, {MessageID: 1, Statistics: &Statistic{TotalAssignedJobs: 0}}},
	}
	listener, err := New(client, Config{ScaleSetID: 1, MaxRunners: 2})
	if err != nil {
		t.Fatal(err)
	}
	err = listener.Run(context.Background(), &scriptScaler{})
	if err == nil {
		t.Fatal("expected the script to end")
	}
	if len(client.deleted) != 1 || client.deleted[0] != 1 {
		t.Fatalf("deleted=%v, want only message 1", client.deleted)
	}
}

func TestListenerAckSurvivesCallerCancel(t *testing.T) {
	client := &fakeClient{
		session:    testSession(0),
		msgs:       []*Message{{MessageID: 5, Statistics: &Statistic{TotalAssignedJobs: 1}}},
		blockAfter: true,
	}
	listener, err := New(client, Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	scaler := &scriptScaler{cancel: cancel}
	err = listener.Run(ctx, scaler)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != 5 {
		t.Fatalf("deleted=%v", client.deleted)
	}
	if client.deleteCtxErr != nil {
		t.Fatalf("ack observed a cancelled context: %v", client.deleteCtxErr)
	}
}

func TestListenerRejectsIncompleteConfig(t *testing.T) {
	if _, err := New(nil, Config{ScaleSetID: 1}); err == nil {
		t.Fatal("nil client")
	}
	if _, err := New(&fakeClient{}, Config{}); err == nil {
		t.Fatal("missing scale set id")
	}
	listener, err := New(&fakeClient{}, Config{ScaleSetID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Run(context.Background(), nil); err == nil {
		t.Fatal("nil scaler")
	}
	if err := listener.Run(context.Background(), &scriptScaler{}); err == nil || err.Error() != "initial session is nil" {
		t.Fatalf("err=%v", err)
	}
	listener, err = New(&fakeClient{session: Session{SessionID: "s"}}, Config{ScaleSetID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Run(context.Background(), &scriptScaler{}); err == nil || err.Error() != "session statistics is nil" {
		t.Fatalf("err=%v", err)
	}
}

func TestGetMessageErrorDoesNotAck(t *testing.T) {
	client := &fakeClient{session: testSession(0), getErr: errors.New("boom")}
	listener, err := New(client, Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = listener.Run(context.Background(), &scriptScaler{})
	if err == nil || err.Error() != "failed to get message: boom" {
		t.Fatalf("err=%v", err)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("deleted=%v", client.deleted)
	}
}

func TestWithLeaderSkipsPollWhenNotLeader(t *testing.T) {
	client := &fakeClient{session: testSession(0), blockAfter: true}
	wrapped := WithLeader(client, func(context.Context) (bool, error) { return false, nil })
	listener, err := New(wrapped, Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = listener.Run(context.Background(), &scriptScaler{})
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("err=%v", err)
	}
	if len(client.gets) != 0 {
		t.Fatalf("polled while not leader: %+v", client.gets)
	}
	if WithLeader(client, nil) != client {
		t.Fatal("nil leader check should return the client")
	}
}
