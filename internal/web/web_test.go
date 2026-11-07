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
