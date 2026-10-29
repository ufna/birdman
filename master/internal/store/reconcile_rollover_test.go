package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func seedRollover(t *testing.T, capacity, maximum int32) (*store.Store, *testdb.Fixture, store.FleetConfig, *store.Version) {
	t.Helper()
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", capacity)
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx, `update versions set state='deprecated',deprecated_at=now() where id=$1::uuid`, f.VersionID); err != nil {
		t.Fatal(err)
	}
	active := f.AddVersion(t, "1.1.0", "dev")
	buffer := int32(1)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{Project: f.Project, Env: f.Env, Region: f.Region, ActiveVersion: &active, BufferReady: &buffer, MaxServers: &maximum}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `update versions set state='active' where id=$1::uuid`, active); err != nil {
		t.Fatal(err)
	}
	fleet := f.FleetConfig(t, f.Env, f.Region)
	deprecated, err := st.DeprecatedWindowVersion(ctx, fleet.ProjectID, fleet.Env)
	if err != nil || deprecated == nil || deprecated.ID != f.VersionID {
		t.Fatalf("deprecated window: %+v err=%v", deprecated, err)
	}
	return st, f, fleet, deprecated
}

func TestPlanFleetActiveWarmPriority(t *testing.T) {
	for _, tc := range []struct {
		name                string
		capacity, maximum   int32
		oldState            string
		pendingDrain, pause bool
		starts, stops       int
	}{
		{"single_slot_rollover", 1, 1, "ready", false, false, 0, 1},
		{"physical_capacity_also_limits_rollover", 1, 2, "ready", false, false, 0, 1},
		{"keep_both_buffers_when_they_fit", 2, 2, "ready", false, false, 1, 0},
		{"allocated_match_keeps_playing", 1, 1, "allocated", false, false, 0, 0},
		{"pending_stop_already_frees_needed_room", 2, 2, "ready", true, false, 0, 0},
		{"paused_active_keeps_working_fallback", 1, 1, "ready", false, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, f, fleet, deprecated := seedRollover(t, tc.capacity, tc.maximum)
			ctx := context.Background()
			old := f.InsertServer(t, f.NodeID, f.VersionID, tc.oldState, 20001, time.Minute)
			if tc.pendingDrain {
				f.InsertServer(t, f.NodeID, f.VersionID, "draining", 20002, time.Minute)
			}
			paused := map[string][]string{}
			if tc.pause {
				paused[*fleet.ActiveVersion] = []string{f.NodeID}
			}
			starts, stops, drains, locked, err := st.PlanFleet(ctx, fleet, deprecated, paused)
			if err != nil || !locked || len(starts) != tc.starts || len(stops) != tc.stops || len(drains) != 0 {
				t.Fatalf("plan: starts=%+v stops=%+v drains=%+v locked=%v err=%v", starts, stops, drains, locked, err)
			}
			wantState := tc.oldState
			if tc.stops == 1 {
				wantState = "draining"
				if stops[0].ServerID != old {
					t.Fatal("stopped a different server")
				}
			}
			server, err := st.GetServer(ctx, old)
			if err != nil || server.State != wantState {
				t.Fatalf("old server: state=%s want=%s err=%v", server.State, wantState, err)
			}
			for _, start := range starts {
				server, err := st.GetServer(ctx, start.ServerID)
				if err != nil || server.VersionID != *fleet.ActiveVersion {
					t.Fatalf("created a deprecated replacement instead of active: %+v err=%v", server, err)
				}
			}
		})
	}
}

