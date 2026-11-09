package monitor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/AndreaAcanfora/openheartbeat/ohp"
	"github.com/AndreaAcanfora/uptimer/internal/check"
	"github.com/AndreaAcanfora/uptimer/internal/config"
	"github.com/AndreaAcanfora/uptimer/internal/notify"
)

// ErrNoSuchHeartbeat is returned for pings to an unknown heartbeat check.
var ErrNoSuchHeartbeat = errors.New("no such heartbeat check")

// Ping is one Open Heartbeat Protocol signal received for a heartbeat check.
type Ping struct {
	Signal ohp.Signal
	RunID  string
	Body   string
	// Duration is the client-reported run time (OHP-Duration), used when no
	// start signal was seen.
	Duration time.Duration
}

// Heartbeat records a signal for a heartbeat check: success and exit code 0
// bring it up, fail and other exit codes bring it down, start opens a run.
func (m *Monitor) Heartbeat(ctx context.Context, checkID string, p Ping) error {
	m.mu.Lock()
	state, ok := m.states[checkID]
	if !ok || state.Check.Type != "heartbeat" {
		m.mu.Unlock()
		return ErrNoSuchHeartbeat
	}
	c := state.Check
	now := m.now()
	if state.Runs == nil {
		state.Runs = map[string]time.Time{}
	}
	switch {
	case p.Signal == ohp.Start:
		state.Runs[p.RunID] = now
		m.mu.Unlock()
		return nil
	case !p.Signal.Completes(): // log
		m.mu.Unlock()
		return nil
	}
	duration := p.Duration
	if started, ok := state.Runs[p.RunID]; ok {
		duration = now.Sub(started)
		delete(state.Runs, p.RunID)
	}
	result := check.Result{CheckID: c.ID, At: now, OK: !p.Signal.Failed(), Latency: duration}
	if !result.OK {
		result.Error = failureCause(p)
	}
	m.mu.Unlock()
	m.record(ctx, c, result)
	return nil
}

func failureCause(p Ping) string {
	cause := "the job reported a failure"
	if code, ok := p.Signal.Code(); ok {
		cause = fmt.Sprintf("the job exited with status %d", code)
	}
	if line := errorLine(p.Body); line != "" {
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		cause += ": " + line
	}
	return cause
}

var errorWords = regexp.MustCompile(`(?i)error|fail|fatal|panic|exception|denied|refused|not found|no such|cannot|can't|unable|timeout|timed out|full|killed`)

// errorLine picks the line of output that best explains a failure: the last
// of the final lines that looks like an error, or else the last line. Stdout
// and stderr arrive separately, so the error is not always the very last line.
func errorLine(body string) string {
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	tail := lines[max(0, len(lines)-10):]
	for i := len(tail) - 1; i >= 0; i-- {
		if errorWords.MatchString(tail[i]) {
			return tail[i]
		}
	}
	return tail[len(tail)-1]
}

// record stores a heartbeat result and applies the state change, like a probe.
func (m *Monitor) record(ctx context.Context, c config.Check, result check.Result) {
	if err := m.store.AddResult(ctx, result); err != nil {
		m.log.Error("storing result", "check", c.ID, "err", err)
	}
	m.mu.Lock()
	state := m.states[c.ID]
	state.Last = &result
	previous := state.Status
	var event *notify.Event
	if result.OK {
		state.Failures = 0
		state.Status = Up
		state.LastSuccess = result.At
		if previous == Down {
			event = &notify.Event{Kind: notify.Up}
		}
	} else {
		state.Failures++
		if previous != Down {
			state.Status = Down
			event = &notify.Event{Kind: notify.Down, Cause: result.Error}
		}
	}
	m.mu.Unlock()
	if event != nil {
		m.transition(ctx, c, result, *event)
	}
	if !result.OK {
		m.log.Warn("heartbeat failed", "check", c.ID, "cause", result.Error)
	}
}

// CheckHeartbeats marks as down the heartbeat checks whose next success is
// overdue by more than their grace time, or whose run started and did not
// finish within the grace time. Checks that never pinged stay unknown.
func (m *Monitor) CheckHeartbeats(ctx context.Context) {
	now := m.now()
	type miss struct {
		c     config.Check
		cause string
	}
	var misses []miss
	m.mu.Lock()
	for _, c := range m.checks {
		if c.Type != "heartbeat" {
			continue
		}
		state := m.states[c.ID]
		if state.Status == Down || state.LastSuccess.IsZero() && len(state.Runs) == 0 {
			continue
		}
		grace := c.Grace.Std()
		for rid, started := range state.Runs {
			if now.Sub(started) > grace {
				delete(state.Runs, rid)
				misses = append(misses, miss{c, fmt.Sprintf("the job started at %s and did not finish within %s", started.UTC().Format("15:04 MST"), config.HumanPeriod(grace))})
				break
			}
		}
		if !state.LastSuccess.IsZero() && now.Sub(state.LastSuccess) > c.Period.Std()+grace {
			misses = append(misses, miss{c, fmt.Sprintf("no ping since %s (expected every %s)", state.LastSuccess.UTC().Format("Jan 2 15:04 MST"), config.HumanPeriod(c.Period.Std()))})
		}
	}
	m.mu.Unlock()
	seen := map[string]bool{}
	for _, x := range misses {
		if seen[x.c.ID] {
			continue
		}
		seen[x.c.ID] = true
		m.record(ctx, x.c, check.Result{CheckID: x.c.ID, At: now, OK: false, Error: x.cause})
	}
}

func (m *Monitor) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CheckHeartbeats(ctx)
		}
	}
}
