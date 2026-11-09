package scaleset

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
)

// Client is the message-session surface the listener needs. It matches
// actions/scaleset/listener.Client so a later adapter can wrap that module.
type Client interface {
	GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*Message, error)
	DeleteMessage(ctx context.Context, messageID int) error
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	Session() Session
}

// Config is the listener's scale set and capacity.
type Config struct {
	ScaleSetID int
	MaxRunners int
}

// Validate returns an error if the configuration cannot listen.
func (c Config) Validate() error {
	if c.ScaleSetID == 0 {
		return errors.New("scaleSetID is required")
	}
	if c.MaxRunners < 0 || c.MaxRunners > math.MaxInt32 {
		return errors.New("maxRunners must be between 0 and MaxInt32")
	}
	return nil
}

// InitialMessageID is the synthetic message passed to Scale before polling.
// The service numbers real messages from 0, so this value cannot collide.
const InitialMessageID = -1

// Scaler handles one poll. Scale is never called concurrently.
//
// Scale must tolerate a nil message (the long poll timed out), the initial
// message (MessageID InitialMessageID, session statistics, no jobs), and
// seeing the same message more than once. The message is acknowledged only
// after Scale returns nil. An error leaves it unacked so the next session
// replays it. ctx is the Run context; the acknowledgement is not, so a
// shutdown during a successful Scale still deletes the message.
type Scaler interface {
	Scale(ctx context.Context, message *Message) error
}

// Listener long-polls one scale-set message session.
type Listener struct {
	client     Client
	scaleSetID int
	maxRunners atomic.Uint32
}

// New returns a listener. client is required.
func New(client Client, config Config) (*Listener, error) {
	if client == nil {
		return nil, errors.New("client is required")
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	listener := &Listener{client: client, scaleSetID: config.ScaleSetID}
	listener.SetMaxRunners(config.MaxRunners)
	return listener, nil
}

// SetMaxRunners updates the capacity reported on the next poll.
func (l *Listener) SetMaxRunners(count int) {
	if count < 0 {
		count = 0
	}
	l.maxRunners.Store(uint32(count))
}

// ScaleSetID returns the configured scale set id.
func (l *Listener) ScaleSetID() int { return l.scaleSetID }

// Run polls until ctx is cancelled or a poll, Scale, or ack fails.
func (l *Listener) Run(ctx context.Context, scaler Scaler) error {
	if scaler == nil {
		return errors.New("scaler is required")
	}
	session := l.client.Session()
	if session.SessionID == "" {
		return errors.New("initial session is nil")
	}
	if session.Statistics == nil {
		return errors.New("session statistics is nil")
	}
	if err := scaler.Scale(ctx, &Message{
		MessageID:  InitialMessageID,
		Statistics: session.Statistics,
	}); err != nil {
		return fmt.Errorf("failed to handle initial session statistics: %w", err)
	}

	var lastMessageID int
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := l.client.GetMessage(ctx, lastMessageID, int(l.maxRunners.Load()))
		if err != nil {
			return fmt.Errorf("failed to get message: %w", err)
		}
		if err := scaler.Scale(ctx, msg); err != nil {
			return fmt.Errorf("failed to scale: %w", err)
		}
		if msg == nil {
			continue
		}
		lastMessageID = msg.MessageID
		// Ack even if ctx is already cancelled. Scale succeeded, so the
		// message must not be replayed as if the work had not happened.
		if err := l.client.DeleteMessage(context.WithoutCancel(ctx), msg.MessageID); err != nil {
			return fmt.Errorf("failed to delete the message %d: %w", msg.MessageID, err)
		}
	}
}

// ErrNotLeader stops a listener without acknowledging a message that was not
// scaled, so the replica that holds the lease can take the session.
var ErrNotLeader = errors.New("scaleset: not leader")

// WithLeader returns a client that refuses to poll when isLeader is false.
// DeleteMessage and AcquireJobs still pass through so an in-flight ack finishes.
// A nil isLeader returns client unchanged.
func WithLeader(client Client, isLeader func(context.Context) (bool, error)) Client {
	if client == nil || isLeader == nil {
		return client
	}
	return leaderClient{next: client, isLeader: isLeader}
}

type leaderClient struct {
	next     Client
	isLeader func(context.Context) (bool, error)
}

func (c leaderClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*Message, error) {
	ok, err := c.isLeader(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotLeader
	}
	return c.next.GetMessage(ctx, lastMessageID, maxCapacity)
}

func (c leaderClient) DeleteMessage(ctx context.Context, messageID int) error {
	return c.next.DeleteMessage(ctx, messageID)
}

func (c leaderClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	return c.next.AcquireJobs(ctx, requestIDs)
}

func (c leaderClient) Session() Session { return c.next.Session() }
