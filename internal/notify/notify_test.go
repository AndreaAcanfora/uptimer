package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/config"
)

var at = time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)

func TestEventText(t *testing.T) {
	down := Event{Kind: Down, Check: "API", Target: "https://api.example.com", At: at, Cause: "timeout"}
	if got := down.Text(); got != "🔴 API is DOWN\nhttps://api.example.com\nCause: timeout\n2026-11-07 09:30 UTC" {
		t.Errorf("down text = %q", got)
	}
	up := Event{Kind: Up, Check: "API", At: at, Downtime: 75 * time.Minute}
	if !strings.Contains(up.Text(), "Down for 1h 15m") {
		t.Errorf("up text = %q", up.Text())
	}
	cert := Event{Kind: CertExpiry, Check: "API", At: at, CertExpires: time.Now().Add(10*24*time.Hour + time.Hour)}
	if !strings.Contains(cert.Subject(), "expires in 10 days") {
		t.Errorf("cert subject = %q", cert.Subject())
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second: "45s", 3 * time.Minute: "3m", 2*time.Hour + 5*time.Minute: "2h 5m", 50 * time.Hour: "2d 2h",
	} {
		if got := HumanDuration(d); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

type recorder struct {
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
}

func (r *recorder) server(t *testing.T, status int) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		r.mu.Lock()
		r.paths, r.bodies = append(r.paths, req.URL.Path), append(r.bodies, body)
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestTelegram(t *testing.T) {
	rec := &recorder{}
	server := rec.server(t, http.StatusOK)
	old := TelegramAPI
	TelegramAPI = server.URL
	t.Cleanup(func() { TelegramAPI = old })

	tg := &Telegram{Token: "123:ABC", ChatID: "42", Client: server.Client()}
	if err := tg.Send(context.Background(), Event{Kind: Down, Check: "API", At: at}); err != nil {
		t.Fatal(err)
	}
	if rec.paths[0] != "/bot123:ABC/sendMessage" || rec.bodies[0]["chat_id"] != "42" {
		t.Errorf("telegram request: %v %v", rec.paths, rec.bodies)
	}
}

func TestWebhookFormats(t *testing.T) {
	rec := &recorder{}
	server := rec.server(t, http.StatusNoContent)
	event := Event{Kind: Up, CheckID: "api", Check: "API", At: at, Downtime: 90 * time.Second}
	for _, format := range []string{"slack", "discord", "json"} {
		hook := &Webhook{URL: server.URL, Format: format, Client: server.Client()}
		if err := hook.Send(context.Background(), event); err != nil {
			t.Fatal(format, err)
		}
	}
	if !strings.HasPrefix(rec.bodies[0]["text"].(string), "🟢 API is back UP") {
		t.Errorf("slack body: %v", rec.bodies[0])
	}
	if _, ok := rec.bodies[1]["content"]; !ok {
		t.Errorf("discord body: %v", rec.bodies[1])
	}
	plain := rec.bodies[2]
	if plain["kind"] != "up" || plain["check_id"] != "api" || plain["downtime_seconds"] != float64(90) || plain["message"] == nil {
		t.Errorf("json body: %v", plain)
	}
}

func TestErrorsDoNotLeakTheToken(t *testing.T) {
	old := TelegramAPI
	TelegramAPI = "http://127.0.0.1:1" // nothing listens here
	t.Cleanup(func() { TelegramAPI = old })
	err := (&Telegram{Token: "SECRET-TOKEN", ChatID: "1", Client: http.DefaultClient}).Send(context.Background(), Event{Kind: Down})
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("err = %v", err)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	server := (&recorder{}).server(t, http.StatusBadRequest)
	err := (&Webhook{URL: server.URL, Format: "json", Client: server.Client()}).Send(context.Background(), Event{Kind: Down})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("err = %v", err)
	}
}

func TestEmail(t *testing.T) {
	var gotAddr, gotMsg string
	var gotTo []string
	mail := &Email{
		Config: config.Email{Host: "smtp.example.com", Port: 587, Username: "u", Password: "p", From: "me@example.com", To: []string{"you@example.com"}},
		send: func(addr string, _ smtp.Auth, _ string, to []string, msg []byte) error {
			gotAddr, gotTo, gotMsg = addr, to, string(msg)
			return nil
		},
	}
	if err := mail.Send(context.Background(), Event{Kind: Down, Check: "API", At: at, Cause: "timeout"}); err != nil {
		t.Fatal(err)
	}
	if gotAddr != "smtp.example.com:587" || gotTo[0] != "you@example.com" {
		t.Errorf("addr %q to %v", gotAddr, gotTo)
	}
	if !strings.Contains(gotMsg, "Subject: =?UTF-8?b?") || !strings.Contains(gotMsg, "Cause: timeout\r\n") {
		t.Errorf("message:\n%s", gotMsg)
	}
}

type failing struct{}

func (failing) Name() string                      { return "broken" }
func (failing) Send(context.Context, Event) error { return errors.New("boom") }

func TestMultiKeepsGoing(t *testing.T) {
	rec := &recorder{}
	server := rec.server(t, http.StatusOK)
	multi := Multi{Notifiers: []Notifier{failing{}, &Webhook{URL: server.URL, Format: "json", Client: server.Client()}}}
	err := multi.Send(context.Background(), Event{Kind: Down})
	if err == nil || !strings.Contains(err.Error(), "broken: boom") || len(rec.bodies) != 1 {
		t.Errorf("err = %v, delivered = %d", err, len(rec.bodies))
	}
}

func TestFromConfig(t *testing.T) {
	notifiers := FromConfig(config.Notifiers{
		Telegram: &config.Telegram{BotToken: "t", ChatID: "c"},
		Webhooks: []config.Webhook{{URL: "https://a"}, {URL: "https://b"}},
		Email:    &config.Email{Host: "h"},
	}, http.DefaultClient)
	var names []string
	for _, n := range notifiers {
		names = append(names, n.Name())
	}
	if strings.Join(names, ",") != "telegram,webhook,webhook,email" {
		t.Errorf("names = %v", names)
	}
}
