package monitor

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/check"
	"github.com/AndreaAcanfora/uptimer/internal/config"
	"github.com/AndreaAcanfora/uptimer/internal/notify"
	"github.com/AndreaAcanfora/uptimer/internal/store"
)

type sent struct {
	mu     sync.Mutex
	events []notify.Event
}

func (s *sent) Send(_ context.Context, e notify.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *sent) kinds() []notify.Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []notify.Kind
	for _, e := range s.events {
		out = append(out, e.Kind)
	}
	return out
}

var api = config.Check{
	ID: "api", Name: "API", Type: "http", URL: "https://api.example.com",
	Interval: config.Duration(time.Minute), Timeout: config.Duration(time.Second),
	FailureThreshold: 2, CertExpiryDays: 14,
}

// scripted returns a monitor whose probes return the given outcomes in order,
// one minute apart.
func scripted(t *testing.T, st *store.Store, sender Sender, outcomes ...bool) *Monitor {
	t.Helper()
	m := New([]config.Check{api}, st, sender, nil)
	clock := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return clock }
	i := 0
	m.probe = func(_ context.Context, c config.Check, _ *http.Client) check.Result {
		ok := outcomes[i]
		i++
		clock = clock.Add(time.Minute)
		r := check.Result{CheckID: c.ID, At: clock, OK: ok, Latency: 50 * time.Millisecond}
		if !ok {
			r.Error = "unexpected status 502"
		}
		return r
	}
	return m
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAlertsOnlyOnStateChanges(t *testing.T) {
	st, sender := openStore(t), &sent{}
	m := scripted(t, st, sender, true, false, false, false, true, true)
	ctx := context.Background()

	m.ProbeOnce(ctx, api) // up
	m.ProbeOnce(ctx, api) // 1 failure: below threshold
	if len(sender.kinds()) != 0 || m.States()[0].Status != Up {
		t.Fatalf("one failure should not alert: %v %v", sender.kinds(), m.States()[0].Status)
	}
	m.ProbeOnce(ctx, api) // 2 failures: down
	m.ProbeOnce(ctx, api) // still down: no second alert
	if got := sender.kinds(); len(got) != 1 || got[0] != notify.Down {
		t.Fatalf("events = %v", got)
	}
	if sender.events[0].Cause != "unexpected status 502" || sender.events[0].Target != api.URL {
		t.Errorf("down event = %+v", sender.events[0])
	}
	m.ProbeOnce(ctx, api) // recovered
	m.ProbeOnce(ctx, api)
	got := sender.kinds()
	if len(got) != 2 || got[1] != notify.Up {
		t.Fatalf("events = %v", got)
	}
	// Down when the threshold was reached (09:03), up at 09:05.
	if sender.events[1].Downtime != 2*time.Minute {
		t.Errorf("downtime = %v", sender.events[1].Downtime)
	}
	if incident, _ := st.OngoingIncident(ctx, "api"); incident != nil {
		t.Errorf("incident still open: %+v", incident)
	}
	ratio, probes, _ := st.Uptime(ctx, "api", time.Time{})
	if probes != 6 || ratio != 0.5 {
		t.Errorf("stored %d probes, uptime %v", probes, ratio)
	}
}

func TestRestartDuringOutageDoesNotAlertAgain(t *testing.T) {
	st, ctx := openStore(t), context.Background()
	first := &sent{}
	m := scripted(t, st, first, false, false)
	m.ProbeOnce(ctx, api)
	m.ProbeOnce(ctx, api)
	if len(first.kinds()) != 1 {
		t.Fatalf("events = %v", first.kinds())
	}

	second := &sent{}
	restarted := scripted(t, st, second, false, true)
	if err := restarted.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if restarted.States()[0].Status != Down {
		t.Fatalf("restored status = %v", restarted.States()[0].Status)
	}
	restarted.ProbeOnce(ctx, api) // still failing: already alerted before the restart
	restarted.ProbeOnce(ctx, api) // recovers: the incident from before the restart closes
	if got := second.kinds(); len(got) != 1 || got[0] != notify.Up {
		t.Errorf("events after restart = %v", got)
	}
}

func TestCertificateWarningOncePerDay(t *testing.T) {
	st, sender := openStore(t), &sent{}
	m := New([]config.Check{api}, st, sender, nil)
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	expiry := now.Add(5 * 24 * time.Hour)
	m.probe = func(context.Context, config.Check, *http.Client) check.Result {
		return check.Result{CheckID: "api", At: now, OK: true, CertExpiry: expiry}
	}
	ctx := context.Background()
	m.ProbeOnce(ctx, api)
	m.ProbeOnce(ctx, api)
	now = now.Add(25 * time.Hour)
	m.ProbeOnce(ctx, api)
	got := sender.kinds()
	if len(got) != 2 || got[0] != notify.CertExpiry {
		t.Errorf("events = %v", got)
	}
	if m.States()[0].CertExpiry != expiry {
		t.Error("cert expiry not kept in state")
	}

	far := &sent{}
	m2 := New([]config.Check{api}, st, far, nil)
	m2.probe = func(context.Context, config.Check, *http.Client) check.Result {
		return check.Result{CheckID: "api", At: time.Now(), OK: true, CertExpiry: time.Now().Add(60 * 24 * time.Hour)}
	}
	m2.ProbeOnce(ctx, api)
	if len(far.kinds()) != 0 {
		t.Error("no warning expected for a certificate far from expiry")
	}
}

func TestCancelledProbeIsNotRecorded(t *testing.T) {
	st := openStore(t)
	m := scripted(t, st, nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.ProbeOnce(ctx, api)
	if _, probes, _ := st.Uptime(context.Background(), "api", time.Time{}); probes != 0 {
		t.Errorf("stored %d probes after shutdown", probes)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	st := openStore(t)
	fast := api
	fast.Interval = config.Duration(10 * time.Millisecond)
	m := New([]config.Check{fast}, st, nil, nil)
	var mu sync.Mutex
	probes := 0
	m.probe = func(context.Context, config.Check, *http.Client) check.Result {
		mu.Lock()
		probes++
		mu.Unlock()
		return check.Result{CheckID: "api", At: time.Now(), OK: true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	m.Run(ctx) // returns when the context ends
	mu.Lock()
	defer mu.Unlock()
	if probes < 2 {
		t.Errorf("only %d probes ran", probes)
	}
}
