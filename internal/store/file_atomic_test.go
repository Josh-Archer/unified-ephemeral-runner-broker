package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// home#4206: a crash mid-write left allocations.json full of NUL bytes. A
// failed persist must leave the previous file intact and parseable, and a
// successful one must leave no temp file behind.
func TestFileStorePersistIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allocations.json")
	s, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if err := s.Save(model.AllocationStatus{ID: "a1", State: model.StateReady}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left after successful persist: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Make the temp write fail: the target must keep its old contents.
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := s.Save(model.AllocationStatus{ID: "a2", State: model.StateReady}); err == nil {
		t.Fatal("Save succeeded with an unwritable temp file")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("failed persist changed the state file:\nbefore=%s\nafter=%s", before, after)
	}
	var snap fileSnapshot
	if err := json.Unmarshal(after, &snap); err != nil || len(snap.Allocations) != 1 {
		t.Fatalf("state file not intact: err=%v allocations=%d", err, len(snap.Allocations))
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatalf("rm: %v", err)
	}

	if err := writeFileAtomic(path, []byte(`{"allocations":{}}`), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v, want 0600", info.Mode().Perm(), err)
	}
}

// home#4206: NewFromConfig returned NewFile's (*File)(nil) as a non-nil Store
// on error, so callers' nil checks passed and the broker panicked.
func TestNewFromConfigFileErrorReturnsUntypedNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allocations.json")
	if err := os.WriteFile(path, make([]byte, 64), 0o600); err != nil { // NUL bytes
		t.Fatalf("write: %v", err)
	}
	s, err := NewFromConfig(model.StateStoreConfig{Type: TypeFile, Path: path})
	if err == nil {
		t.Fatal("expected decode error for a NUL-filled state file")
	}
	if s != nil {
		t.Fatalf("store = %#v, want untyped nil", s)
	}
	if !IsNil((*File)(nil)) || !IsNil(nil) || IsNil(NewMemory()) {
		t.Fatal("IsNil misclassifies stores")
	}
}
