package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/check"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func add(t *testing.T, st *Store, id string, at time.Time, ok bool, latency time.Duration) {
	t.Helper()
	if err := st.AddResult(context.Background(), check.Result{CheckID: id, At: at, OK: ok, Latency: latency}); err != nil {
		t.Fatal(err)
	}
}

func TestUptimeAndLastResult(t *testing.T) {
	st, ctx := open(t), context.Background()
	now := time.Now()
	ratio, probes, err := st.Uptime(ctx, "web", now.Add(-time.Hour))
	if err != nil || ratio != -1 || probes != 0 {
		t.Fatalf("empty uptime = %v, %d, %v", ratio, probes, err)
	}
	for i := range 4 {
		add(t, st, "web", now.Add(-time.Duration(i)*time.Minute), i != 1, 40*time.Millisecond)
	}
	add(t, st, "other", now, false, 0)
	add(t, st, "web", now.Add(-48*time.Hour), false, 0) // outside the window

	ratio, probes, _ = st.Uptime(ctx, "web", now.Add(-time.Hour))
	if ratio != 0.75 || probes != 4 {
		t.Errorf("uptime = %v over %d probes", ratio, probes)
	}
	last, err := st.LastResult(ctx, "web")
	if err != nil || last == nil || !last.OK || last.Latency != 40*time.Millisecond {
		t.Errorf("last = %+v, %v", last, err)
	}
	if none, _ := st.LastResult(ctx, "missing"); none != nil {
		t.Error("expected no result")
	}
}

func TestIncidents(t *testing.T) {
	st, ctx := open(t), context.Background()
	start := time.Now().Add(-10 * time.Minute)
	if err := st.OpenIncident(ctx, "api", start, "timeout"); err != nil {
		t.Fatal(err)
	}
	ongoing, err := st.OngoingIncident(ctx, "api")
	if err != nil || ongoing == nil || !ongoing.Ongoing() || ongoing.Cause != "timeout" {
		t.Fatalf("ongoing = %+v, %v", ongoing, err)
	}
	resolved, err := st.ResolveIncident(ctx, "api", start.Add(4*time.Minute))
	if err != nil || resolved == nil || resolved.Duration(time.Now()) != 4*time.Minute {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	if again, _ := st.ResolveIncident(ctx, "api", time.Now()); again != nil {
		t.Error("nothing left to resolve")
	}
	recent, err := st.RecentIncidents(ctx, start.Add(-time.Hour), 10)
	if err != nil || len(recent) != 1 || recent[0].Ongoing() {
		t.Errorf("recent = %+v, %v", recent, err)
	}
}

func TestDaysAndLatency(t *testing.T) {
	st, ctx := open(t), context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	add(t, st, "web", now.Add(-time.Hour), true, 100*time.Millisecond)
	add(t, st, "web", now.Add(-2*time.Hour), false, 0)
	add(t, st, "web", now.Add(-26*time.Hour), true, 300*time.Millisecond)

	days, err := st.Days(ctx, "web", 3, now)
	if err != nil || len(days) != 3 {
		t.Fatalf("days = %+v, %v", days, err)
	}
	if days[0].Uptime != -1 || days[1].Uptime != 1 || days[2].Uptime != 0.5 || days[2].Probes != 2 {
		t.Errorf("days = %+v", days)
	}
	if !days[2].Date.Equal(time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last day = %v", days[2].Date)
	}

	latency, err := st.Latency(ctx, "web", now.Add(-48*time.Hour), now, 4)
	if err != nil {
		t.Fatal(err)
	}
	// Buckets of 12h from 48h ago: the probe 26h ago is in the second, the one 1h ago
	// in the fourth; the failed probe does not count.
	want := []time.Duration{-1, 300 * time.Millisecond, -1, 100 * time.Millisecond}
	for i := range want {
		if latency[i] != want[i] {
			t.Errorf("latency = %v, want %v", latency, want)
			break
		}
	}
}

func TestPrune(t *testing.T) {
	st, ctx := open(t), context.Background()
	now := time.Now()
	add(t, st, "web", now.Add(-100*24*time.Hour), true, 0)
	add(t, st, "web", now, true, 0)
	_ = st.OpenIncident(ctx, "web", now.Add(-100*24*time.Hour), "old")
	_, _ = st.ResolveIncident(ctx, "web", now.Add(-99*24*time.Hour))
	removed, err := st.Prune(ctx, now.Add(-90*24*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("removed = %d, %v", removed, err)
	}
	if recent, _ := st.RecentIncidents(ctx, time.Time{}, 10); len(recent) != 0 {
		t.Errorf("old incident kept: %+v", recent)
	}
}
