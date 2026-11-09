package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/AndreaAcanfora/openheartbeat/ohp"
	"github.com/AndreaAcanfora/uptimer/internal/monitor"
)

// Open Heartbeat Protocol (https://github.com/AndreaAcanfora/openheartbeat):
// jobs report to /ping/<heartbeat_key>/<check id>[/start|/fail|/log|/<exit code>].

const maxPingBody = 100_000

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("OHP-Version", ohp.Version)
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		pingReply(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPingBody+1))
	if err != nil {
		pingReply(w, r, http.StatusBadRequest, "unreadable body")
		return
	}
	truncated := len(body) > maxPingBody
	if truncated {
		body = body[:maxPingBody]
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 10<<20))
	}
	sig, err := ohp.ParseSignal(r.PathValue("signal"))
	if err != nil {
		pingReply(w, r, http.StatusBadRequest, "unknown signal")
		return
	}
	rid := r.URL.Query().Get("rid")
	if r.URL.Query().Has("rid") && !ohp.ValidRunID(rid) {
		pingReply(w, r, http.StatusBadRequest, "invalid rid")
		return
	}
	if s.HeartbeatKey == "" || subtle.ConstantTimeCompare([]byte(r.PathValue("key")), []byte(s.HeartbeatKey)) != 1 {
		pingReply(w, r, http.StatusNotFound, "not found")
		return
	}
	var duration time.Duration
	if secs, err := strconv.ParseFloat(r.Header.Get("OHP-Duration"), 64); err == nil && secs > 0 && secs < 1e9 {
		duration = time.Duration(secs * float64(time.Second))
	}
	err = s.Monitor.Heartbeat(r.Context(), r.PathValue("id"), monitor.Ping{Signal: sig, RunID: rid, Body: string(body), Duration: duration})
	if errors.Is(err, monitor.ErrNoSuchHeartbeat) {
		pingReply(w, r, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if truncated {
		w.Header().Set("OHP-Log-Truncated", "1")
	}
	pingReply(w, r, http.StatusOK, "OK")
}

func pingReply(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, msg+"\n")
	}
}

func (s *Server) ohpDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(map[string]any{"versions": []string{ohp.Version}, "max_body_bytes": maxPingBody, "create": false})
}
