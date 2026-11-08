package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/monitor"
)

// Open Status Document (https://github.com/AndreaAcanfora/statusdoc): the
// status page as JSON in the summary.json shape that status page clients
// already read, plus discovery at /.well-known/status.

const osdVersion = "0.1"

type osdComponent struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	UpdatedAt string  `json:"updated_at,omitempty"`
	Position  int     `json:"position"`
	Group     bool    `json:"group"`
	GroupID   *string `json:"group_id"`
}

type osdUpdate struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type osdIncident struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Status          string         `json:"status"`
	Impact          string         `json:"impact"`
	CreatedAt       string         `json:"created_at"`
	UpdatedAt       string         `json:"updated_at"`
	StartedAt       string         `json:"started_at"`
	ResolvedAt      *string        `json:"resolved_at"`
	Shortlink       string         `json:"shortlink"`
	IncidentUpdates []osdUpdate    `json:"incident_updates"`
	Components      []osdComponent `json:"components"`
}

type osdDocument struct {
	OSDVersion string `json:"osd_version"`
	Page       struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		URL       string `json:"url"`
		UpdatedAt string `json:"updated_at"`
	} `json:"page"`
	Status struct {
		Indicator   string `json:"indicator"`
		Description string `json:"description"`
	} `json:"status"`
	Components            []osdComponent `json:"components"`
	Incidents             []osdIncident  `json:"incidents"`
	ScheduledMaintenances []osdIncident  `json:"scheduled_maintenances"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// baseURL is the public URL of the status page, as the client reached it
// (reverse proxies set X-Forwarded-Proto and X-Forwarded-Host).
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" && !strings.ContainsAny(h, "/ ,") {
		host = h
	}
	return scheme + "://" + host
}

func (s *Server) osdSummary(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	now := s.now()
	base := baseURL(r)
	var doc osdDocument
	doc.OSDVersion = osdVersion
	doc.Page.ID = "uptimer"
	doc.Page.Name = s.Title
	doc.Page.URL = base + "/"
	doc.Page.UpdatedAt = rfc3339(now)
	doc.Status.Description, _ = overall(views)
	doc.Components = []osdComponent{}
	doc.Incidents = []osdIncident{}
	doc.ScheduledMaintenances = []osdIncident{}

	byID := map[string]osdComponent{}
	down, known := 0, 0
	for i, v := range views {
		var status string
		switch v.Status {
		case monitor.Up:
			status = "operational"
		case monitor.Down:
			status = "major_outage"
			down++
		default:
			// Not checked yet: there is no OSD status for "unknown", so the
			// check is left out until its first result.
			continue
		}
		known++
		c := osdComponent{ID: v.ID, Name: v.Name, Status: status, Position: i}
		if !v.LastCheck.IsZero() {
			c.UpdatedAt = rfc3339(v.LastCheck)
		}
		doc.Components = append(doc.Components, c)
		byID[v.ID] = c
	}
	impact := "major"
	switch {
	case down == 0:
		doc.Status.Indicator = "none"
	case down == known:
		doc.Status.Indicator, impact = "critical", "critical"
	default:
		doc.Status.Indicator = "major"
	}

	// Open incidents, and those resolved in the last day.
	recent, err := s.Store.RecentIncidents(r.Context(), now.Add(-24*time.Hour), 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, inc := range recent {
		c, ok := byID[inc.CheckID]
		if !ok {
			continue
		}
		id := fmt.Sprintf("%s-%d", inc.CheckID, inc.ID)
		o := osdIncident{
			ID: id, Name: c.Name + " is down", Status: "investigating", Impact: impact,
			CreatedAt: rfc3339(inc.Started), UpdatedAt: rfc3339(inc.Started), StartedAt: rfc3339(inc.Started),
			Shortlink:       base + "/",
			IncidentUpdates: []osdUpdate{{ID: id + "-detected", Status: "investigating", Body: "Detected by uptimer: " + inc.Cause, CreatedAt: rfc3339(inc.Started)}},
			Components:      []osdComponent{c},
		}
		if !inc.Ongoing() {
			resolved := rfc3339(inc.Resolved)
			o.Status, o.UpdatedAt, o.ResolvedAt = "resolved", resolved, &resolved
			o.Components[0].Status = "operational"
			// Newest update first.
			o.IncidentUpdates = append([]osdUpdate{{ID: id + "-resolved", Status: "resolved", Body: "The check is passing again.", CreatedAt: resolved}}, o.IncidentUpdates...)
		}
		doc.Incidents = append(doc.Incidents, o)
	}
	s.writeOSD(w, r, doc)
}

func (s *Server) osdDiscovery(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	s.writeOSD(w, r, map[string]string{
		"osd_version": osdVersion,
		"document":    base + "/api/v2/summary.json",
		"page":        base + "/",
	})
}

// writeOSD sends a JSON document with the headers OSD asks for (section 6):
// CORS, a max-age and an ETag for conditional requests.
func (s *Server) writeOSD(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.fail(w, err)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "public, max-age=30")
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(append(body, '\n'))
}