func TestPlanFleetSingleSlotRolloverWaitsForTermination(t *testing.T) {
	st, f, fleet, deprecated := seedRollover(t, 1, 1)
	ctx := context.Background()
	old := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, time.Minute)
	starts, stops, _, _, err := st.PlanFleet(ctx, fleet, deprecated, nil)
	if err != nil || len(starts) != 0 || len(stops) != 1 || stops[0].ServerID != old {
		t.Fatalf("rollover must stop only the idle old buffer: starts=%+v stops=%+v err=%v", starts, stops, err)
	}
	// A stop has won the same ready-row lock used by allocation. Nobody may
	// claim that dedicated while its process is still terminating.
	if _, err := st.Allocate(ctx, f.Project, f.Env, f.Region, &f.VersionID, uuid.NewString(), 4, nil); !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("draining buffer was claimable: %v", err)
	}
	starts, stops, _, _, err = st.PlanFleet(ctx, fleet, deprecated, nil)
	if err != nil || len(starts) != 0 || len(stops) != 0 {
		t.Fatalf("repeat oversubscribed capacity or repeated stop: starts=%+v stops=%+v err=%v", starts, stops, err)
	}
	// The agent's terminal report releases actual process capacity; a command
	// receipt alone does not. No maximum or node capacity changes are needed.
	if _, err := st.Pool.Exec(ctx, `update servers set state='reaped',updated_at=now() where id=$1::uuid`, old); err != nil {
		t.Fatal(err)
	}
	starts, stops, _, _, err = st.PlanFleet(ctx, fleet, deprecated, nil)
	if err != nil || len(starts) != 1 || len(stops) != 0 {
		t.Fatalf("active buffer did not converge after termination: starts=%+v stops=%+v err=%v", starts, stops, err)
	}
	created, err := st.GetServer(ctx, starts[0].ServerID)
	if err != nil || created.VersionID != *fleet.ActiveVersion || created.NodeID != f.NodeID {
		t.Fatalf("wrong replacement: %+v err=%v", created, err)
	}
	if _, err := st.Pool.Exec(ctx, `update servers set state='ready' where id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	starts, stops, _, _, err = st.PlanFleet(ctx, fleet, deprecated, nil)
	if err != nil || len(starts) != 0 || len(stops) != 0 {
		t.Fatalf("converged active buffer churned: starts=%+v stops=%+v err=%v", starts, stops, err)
	}
}

func TestPlanFleetRolloverDoesNotStopRacingAllocation(t *testing.T) {
	st, f, fleet, deprecated := seedRollover(t, 1, 1)
	ctx := context.Background()
	old := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, time.Minute)
	claim, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Rollback(ctx)
	match := uuid.NewString()
	// Hold the actual ready→allocated row transition uncommitted, so reconcile
	// sees the old READY snapshot but must skip the allocation's locked row.
	if _, err := claim.Exec(ctx, `update servers set state='allocated',match_id=$2::uuid where id=$1::uuid and state='ready'`, old, match); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	starts, stops, drains, locked, err := st.PlanFleet(deadline, fleet, deprecated, nil)
	if err != nil || !locked || len(starts) != 0 || len(stops) != 0 || len(drains) != 0 {
		t.Fatalf("allocation race produced a stop or blocked: starts=%+v stops=%+v drains=%+v locked=%v err=%v", starts, stops, drains, locked, err)
	}
	if err := claim.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServer(ctx, old)
	if err != nil || server.State != "allocated" || server.MatchID == nil || *server.MatchID != match {
		t.Fatalf("raced live match was lost: %+v err=%v", server, err)
	}
}

func TestPlanFleetRolloverCanReplacePausedNodeBufferElsewhere(t *testing.T) {
	st, f, fleet, deprecated := seedRollover(t, 1, 1)
	ctx := context.Background()
	old := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, time.Minute)
	healthy := f.AddNode(t, "healthy-active-target", "203.0.113.20", 1)
	paused := map[string][]string{*fleet.ActiveVersion: {f.NodeID}}
	starts, stops, _, _, err := st.PlanFleet(ctx, fleet, deprecated, paused)
	if err != nil || len(starts) != 0 || len(stops) != 1 || stops[0].ServerID != old {
		t.Fatalf("idle paused-node buffer still blocks healthy capacity: starts=%+v stops=%+v err=%v", starts, stops, err)
	}
	if _, err := st.Pool.Exec(ctx, `update servers set state='reaped' where id=$1::uuid`, old); err != nil {
		t.Fatal(err)
	}
	starts, stops, _, _, err = st.PlanFleet(ctx, fleet, deprecated, paused)
	if err != nil || len(starts) != 1 || starts[0].NodeID != healthy || len(stops) != 0 {
		t.Fatalf("rollover did not use the healthy node: starts=%+v stops=%+v err=%v", starts, stops, err)
	}
}
