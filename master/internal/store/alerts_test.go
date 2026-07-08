package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func strptr(s string) *string { return &s }

// TestAlertMutesStore covers the mute store contract: insert, the
// same-target upsert (dedup), null-aware region matching, active-vs-all
// filtering by expiry, and delete's deleted/no-op signal.
func TestAlertMutesStore(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	future := time.Now().Add(time.Hour)

	// Insert an active mute for NodeDown in eu.
	a, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "NodeDown", Region: strptr("eu"), Note: "flaky link", ExpiresAt: &future, CreatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}
	if a.Region == nil || *a.Region != "eu" || a.Note != "flaky link" || a.CreatedBy != "admin" {
		t.Fatalf("A fields: %+v", a)
	}

	// Upsert the same target (NodeDown, eu) → updates in place, same id.
	a2, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "NodeDown", Region: strptr("eu"), Note: "still flaky", ExpiresAt: nil, CreatedBy: "admin2",
	})
	if err != nil {
		t.Fatalf("upsert A2: %v", err)
	}
	if a2.ID != a.ID {
		t.Fatalf("upsert should reuse the active row: %s != %s", a2.ID, a.ID)
	}
	if a2.Note != "still flaky" || a2.ExpiresAt != nil {
		t.Fatalf("upsert should update note/expires: %+v", a2)
	}

	// A null-region NodeDown mute is a DISTINCT target from the eu one → new row.
	b, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "NodeDown", Region: nil, Note: "all regions", CreatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("insert B: %v", err)
	}
	if b.ID == a.ID || b.Region != nil {
		t.Fatalf("B should be a new null-region row: %+v", b)
	}

	// An already-expired mute: inserted directly (the handler forbids past
	// expiry, but the store does not — it is the filter's job to hide it).
	past := time.Now().Add(-time.Hour)
	c, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "CrashLoop", Region: nil, ExpiresAt: &past, CreatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("insert C: %v", err)
	}

	// Active list excludes the expired C; includeExpired adds it.
	active, err := st.ListAlertMutes(ctx, false)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active want 2 (A,B), got %d: %+v", len(active), active)
	}
	all, err := st.ListAlertMutes(ctx, true)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all want 3 (A,B,C), got %d", len(all))
	}

	// Region matching: eu-scoped A matches only eu; null-region B matches any.
	if !a.Matches("NodeDown", "eu") || a.Matches("NodeDown", "us") || a.Matches("DiskHigh", "eu") {
		t.Fatalf("A.Matches region/name scoping wrong")
	}
	if !b.Matches("NodeDown", "us") || !b.Matches("NodeDown", "eu") {
		t.Fatalf("B (null region) should match any region")
	}

	// Delete signals a real removal, then a no-op for the same id.
	del, deleted, err := st.DeleteAlertMute(ctx, a.ID)
	if err != nil || !deleted || del.ID != a.ID {
		t.Fatalf("delete A: deleted=%v id=%s err=%v", deleted, del.ID, err)
	}
	if _, deleted, err := st.DeleteAlertMute(ctx, a.ID); err != nil || deleted {
		t.Fatalf("delete A again should be a no-op: deleted=%v err=%v", deleted, err)
	}
	// C (expired) is still deletable by id.
	if _, deleted, err := st.DeleteAlertMute(ctx, c.ID); err != nil || !deleted {
		t.Fatalf("delete expired C: deleted=%v err=%v", deleted, err)
	}

	active, _ = st.ListAlertMutes(ctx, false)
	if len(active) != 1 || active[0].ID != b.ID {
		t.Fatalf("after deletes, active should be [B]: %+v", active)
	}
}

// TestAlertMutesEmpty: the list is [] (never nil) when there are no mutes.
func TestAlertMutesEmpty(t *testing.T) {
	st := testdb.New(t)
	mutes, err := st.ListAlertMutes(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if mutes == nil || len(mutes) != 0 {
		t.Fatalf("want [], got %+v", mutes)
	}
}
