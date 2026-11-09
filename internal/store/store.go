// Package store keeps check results and incidents in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/check"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, one static binary
)

const schema = `
CREATE TABLE IF NOT EXISTS results (
	check_id    TEXT    NOT NULL,
	at          INTEGER NOT NULL, -- unix milliseconds
	ok          INTEGER NOT NULL,
	latency_ms  INTEGER NOT NULL,
	status_code INTEGER NOT NULL DEFAULT 0,
	error       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS results_check_at ON results (check_id, at);

CREATE TABLE IF NOT EXISTS incidents (
	id       INTEGER PRIMARY KEY,
	check_id TEXT    NOT NULL,
	started  INTEGER NOT NULL,
	resolved INTEGER,
	cause    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS incidents_check ON incidents (check_id, started);
`

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// Open opens (and creates if needed) the database at path.
func Open(path string) (*Store, error) {
	// URI form, with forward slashes so Windows paths work too.
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time; a single connection avoids "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// AddResult stores the outcome of one probe.
func (s *Store) AddResult(ctx context.Context, r check.Result) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO results (check_id, at, ok, latency_ms, status_code, error) VALUES (?, ?, ?, ?, ?, ?)`,
		r.CheckID, r.At.UnixMilli(), r.OK, r.Latency.Milliseconds(), r.StatusCode, r.Error)
	return err
}

// Incident is a period during which a check was down.
type Incident struct {
	ID       int64
	CheckID  string
	Started  time.Time
	Resolved time.Time // zero while ongoing
	Cause    string
}

// Ongoing reports whether the incident is still open.
func (i Incident) Ongoing() bool { return i.Resolved.IsZero() }

// Duration is how long the check was (or has been) down.
func (i Incident) Duration(now time.Time) time.Duration {
	if i.Ongoing() {
		return now.Sub(i.Started)
	}
	return i.Resolved.Sub(i.Started)
}

// OpenIncident starts an incident for a check.
func (s *Store) OpenIncident(ctx context.Context, checkID string, at time.Time, cause string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO incidents (check_id, started, cause) VALUES (?, ?, ?)`,
		checkID, at.UnixMilli(), cause)
	return err
}

// ResolveIncident closes the open incident of a check, if any, and returns it.
func (s *Store) ResolveIncident(ctx context.Context, checkID string, at time.Time) (*Incident, error) {
	incident, err := s.OngoingIncident(ctx, checkID)
	if err != nil || incident == nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE incidents SET resolved = ? WHERE id = ?`,
		at.UnixMilli(), incident.ID); err != nil {
		return nil, err
	}
	incident.Resolved = time.UnixMilli(at.UnixMilli()) // same precision as stored
	return incident, nil
}

// OngoingIncident returns the open incident of a check, or nil.
func (s *Store) OngoingIncident(ctx context.Context, checkID string) (*Incident, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, check_id, started, resolved, cause FROM incidents
		 WHERE check_id = ? AND resolved IS NULL ORDER BY started DESC LIMIT 1`, checkID)
	incident, err := scanIncident(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return incident, err
}

// RecentIncidents returns the latest incidents across all checks, newest first.
func (s *Store) RecentIncidents(ctx context.Context, since time.Time, limit int) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, check_id, started, resolved, cause FROM incidents
		 WHERE started >= ? OR resolved IS NULL ORDER BY started DESC LIMIT ?`,
		since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var incidents []Incident
	for rows.Next() {
		incident, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		incidents = append(incidents, *incident)
	}
	return incidents, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanIncident(row scanner) (*Incident, error) {
	var (
		incident Incident
		started  int64
		resolved sql.NullInt64
	)
	if err := row.Scan(&incident.ID, &incident.CheckID, &started, &resolved, &incident.Cause); err != nil {
		return nil, err
	}
	incident.Started = time.UnixMilli(started)
	if resolved.Valid {
		incident.Resolved = time.UnixMilli(resolved.Int64)
	}
	return &incident, nil
}

// Uptime is the share of successful probes since a time, and how many probes there were.
// With no probes the ratio is -1 (unknown).
func (s *Store) Uptime(ctx context.Context, checkID string, since time.Time) (ratio float64, probes int, err error) {
	var ok sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), SUM(ok) FROM results WHERE check_id = ? AND at >= ?`,
		checkID, since.UnixMilli()).Scan(&probes, &ok)
	if err != nil || probes == 0 {
		return -1, probes, err
	}
	return float64(ok.Int64) / float64(probes), probes, nil
}

