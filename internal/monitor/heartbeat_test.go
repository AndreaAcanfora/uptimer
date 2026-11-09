package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AndreaAcanfora/openheartbeat/ohp"
	"github.com/AndreaAcanfora/uptimer/internal/config"
	"github.com/AndreaAcanfora/uptimer/internal/notify"
	"github.com/AndreaAcanfora/uptimer/internal/store"
)

var backup = config.Check{
	ID: "backup", Name: "Nightly backup", Type: "heartbeat",
	Period: config.Duration(24 * time.Hour), Grace: config.Duration(time.Hour), FailureThreshold: 1,
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func heartbeatMonitor(t *testing.T, st *store.Store, sender Sender) (*Monitor, *clock) {
	t.Helper()
	m := New([]config.Check{backup}, st, sender, nil)
	clk := &clock{time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)}
	m.now = clk.now
	return m, clk
}

func status(m *Monitor) State { return m.States()[0] }

func TestHeartbeatSignals(t *testing.T) {
	ctx := context.Background()
	alerts := &sent{}
	m, clk := heartbeatMonitor(t, openStore(t), alerts)
	if status(m).Status != Unknown {
		t.Fatal("a new heartbeat should be unknown")
	}
	// A run with a start and an exit code 0: up, with the measured duration.
	m.Heartbeat(ctx, "backup", Ping{Signal: ohp.Start, RunID: "r1"})
	clk.advance(90 * time.Second)
	m.Heartbeat(ctx, "backup", Ping{Signal: ohp.ExitCode(0), RunID: "r1"})
	if s := status(m); s.Status != Up || s.Last.Latency != 90*time.Second {
		t.Fatalf("after success: %+v", s)
	}
	// log changes nothing.
	m.Heartbeat(ctx, "backup", Ping{Signal: ohp.Log, Body: "50% done"})
	// A failure with output: down at once, cause from the last line.
	clk.advance(time.Hour)
	m.Heartbeat(ctx, "backup", Ping{Signal: ohp.ExitCode(3), Body: "dumping…\npg_dump: connection refused\n"})
	s := status(m)
	if s.Status != Down || s.Last.Error != "the job exited with status 3: pg_dump: connection refused" {
		t.Fatalf("after failure: %+v", s)
	}
	clk.advance(time.Hour)
	m.Heartbeat(ctx, "backup", Ping{Signal: ohp.Success})
	if got := alerts.kinds(); len(got) != 2 || got[0] != notify.Down || got[1] != notify.Up {
		t.Fatalf("alerts %v", got)
	}
	if alerts.events[1].Downtime != time.Hour {
		t.Errorf("downtime %s", alerts.events[1].Downtime)
	}
	if err := m.Heartbeat(ctx, "nope", Ping{Signal: ohp.Success}); err != ErrNoSuchHeartbeat {
		t.Errorf("unknown check: %v", err)
	}
}

func TestHeartbeatDeadlines(t *testing.T) {
	ctx := context.Background()
	alerts := &sent{}
	m, clk := heartbeatMonitor(t, openStore(t), alerts)
	// Never pinged: no alert, however long it waits.
	clk.advance(72 * time.Hour)
	m.CheckHeartbeats(ctx)
	if len(alerts.kinds()) != 0 || status(m).Status != Unknown {
		t.Fatal("a heartbeat that never pinged must not alert")
	}
	m.Heartbeat(ctx, "backup", Ping{Signal: ohp.Success})
	clk.advance(25 * time.Hour) // period + grace, exactly
	m.CheckHeartbeats(ctx)
	if status(m).Status != Up {
		t.Fatal("down before the grace time ran out")
	}
	clk.advance(time.Minute)
	m.CheckHeartbeats(ctx)
	s := status(m)
	if s.Status != Down || !strings.HasPrefix(s.Last.Error, "no ping since Nov 12 03:00 UTC (expected every 1d)") {
		t.Fatalf("overdue: %+v", s)
	}
	m.CheckHeartbeats(ctx) // already down: no second alert
	if got := alerts.kinds(); len(got) != 1 || got[0] != notify.Down {
		t.Fatalf("alerts %v", got)
	}
}

func TestHeartbeatHungRun(t *testing.T) {
	ctx := context.Background()
	m, clk := heartbeatMonitor(t, openStore(t), &sent{})
	m.Heartbeat(ctx, "backup", Ping{Signal: ohp.Success})
	clk.advance(time.Hour)
	m.Heartbeat(ctx, "backup", Ping{Signal: ohp.Start})
	clk.advance(61 * time.Minute)
	m.CheckHeartbeats(ctx)
	if s := status(m); s.Status != Down || !strings.Contains(s.Last.Error, "did not finish within 1h") {
		t.Fatalf("hung run: %+v", s)
	}
}

func TestHeartbeatRestore(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	m1, _ := heartbeatMonitor(t, st, &sent{})
	m1.Heartbeat(ctx, "backup", Ping{Signal: ohp.Success})
	last := status(m1).LastSuccess

	alerts := &sent{}
	m2, clk := heartbeatMonitor(t, st, alerts)
	if err := m2.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if s := status(m2); s.Status != Up || !s.LastSuccess.Equal(last) {
		t.Fatalf("restored %+v", s)
	}
	// The deadline still counts from the success before the restart.
	clk.advance(26 * time.Hour)
	m2.CheckHeartbeats(ctx)
	if status(m2).Status != Down {
		t.Error("deadline lost across a restart")
	}
}

func TestRunSkipsHeartbeats(t *testing.T) {
	m := New([]config.Check{backup}, openStore(t), nil, nil)
	m.probe = nil // would panic if a heartbeat were probed
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	m.Run(ctx)
}

func TestErrorLine(t *testing.T) {
	cases := map[string]string{
		"pg_dump: disk full\ndumping\n":           "pg_dump: disk full",
		"starting\ncopied 12 files\n":             "copied 12 files",
		"Error: bucket not found\nretrying\ndone": "Error: bucket not found",
		"": "",
	}
	for body, want := range cases {
		if got := errorLine(body); got != want {
			t.Errorf("errorLine(%q) = %q, want %q", body, got, want)
		}
	}
}
