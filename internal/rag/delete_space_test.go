package rag

import (
	"context"
	"testing"
)

// TestDeleteSourceInOneSpace: a forget scoped to a space removes the
// source from that space's collection only (D-77), counts only what it
// removed, and a space with no collection holds nothing.
func TestDeleteSourceInOneSpace(t *testing.T) {
	svc, f := spacesService(t)
	ctx := context.Background()
	ingestIn(t, svc, "acme", "manual://shared", "acme's copy")
	ingestIn(t, svc, "globex", "manual://other", "globex's note")
	// The same URI in another space (written before moves existed).
	f.collection("c__globex").points["g"] = fakePoint{ID: "g", Payload: map[string]any{
		"source_uri": "manual://shared", "chunk_index": float64(0), "text": "globex's copy"}}

	n, err := svc.DeleteSourceIn(ctx, "manual://shared", "globex")
	if err != nil || n != 1 {
		t.Fatalf("DeleteSourceIn(globex) = %d, %v", n, err)
	}
	if f.collection("c__globex").texts()["manual://shared"] != nil {
		t.Error("globex still holds the source")
	}
	if f.collection("c__acme").texts()["manual://shared"] == nil {
		t.Error("a forget in globex reached acme's copy")
	}
	if n, err := svc.DeleteSourceIn(ctx, "manual://shared", "personal"); err != nil || n != 0 {
		t.Errorf("a space with no collection = %d, %v", n, err)
	}
	if _, err := svc.DeleteSourceIn(ctx, "", "acme"); err == nil {
		t.Error("an empty URI was accepted")
	}
	if _, err := svc.DeleteSourceIn(ctx, "manual://shared", ""); err == nil {
		t.Error("an empty space was accepted")
	}
	// During a migration the shadow's copy goes too; a space the shadow
	// has no collection for holds nothing there.
	svc.setShadow(svc.active().WithCollection("shadow"))
	ingestIn(t, svc, "acme", "manual://m", "migrating note")
	if n, err := svc.DeleteSourceIn(ctx, "manual://m", "acme"); err != nil || n != 1 {
		t.Errorf("DeleteSourceIn during a migration = %d, %v", n, err)
	}
	if n, err := svc.DeleteSourceIn(ctx, "manual://m", "nowhere"); err != nil || n != 0 {
		t.Errorf("a space with no collection during a migration = %d, %v", n, err)
	}
	broken := NewService(&hookEmbedder{}, NewQdrantStore("http://127.0.0.1:1", "c", ""), 40, 4)
	if _, err := broken.DeleteSourceIn(ctx, "manual://x", "acme"); err == nil {
		t.Error("an unreachable store deleted nothing without an error")
	}
	broken.setShadow(broken.active().WithCollection("shadow"))
	if _, err := broken.DeleteSourceIn(ctx, "manual://x", "acme"); err == nil {
		t.Error("an unreachable store deleted nothing without an error during a migration")
	}
}
