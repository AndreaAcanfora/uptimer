// Package web serves the public status page, a JSON API and README badges.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/monitor"
	"github.com/AndreaAcanfora/uptimer/internal/notify"
	"github.com/AndreaAcanfora/uptimer/internal/store"
)

//go:embed templates/*.html
var templates embed.FS

var page = template.Must(template.New("status.html").Funcs(template.FuncMap{
	"human":   notify.HumanDuration,
	"percent": Percent,
}).ParseFS(templates, "templates/status.html"))

// HistoryDays is how many daily bars the status page shows.
const HistoryDays = 90

// Server renders the status page from the monitor's live state and the stored history.
type Server struct {
	Title   string
	Version string
	Monitor *monitor.Monitor
	Store   *store.Store
	Log     *slog.Logger
	Now     func() time.Time
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.statusPage)
	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("GET /badge/{file}", s.badge)
	mux.HandleFunc("GET /api/v2/summary.json", s.osdSummary)
	mux.HandleFunc("GET /.well-known/status", s.osdDiscovery)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// CheckView is one check as shown on the page and in the API.
type CheckView struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Target     string         `json:"target"`
	Status     monitor.Status `json:"status"`
	LastError  string         `json:"last_error,omitempty"`
	LastCheck  time.Time      `json:"last_check,omitzero"`
	LatencyMS  int64          `json:"latency_ms"`
	Uptime24h  float64        `json:"uptime_24h"`
	Uptime7d   float64        `json:"uptime_7d"`
	Uptime30d  float64        `json:"uptime_30d"`
	CertExpiry time.Time      `json:"cert_expiry,omitzero"`

	Days      []DayView     `json:"-"`
	Sparkline template.HTML `json:"-"`
}

// DayView is one bar of the 90-day history.
type DayView struct {
	Class string
	Title string
}

// IncidentView is one row of the incident list.
type IncidentView struct {
	Check    string
	Cause    string
	Started  time.Time
	Duration time.Duration
	Ongoing  bool
}

type pageData struct {
	Title     string
	Version   string
	Overall   string
	Class     string
	Updated   time.Time
	Checks    []CheckView
	Incidents []IncidentView
}

func (s *Server) views(ctx context.Context, withHistory bool) ([]CheckView, error) {
	now := s.now()
	var views []CheckView
	for _, state := range s.Monitor.States() {
		c := state.Check
		view := CheckView{ID: c.ID, Name: c.Name, Target: c.Target(), Status: state.Status, CertExpiry: state.CertExpiry}
		if state.Last != nil {
			view.LastCheck = state.Last.At
			view.LatencyMS = state.Last.Latency.Milliseconds()
			if !state.Last.OK {
				view.LastError = state.Last.Error
			}
		}
		for _, window := range []struct {
			dst *float64
			ago time.Duration
		}{{&view.Uptime24h, 24 * time.Hour}, {&view.Uptime7d, 7 * 24 * time.Hour}, {&view.Uptime30d, 30 * 24 * time.Hour}} {
			ratio, _, err := s.Store.Uptime(ctx, c.ID, now.Add(-window.ago))
			if err != nil {
				return nil, err
			}
			*window.dst = ratio
		}
		if withHistory {
			days, err := s.Store.Days(ctx, c.ID, HistoryDays, now)
			if err != nil {
				return nil, err
			}
			for _, day := range days {
				view.Days = append(view.Days, dayView(day))
			}
			latency, err := s.Store.Latency(ctx, c.ID, now.Add(-24*time.Hour), now, 48)
			if err != nil {
				return nil, err
			}
			view.Sparkline = sparkline(latency, 240, 36)
		}
		views = append(views, view)
	}
	return views, nil
}

func dayView(day store.Day) DayView {
	date := day.Date.Format("Jan 2, 2006")
	switch {
	case day.Uptime < 0:
		return DayView{Class: "none", Title: date + " · no data"}
	case day.Uptime >= 0.999:
		return DayView{Class: "ok", Title: fmt.Sprintf("%s · %s uptime", date, Percent(day.Uptime))}
	case day.Uptime >= 0.95:
		return DayView{Class: "warn", Title: fmt.Sprintf("%s · %s uptime", date, Percent(day.Uptime))}
	default:
		return DayView{Class: "bad", Title: fmt.Sprintf("%s · %s uptime", date, Percent(day.Uptime))}
	}
}

// Percent formats an uptime ratio: "99.98%", "100%", or "–" when unknown.
func Percent(ratio float64) string {
	if ratio < 0 {
		return "–"
	}
	if ratio >= 1 {
		return "100%"
	}
	// Floor, so 99.996% shows as 99.99% and never rounds up to a misleading 100%.
	return fmt.Sprintf("%.2f%%", math.Floor(ratio*10000)/100)
}

// sparkline draws average latency as an SVG polyline; gaps (no data) break the line.
func sparkline(values []time.Duration, width, height int) template.HTML {
	peak := time.Duration(0)
	for _, v := range values {
		peak = max(peak, v)
	}
	if peak <= 0 || len(values) < 2 {
		return ""
	}
	step := float64(width) / float64(len(values)-1)
	var lines []string
	var current []string
	flush := func() {
		if len(current) > 1 {
			lines = append(lines, fmt.Sprintf(`<polyline points="%s"/>`, strings.Join(current, " ")))
		}
		current = nil
	}
	for i, v := range values {
		if v < 0 {
			flush()
			continue
		}
		y := float64(height-2) - float64(v)/float64(peak)*float64(height-4)
		current = append(current, fmt.Sprintf("%.1f,%.1f", float64(i)*step, y))
	}
	flush()
	svg := fmt.Sprintf(`<svg class="spark" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-label="Response time, last 24 hours, peak %d ms">%s</svg>`,
		width, height, width, height, peak.Milliseconds(), strings.Join(lines, ""))
	return template.HTML(svg) // built only from numbers above
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(r.Context(), true)
	if err != nil {
		s.fail(w, err)
		return
	}
	now := s.now()
	recent, err := s.Store.RecentIncidents(r.Context(), now.Add(-14*24*time.Hour), 20)
	if err != nil {
		s.fail(w, err)
		return
	}
	names := map[string]string{}
	for _, v := range views {
		names[v.ID] = v.Name
	}
	data := pageData{Title: s.Title, Version: s.Version, Updated: now, Checks: views}
	for _, incident := range recent {
		name, ok := names[incident.CheckID]
		if !ok {
			continue // a check that was removed from the configuration
		}
		data.Incidents = append(data.Incidents, IncidentView{
			Check: name, Cause: incident.Cause, Started: incident.Started,
			Duration: incident.Duration(now), Ongoing: incident.Ongoing(),
		})
	}
	data.Overall, data.Class = overall(views)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(w, data); err != nil {
		s.Log.Error("rendering status page", "err", err)
	}
}

func overall(views []CheckView) (string, string) {
	down, unknown := 0, 0
	for _, v := range views {
		switch v.Status {
		case monitor.Down:
			down++
		case monitor.Unknown:
			unknown++
		}
	}
	switch {
	case down == len(views) && down > 0:
		return "Major outage", "bad"
	case down > 0:
		return fmt.Sprintf("Partial outage: %d of %d down", down, len(views)), "warn"
	case unknown == len(views):
		return "Waiting for the first checks", "none"
	default:
		return "All systems operational", "ok"
	}
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	overallText, _ := overall(views)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"title": s.Title, "status": overallText, "updated": s.now(), "checks": views,
	})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("serving request", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
