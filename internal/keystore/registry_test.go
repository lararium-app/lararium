package keystore

import (
	"context"
	"testing"
)

func TestRegistrySwapAndCopy(t *testing.T) {
	initial := map[string]string{"a": "1"}
	r := NewRegistry(initial)
	initial["a"] = "mutated" // caller mutating its map must not leak in
	if r.Key("a") != "1" {
		t.Fatalf("NewRegistry must copy: got %q", r.Key("a"))
	}

	snap := r.Snapshot()
	snap["a"] = "clobbered" // Snapshot is shared; mutating it is a caller bug,
	// but Swap must have copied too — verify via fresh registry path:
	r.Swap(map[string]string{"b": "2"})
	if r.Key("a") != "" || r.Key("b") != "2" {
		t.Fatalf("Swap did not replace: a=%q b=%q", r.Key("a"), r.Key("b"))
	}
	src := map[string]string{"c": "3"}
	r.Swap(src)
	src["c"] = "late"
	if r.Key("c") != "3" {
		t.Fatalf("Swap must copy input: got %q", r.Key("c"))
	}
}

func TestContextSnapshot(t *testing.T) {
	snap := map[string]string{"k": "v"}
	ctx := WithSnapshot(context.Background(), snap)
	got, ok := SnapshotFrom(ctx)
	if !ok || got["k"] != "v" {
		t.Fatalf("round-trip: got %v %v", got, ok)
	}
	if _, ok := SnapshotFrom(context.Background()); ok {
		t.Fatal("bare ctx must report ok=false")
	}
}
