// Package monitor schedules the checks, tracks up/down state and raises alerts.
package monitor

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/check"
	"github.com/AndreaAcanfora/uptimer/internal/config"
	"github.com/AndreaAcanfora/uptimer/internal/notify"
	"github.com/AndreaAcanfora/uptimer/internal/store"
)

// Status of a check.
type Status string

const (
	Unknown Status = "unknown"
	Up      Status = "up"
	Down    Status = "down"
)

// State is the live view of one check, for the status page.
type State struct {
	Check      config.Check
	Status     Status
	Last       *check.Result
	Failures   int // consecutive failed probes
	CertExpiry time.Time
}

// Sender delivers alerts (notify.Multi in production).
type Sender interface {
	Send(ctx context.Context, e notify.Event) error
}

// Monitor runs all checks.
type Monitor struct {
	checks []config.Check
	store  *store.Store
	sender Sender
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
	probe  func(ctx context.Context, c config.Check, client *http.Client) check.Result

	mu       sync.RWMutex
	states   map[string]*State
	certWarn map[string]time.Time // last certificate warning per check
}

// New creates a monitor. sender may be nil (no alerts).
func New(checks []config.Check, st *store.Store, sender Sender, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	m := &Monitor{
		checks:   checks,
		store:    st,
		sender:   sender,
		client:   check.NewHTTPClient(),
		log:      log,
		now:      time.Now,
		probe:    check.Run,
		states:   map[string]*State{},
		certWarn: map[string]time.Time{},
	}
	for _, c := range checks {
		m.states[c.ID] = &State{Check: c, Status: Unknown}
	}
	return m
}

// Restore loads the last known state from the database, so a restart during an outage
// neither forgets the incident nor alerts about it twice.
func (m *Monitor) Restore(ctx context.Context) error {
	for _, c := range m.checks {
		last, err := m.store.LastResult(ctx, c.ID)
		if err != nil {
			return err
		}
		incident, err := m.store.OngoingIncident(ctx, c.ID)
		if err != nil {
			return err
		}
		m.mu.Lock()
		state := m.states[c.ID]
		state.Last = last
		switch {
		case incident != nil:
			state.Status = Down
			state.Failures = c.FailureThreshold
		case last != nil && last.OK:
			state.Status = Up
		}
		m.mu.Unlock()
	}
	return nil
}

// Run probes every check on its interval until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range m.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.loop(ctx, c)
		}()
	}
	wg.Wait()
}

func (m *Monitor) loop(ctx context.Context, c config.Check) {
	// Spread the first probes so checks don't all fire in the same instant.
	jitter := time.Duration(rand.Int64N(int64(min(c.Interval.Std(), 5*time.Second))))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}
	ticker := time.NewTicker(c.Interval.Std())
	defer ticker.Stop()
	for {
		m.ProbeOnce(ctx, c)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProbeOnce runs one probe, stores it and updates the state, alerting on changes.
func (m *Monitor) ProbeOnce(ctx context.Context, c config.Check) check.Result {
	result := m.probe(ctx, c, m.client)
	if ctx.Err() != nil {
		return result // shutting down: don't record a cancelled probe as a failure
	}
	if err := m.store.AddResult(ctx, result); err != nil {
		m.log.Error("storing result", "check", c.ID, "err", err)
	}

	m.mu.Lock()
	state := m.states[c.ID]
	state.Last = &result
	if !result.CertExpiry.IsZero() {
		state.CertExpiry = result.CertExpiry
	}
	previous := state.Status
	var event *notify.Event
	if result.OK {
		state.Failures = 0
		state.Status = Up
		if previous == Down {
			event = &notify.Event{Kind: notify.Up}
		}
	} else {
		state.Failures++
		if state.Failures >= c.FailureThreshold && previous != Down {
			state.Status = Down
			event = &notify.Event{Kind: notify.Down, Cause: result.Error}
		}
	}
	certEvent := m.certWarning(c, result)
	failures := state.Failures
	m.mu.Unlock()

	if event != nil {
		m.transition(ctx, c, result, *event)
	}
	if certEvent != nil {
		m.send(ctx, *certEvent)
	}
	if !result.OK {
		m.log.Warn("probe failed", "check", c.ID, "err", result.Error, "failures", failures)
	}
	return result
}

func (m *Monitor) transition(ctx context.Context, c config.Check, result check.Result, event notify.Event) {
	event.CheckID, event.Check, event.Target, event.At = c.ID, c.Name, c.Target(), result.At
	switch event.Kind {
	case notify.Down:
		m.log.Error("check down", "check", c.ID, "cause", result.Error)
		if err := m.store.OpenIncident(ctx, c.ID, result.At, result.Error); err != nil {
			m.log.Error("opening incident", "check", c.ID, "err", err)
		}
	case notify.Up:
		incident, err := m.store.ResolveIncident(ctx, c.ID, result.At)
		if err != nil {
			m.log.Error("resolving incident", "check", c.ID, "err", err)
		}
		if incident != nil {
			event.Downtime = incident.Duration(result.At)
		}
		m.log.Info("check recovered", "check", c.ID, "downtime", event.Downtime)
	}
	m.send(ctx, event)
}

// certWarning returns an alert when the certificate expires within the configured
// days, at most once a day per check. Must be called with m.mu held.
func (m *Monitor) certWarning(c config.Check, result check.Result) *notify.Event {
	if result.CertExpiry.IsZero() || c.CertExpiryDays <= 0 {
		return nil
	}
	now := m.now()
	if result.CertExpiry.Sub(now) > time.Duration(c.CertExpiryDays)*24*time.Hour {
		return nil
	}
	if last, ok := m.certWarn[c.ID]; ok && now.Sub(last) < 24*time.Hour {
		return nil
	}
	m.certWarn[c.ID] = now
	return &notify.Event{
		Kind: notify.CertExpiry, CheckID: c.ID, Check: c.Name, Target: c.Target(),
		At: result.At, CertExpires: result.CertExpiry,
	}
}

func (m *Monitor) send(ctx context.Context, e notify.Event) {
	if m.sender == nil {
		return
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = m.sender.Send(sendCtx, e) // failures are logged by the sender
}

// States returns a snapshot of every check, in configuration order.
func (m *Monitor) States() []State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]State, 0, len(m.checks))
	for _, c := range m.checks {
		state := *m.states[c.ID]
		if state.Last != nil {
			last := *state.Last
			state.Last = &last
		}
		out = append(out, state)
	}
	return out
}

// PruneLoop deletes data older than retention once an hour.
func (m *Monitor) PruneLoop(ctx context.Context, retention time.Duration) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if removed, err := m.store.Prune(ctx, m.now().Add(-retention)); err != nil {
			m.log.Error("pruning", "err", err)
		} else if removed > 0 {
			m.log.Info("pruned old results", "rows", removed)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
