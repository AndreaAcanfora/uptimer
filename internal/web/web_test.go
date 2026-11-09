package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndreaAcanfora/openheartbeat/conformance"
	"github.com/AndreaAcanfora/uptimer/internal/config"
	"github.com/AndreaAcanfora/uptimer/internal/monitor"
	"github.com/AndreaAcanfora/uptimer/internal/store"
)

func setup(t *testing.T, target string) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	checks := []config.Check{
		{ID: "site", Name: "Main <Site>", Type: "http", URL: target, Method: "GET",
			Timeout: config.Duration(time.Second), FailureThreshold: 1},
		{ID: "db", Name: "Database", Type: "tcp", Address: "127.0.0.1:1",
			Timeout: config.Duration(time.Second), FailureThreshold: 1},
	}
	m := monitor.New(checks, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, c := range checks {
		m.ProbeOnce(context.Background(), c)
	}
	site := &Server{Title: "Acme status", Version: "v9.9.9", Monitor: m, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	server := httptest.NewServer(site.Handler())
	t.Cleanup(server.Close)
	return server
}

func get(t *testing.T, url string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

func TestStatusPage(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()
	server := setup(t, target.URL)

	code, contentType, body := get(t, server.URL+"/")
	if code != 200 || !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("GET / = %d %s", code, contentType)
	}
	for _, want := range []string{
		"<title>Acme status</title>",
		"Partial outage: 1 of 2 down",
		"Main &lt;Site&gt;", // names are escaped
		`class="pill up"`, `class="pill down"`,
		"connection failed", // the database check's error
		"<b>Database</b> · <span class=\"ongoing\">ongoing for", // incident list
		"uptimer</a> v9.9.9",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status page is missing %q", want)
		}
	}
	if strings.Count(body, `title="`) < 2*HistoryDays {
		t.Error("expected 90 daily bars per check")
	}
}

