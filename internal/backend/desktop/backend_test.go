package desktop

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func desktopConfig(maxRunners int, address string, checkPort int) model.BrokerConfig {
	return model.BrokerConfig{
		Pools: []model.PoolConfig{
			{
				Name: model.PoolLite,
				Backends: map[model.BackendName]model.BackendConfig{
					model.BackendDesktop: {
						Enabled:    true,
						Healthy:    true,
						MaxRunners: maxRunners,
						Desktop: &model.DesktopConfig{
							Address:   address,
							CheckPort: checkPort,
						},
					},
				},
			},
		},
	}
}

type stubConn struct{}

func (stubConn) Read([]byte) (int, error)         { return 0, errors.New("unused") }
func (stubConn) Write([]byte) (int, error)        { return 0, errors.New("unused") }
func (stubConn) Close() error                     { return nil }
func (stubConn) LocalAddr() net.Addr              { return nil }
func (stubConn) RemoteAddr() net.Addr             { return nil }
func (stubConn) SetDeadline(time.Time) error      { return nil }
func (stubConn) SetReadDeadline(time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(time.Time) error { return nil }

func TestCapacityOnlineReportsFreeScale(t *testing.T) {
	cfg := desktopConfig(2, "desktop.local", 22)
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		if address != "desktop.local:22" {
			t.Fatalf("unexpected probe address %q", address)
		}
		return stubConn{}, nil
	})

	status, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 2 || status.ActiveRunners != 0 {
		t.Fatalf("unexpected capacity %+v", status)
	}
	if free := backend.FreeSlots(status); free != 2 {
		t.Fatalf("expected 2 free slots, got %d", free)
	}
}

func TestCapacityOfflineReportsExhaustion(t *testing.T) {
	cfg := desktopConfig(1, "desktop.local", 22)
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		return nil, errors.New("connection refused")
	})

	status, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 1 || status.ActiveRunners != 1 {
		t.Fatalf("expected exhausted shape max=1 active=1, got %+v", status)
	}
	if free := backend.FreeSlots(status); free != 0 {
		t.Fatalf("expected 0 free slots when host offline, got %d", free)
	}
}

func TestCapacityDefaultsMaxRunnersToOne(t *testing.T) {
	cfg := desktopConfig(0, "", 0)
	status, err := New(cfg).Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 1 || backend.FreeSlots(status) != 1 {
		t.Fatalf("expected default maxRunners=1 free, got %+v free=%d", status, backend.FreeSlots(status))
	}
}

func TestCapacityNoProbeUsesConfiguredScale(t *testing.T) {
	cfg := desktopConfig(3, "", 0)
	status, err := New(cfg).Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 3 || backend.FreeSlots(status) != 3 {
		t.Fatalf("unexpected capacity %+v free=%d", status, backend.FreeSlots(status))
	}
}

func TestProvisionOfflineIsCapacityExhausted(t *testing.T) {
	cfg := desktopConfig(1, "desktop.local", 22)
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		return nil, errors.New("connection refused")
	})

	_, err := b.Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite,
	}, model.AllocationStatus{
		ID:   "desk-1",
		Pool: model.PoolLite,
	})
	if !backend.IsCapacityExhausted(err) {
		t.Fatalf("expected capacity exhausted, got %v", err)
	}
}

func TestProvisionOnlineReturnsLabel(t *testing.T) {
	cfg := desktopConfig(1, "desktop.local", 22)
	cfg.Pools[0].Backends[model.BackendDesktop] = model.BackendConfig{
		Enabled:     true,
		MaxRunners:  1,
		RunnerLabel: "desktop-runner",
		Desktop: &model.DesktopConfig{
			Address:   "desktop.local",
			CheckPort: 22,
		},
	}
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		return stubConn{}, nil
	})

	provisioned, err := b.Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite,
	}, model.AllocationStatus{
		ID:   "desk-2",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if provisioned.RunnerLabel != "desktop-runner" {
		t.Fatalf("unexpected label %q", provisioned.RunnerLabel)
	}
}

func TestCapacityImplementsCapacityBackend(t *testing.T) {
	var _ backend.CapacityBackend = New(desktopConfig(1, "", 0))
}