// Day summarizes one calendar day (UTC) of probes.
type Day struct {
	Date   time.Time
	Probes int
	Uptime float64 // -1 when there were no probes
}

// Days returns one entry per day for the last n days, oldest first, including today.
func (s *Store) Days(ctx context.Context, checkID string, n int, now time.Time) ([]Day, error) {
	today := now.UTC().Truncate(24 * time.Hour)
	first := today.AddDate(0, 0, -(n - 1))
	rows, err := s.db.QueryContext(ctx,
		`SELECT at / 86400000 AS day, COUNT(*), SUM(ok) FROM results
		 WHERE check_id = ? AND at >= ? GROUP BY day`, checkID, first.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDay := map[int64][2]int64{}
	for rows.Next() {
		var day, count, ok int64
		if err := rows.Scan(&day, &count, &ok); err != nil {
			return nil, err
		}
		byDay[day] = [2]int64{count, ok}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	days := make([]Day, n)
	for i := range days {
		date := first.AddDate(0, 0, i)
		counts := byDay[date.Unix()/86400]
		days[i] = Day{Date: date, Probes: int(counts[0]), Uptime: -1}
		if counts[0] > 0 {
			days[i].Uptime = float64(counts[1]) / float64(counts[0])
		}
	}
	return days, nil
}

// Latency returns the average latency of successful probes in n equal buckets between
// since and now, oldest first. Buckets without data are -1.
func (s *Store) Latency(ctx context.Context, checkID string, since, now time.Time, n int) ([]time.Duration, error) {
	span := now.Sub(since).Milliseconds()
	if span <= 0 || n <= 0 {
		return nil, nil
	}
	width := max(span/int64(n), 1)
	rows, err := s.db.QueryContext(ctx,
		`SELECT (at - ?) / ? AS bucket, AVG(latency_ms) FROM results
		 WHERE check_id = ? AND ok = 1 AND at >= ? AND at <= ? GROUP BY bucket`,
		since.UnixMilli(), width, checkID, since.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = -1
	}
	for rows.Next() {
		var bucket int64
		var avg float64
		if err := rows.Scan(&bucket, &avg); err != nil {
			return nil, err
		}
		if bucket >= 0 && bucket < int64(n) {
			out[bucket] = time.Duration(avg * float64(time.Millisecond))
		}
	}
	return out, rows.Err()
}

// LastResult returns the most recent probe of a check, or nil.
func (s *Store) LastResult(ctx context.Context, checkID string) (*check.Result, error) {
	var (
		r       check.Result
		at, lat int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT check_id, at, ok, latency_ms, status_code, error FROM results
		 WHERE check_id = ? ORDER BY at DESC LIMIT 1`, checkID).
		Scan(&r.CheckID, &at, &r.OK, &lat, &r.StatusCode, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.At = time.UnixMilli(at)
	r.Latency = time.Duration(lat) * time.Millisecond
	return &r, nil
}

// LastSuccess returns the most recent successful result of a check, or nil.
func (s *Store) LastSuccess(ctx context.Context, checkID string) (*check.Result, error) {
	var (
		r       check.Result
		at, lat int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT check_id, at, ok, latency_ms, status_code, error FROM results
		 WHERE check_id = ? AND ok = 1 ORDER BY at DESC LIMIT 1`, checkID).
		Scan(&r.CheckID, &at, &r.OK, &lat, &r.StatusCode, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.At = time.UnixMilli(at)
	r.Latency = time.Duration(lat) * time.Millisecond
	return &r, nil
}

// Prune deletes results and resolved incidents older than before.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM results WHERE at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM incidents WHERE resolved IS NOT NULL AND resolved < ?`, before.UnixMilli()); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
