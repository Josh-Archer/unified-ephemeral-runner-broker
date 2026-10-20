package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// WebhookDelivery is a durably queued inbound webhook delivery awaiting
// processing by the reconciliation leader.
type WebhookDelivery struct {
	ID         int64
	DeliveryID string
	Payload    []byte
	Attempts   int
	ReceivedAt time.Time
}

// WebhookInbox is a durable, replica-shared queue of webhook deliveries. Any
// replica may enqueue; the leader claims and acknowledges. Claims are leases:
// an unacknowledged delivery becomes claimable again after the lease expires,
// so a crash or failed attempt never loses a delivery.
type WebhookInbox interface {
	// EnqueueWebhook stores a delivery. A repeated deliveryID is a no-op.
	EnqueueWebhook(ctx context.Context, deliveryID string, payload []byte) error
	// ClaimWebhooks leases up to limit unclaimed deliveries, oldest first.
	ClaimWebhooks(ctx context.Context, limit int, lease time.Duration) ([]WebhookDelivery, error)
	// AckWebhook removes a processed delivery.
	AckWebhook(ctx context.Context, id int64) error
}

// AsWebhookInbox returns a WebhookInbox when the store supports a shared one.
func AsWebhookInbox(s Store) WebhookInbox {
	if inbox, ok := s.(WebhookInbox); ok {
		return inbox
	}
	return nil
}

type memoryInboxItem struct {
	WebhookDelivery
	lockedUntil time.Time
}

type memoryInbox struct {
	mu     sync.Mutex
	nextID int64
	items  map[int64]*memoryInboxItem
}

func (m *Memory) EnqueueWebhook(_ context.Context, deliveryID string, payload []byte) error {
	m.inbox.mu.Lock()
	defer m.inbox.mu.Unlock()
	if m.inbox.items == nil {
		m.inbox.items = map[int64]*memoryInboxItem{}
	}
	for _, item := range m.inbox.items {
		if item.DeliveryID == deliveryID {
			return nil
		}
	}
	m.inbox.nextID++
	m.inbox.items[m.inbox.nextID] = &memoryInboxItem{WebhookDelivery: WebhookDelivery{
		ID:         m.inbox.nextID,
		DeliveryID: deliveryID,
		Payload:    append([]byte(nil), payload...),
		ReceivedAt: time.Now(),
	}}
	return nil
}

func (m *Memory) ClaimWebhooks(_ context.Context, limit int, lease time.Duration) ([]WebhookDelivery, error) {
	m.inbox.mu.Lock()
	defer m.inbox.mu.Unlock()
	now := time.Now()
	var out []WebhookDelivery
	for _, item := range m.inbox.items {
		if item.lockedUntil.After(now) {
			continue
		}
		out = append(out, item.WebhookDelivery)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		item := m.inbox.items[out[i].ID]
		item.Attempts++
		item.lockedUntil = now.Add(lease)
		out[i].Attempts = item.Attempts
	}
	return out, nil
}

func (m *Memory) AckWebhook(_ context.Context, id int64) error {
	m.inbox.mu.Lock()
	defer m.inbox.mu.Unlock()
	delete(m.inbox.items, id)
	return nil
}