func TestAPIStatus(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()
	server := setup(t, target.URL)
	code, contentType, body := get(t, server.URL+"/api/status")
	if code != 200 || contentType != "application/json" {
		t.Fatalf("GET /api/status = %d %s", code, contentType)
	}
	var data struct {
		Status string      `json:"status"`
		Checks []CheckView `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatal(err)
	}
	if data.Status != "Partial outage: 1 of 2 down" || len(data.Checks) != 2 {
		t.Fatalf("api = %+v", data)
	}
	if data.Checks[0].Status != monitor.Up || data.Checks[0].Uptime24h != 1 || data.Checks[1].LastError == "" {
		t.Errorf("checks = %+v", data.Checks)
	}
}

func TestBadges(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()
	server := setup(t, target.URL)

	code, contentType, body := get(t, server.URL+"/badge/site.svg")
	if code != 200 || contentType != "image/svg+xml" || !strings.Contains(body, "Main &lt;Site&gt;: up") {
		t.Errorf("status badge: %d %s %s", code, contentType, body)
	}
	_, _, body = get(t, server.URL+"/badge/db.svg?kind=uptime&label=db")
	if !strings.Contains(body, "<title>db: 0.00%</title>") || !strings.Contains(body, "#cf222e") {
		t.Errorf("uptime badge: %s", body)
	}
	for _, path := range []string{"/badge/missing.svg", "/badge/site.png", "/nope"} {
		if code, _, _ := get(t, server.URL+path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d", path, code)
		}
	}
	if code, _, body := get(t, server.URL+"/healthz"); code != 200 || body != "ok\n" {
		t.Errorf("healthz = %d %q", code, body)
	}
}

func TestPercent(t *testing.T) {
	for ratio, want := range map[float64]string{-1: "–", 1: "100%", 0.99996: "99.99%", 0.5: "50.00%"} {
		if got := Percent(ratio); got != want {
			t.Errorf("Percent(%v) = %q, want %q", ratio, got, want)
		}
	}
}

func TestSparkline(t *testing.T) {
	if sparkline([]time.Duration{-1, -1}, 100, 20) != "" {
		t.Error("no data should draw nothing")
	}
	svg := string(sparkline([]time.Duration{10 * time.Millisecond, 20 * time.Millisecond, -1, 5 * time.Millisecond, 5 * time.Millisecond}, 100, 20))
	if strings.Count(svg, "<polyline") != 2 || !strings.Contains(svg, "peak 20 ms") {
		t.Errorf("sparkline = %s", svg)
	}
}

func TestOpenStatusDocument(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()
	server := setup(t, target.URL)

	resp, err := http.Get(server.URL + "/api/v2/summary.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for header, want := range map[string]string{"Content-Type": "application/json; charset=utf-8", "Access-Control-Allow-Origin": "*", "Cache-Control": "public, max-age=30"} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	var doc struct {
		OSDVersion string `json:"osd_version"`
		Page       struct{ Name, URL string }
		Status     struct{ Indicator, Description string }
		Components []struct{ ID, Name, Status string }
		Incidents  []struct {
			Name, Status, Impact string
			Components           []struct{ ID, Status string }
			IncidentUpdates      []struct{ Body string } `json:"incident_updates"`
		}
		Maintenances []any `json:"scheduled_maintenances"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if doc.OSDVersion != "0.1" || doc.Page.Name != "Acme status" || doc.Page.URL != server.URL+"/" || doc.Maintenances == nil {
		t.Errorf("page %+v", doc)
	}
	if doc.Status.Indicator != "major" || doc.Status.Description != "Partial outage: 1 of 2 down" {
		t.Errorf("status %+v", doc.Status)
	}
	statuses := map[string]string{}
	for _, c := range doc.Components {
		statuses[c.ID] = c.Status
	}
	if statuses["site"] != "operational" || statuses["db"] != "major_outage" {
		t.Errorf("components %v", statuses)
	}
	if len(doc.Incidents) != 1 {
		t.Fatalf("incidents %+v", doc.Incidents)
	}
	inc := doc.Incidents[0]
	if inc.Name != "Database is down" || inc.Status != "investigating" || inc.Impact != "major" || inc.Components[0].ID != "db" || !strings.Contains(inc.IncidentUpdates[0].Body, "connection failed") {
		t.Errorf("incident %+v", inc)
	}

	// Conditional requests.
	req, _ := http.NewRequest("GET", server.URL+"/api/v2/summary.json", nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	again, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", again.StatusCode)
	}
}

func TestStatusDiscovery(t *testing.T) {
	server := setup(t, "http://127.0.0.1:1")
	req, _ := http.NewRequest("GET", server.URL+"/.well-known/status", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "status.acme.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]string
	json.NewDecoder(resp.Body).Decode(&doc)
	want := map[string]string{"osd_version": "0.1", "document": "https://status.acme.example/api/v2/summary.json", "page": "https://status.acme.example/"}
	for k, v := range want {
		if doc[k] != v {
			t.Errorf("%s = %q, want %q", k, doc[k], v)
		}
	}
}

func TestHeartbeatPingsConformToOHP(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	checks := []config.Check{{ID: "backup", Name: "Nightly backup", Type: "heartbeat",
		Period: config.Duration(24 * time.Hour), Grace: config.Duration(time.Hour), FailureThreshold: 1}}
	m := monitor.New(checks, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	site := &Server{Title: "Acme", HeartbeatKey: "0123456789abcdef", Monitor: m, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	server := httptest.NewServer(site.Handler())
	defer server.Close()
	checkURL := server.URL + "/ping/0123456789abcdef/backup"

	results, err := conformance.Run(context.Background(), server.Client(), checkURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s %s: %s", r.Level, r.Name, r.Detail)
		}
	}
	// The suite ends with a success: the check is up.
	if s := m.States()[0]; s.Status != monitor.Up {
		t.Errorf("state after the suite: %s", s.Status)
	}

	// A wrong key is a 404, and so is a check that is not a heartbeat.
	for _, u := range []string{server.URL + "/ping/wrong-key-0123456/backup", server.URL + "/ping/0123456789abcdef/site"} {
		if code, _, _ := get(t, u); code != http.StatusNotFound {
			t.Errorf("GET %s = %d", u, code)
		}
	}
	code, _, body := get(t, server.URL+"/.well-known/ohp")
	if code != 200 || !strings.Contains(body, `"versions":["0.2"]`) {
		t.Errorf("discovery %d %s", code, body)
	}
	// The status page shows the heartbeat.
	if _, _, page := get(t, server.URL+"/"); !strings.Contains(page, "heartbeat every 1d") {
		t.Error("status page does not describe the heartbeat")
	}
}
