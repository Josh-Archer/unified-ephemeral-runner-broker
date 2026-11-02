package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

// home#4206: a corrupt file store made NewService run on a typed-nil *File
// and panic at startup. It must report the error instead.
func TestNewServiceCorruptFileStoreReportsErrorWithoutPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allocations.json")
	if err := os.WriteFile(path, make([]byte, 128), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := config.Default()
	cfg.Broker.StateStore.Type = store.TypeFile
	cfg.Broker.StateStore.Path = path
	service := NewService(cfg, nil, nil)
	if service.initErr == nil {
		t.Fatal("corrupt state file must surface as an init error")
	}
	if store.IsNil(service.store) {
		t.Fatal("service kept a nil store")
	}
}

func TestNewServiceWithStoreRejectsTypedNil(t *testing.T) {
	var typedNil *store.File
	service := NewServiceWithStore(config.Default(), nil, nil, typedNil)
	if service.initErr == nil {
		t.Fatal("typed-nil store must be rejected with an error")
	}
	if store.IsNil(service.store) {
		t.Fatal("service kept a typed-nil store")
	}
}
