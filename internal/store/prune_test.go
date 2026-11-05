package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestEffectiveRetentionFileDefault(t *testing.T) {
	if got := EffectiveRetention(model.StateStoreConfig{Type: TypeFile}); got != DefaultFileRetention {
		t.Fatalf("file default = %s, want %s", got, DefaultFileRetention)
	}
	if got := EffectiveRetention(model.StateStoreConfig{Type: TypeFile, Retention: -time.Second}); got != 0 {
		t.Fatalf("negative should disable, got %s", got)
	}
	if got := EffectiveRetention(model.StateStoreConfig{Type: TypeFile, Retention: time.Hour}); got != time.Hour {
		t.Fatalf("explicit = %s", got)
	}
	if got := EffectiveRetention(model.StateStoreConfig{Type: TypeMemory}); got != 0 {
		t.Fatalf("memory default should be off, got %s", got)
	}
}

func TestFilePruneTerminalRemovesOldFinished(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allocations.json")
	s, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	old := model.AllocationStatus{
		ID:        "old-done",
		State:     model.StateCompleted,
		CreatedAt: now.Add(-48 * time.Hour),
		ExpiresAt: now.Add(-24 * time.Hour),
	}
	fresh := model.AllocationStatus{
		ID:        "fresh-done",
		State:     model.StateFailed,
		CreatedAt: now.Add(-time.Hour),
		ExpiresAt: now,
	}
	live := model.AllocationStatus{
		ID:        "live",
		State:     model.StateReady,
		CreatedAt: now.Add(-72 * time.Hour),
		ExpiresAt: now.Add(time.Hour),
	}
	for _, st := range []model.AllocationStatus{old, fresh, live} {
		if err := s.Save(st); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneTerminal(now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	if _, ok := s.Get("old-done"); ok {
		t.Fatal("old terminal should be gone")
	}
	if _, ok := s.Get("fresh-done"); !ok {
		t.Fatal("fresh terminal should remain")
	}
	if _, ok := s.Get("live"); !ok {
		t.Fatal("live allocation must never prune")
	}
	// Reload from disk to prove persist.
	reloaded, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get("old-done"); ok {
		t.Fatal("prune must persist")
	}
	if len(reloaded.List()) != 2 {
		t.Fatalf("reloaded size = %d, want 2", len(reloaded.List()))
	}
}

func TestMemoryPruneTerminalNoopWhenDisabled(t *testing.T) {
	s := NewMemory()
	now := time.Now()
	_ = s.Save(model.AllocationStatus{ID: "x", State: model.StateCompleted, CreatedAt: now.Add(-time.Hour)})
	n, err := s.PruneTerminal(now, 0)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, ok := s.Get("x"); !ok {
		t.Fatal("disabled prune deleted a record")
	}
}
