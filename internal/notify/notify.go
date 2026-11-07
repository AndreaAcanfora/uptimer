// Package notify sends alerts when a check goes down, recovers, or its certificate is
// about to expire.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/config"
)

// Kind of event.
type Kind string

const (
	Down       Kind = "down"
	Up         Kind = "up"
	CertExpiry Kind = "cert_expiry"
)

// Event is what an alert is about.
type Event struct {
	Kind     Kind          `json:"kind"`
	CheckID  string        `json:"check_id"`
	Check    string        `json:"check"`
	Target   string        `json:"target"`
	At       time.Time     `json:"at"`
	Cause    string        `json:"cause,omitempty"`
	Downtime time.Duration `json:"-"`
	// CertExpires is set for CertExpiry events.
	CertExpires time.Time `json:"cert_expires,omitzero"`
}

// MarshalJSON adds the downtime in seconds and the ready-made message text.
func (e Event) MarshalJSON() ([]byte, error) {
	type plain Event
	return json.Marshal(struct {
		plain
		DowntimeSeconds int64  `json:"downtime_seconds,omitempty"`
		Message         string `json:"message"`
	}{plain(e), int64(e.Downtime.Seconds()), e.Text()})
}

// Subject is a one-line summary, used as email subject and first line elsewhere.
func (e Event) Subject() string {
	switch e.Kind {
	case Down:
		return fmt.Sprintf("🔴 %s is DOWN", e.Check)
	case Up:
		return fmt.Sprintf("🟢 %s is back UP", e.Check)
	default:
		days := int(time.Until(e.CertExpires).Hours() / 24)
		return fmt.Sprintf("🟠 %s: TLS certificate expires in %d days", e.Check, days)
	}
}

// Text is the full plain-text message.
func (e Event) Text() string {
	var b strings.Builder
	b.WriteString(e.Subject())
	b.WriteString("\n")
	b.WriteString(e.Target)
	switch e.Kind {
	case Down:
		fmt.Fprintf(&b, "\nCause: %s", e.Cause)
	case Up:
		fmt.Fprintf(&b, "\nDown for %s", HumanDuration(e.Downtime))
	case CertExpiry:
		fmt.Fprintf(&b, "\nExpires on %s", e.CertExpires.UTC().Format("2006-01-02"))
	}
	fmt.Fprintf(&b, "\n%s", e.At.UTC().Format("2006-01-02 15:04 UTC"))
	return b.String()
}

// HumanDuration formats durations like "3m", "1h 12m" or "2d 4h".
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// Notifier delivers an event somewhere.
type Notifier interface {
	Name() string
	Send(ctx context.Context, e Event) error
}

// Multi sends every event to every notifier, logging failures instead of stopping.
type Multi struct {
	Notifiers []Notifier
	Log       *slog.Logger
}

// Send delivers to all notifiers and returns the joined errors.
func (m Multi) Send(ctx context.Context, e Event) error {
	var errs []error
	for _, n := range m.Notifiers {
		if err := n.Send(ctx, e); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.Name(), err))
			if m.Log != nil {
				m.Log.Error("notification failed", "notifier", n.Name(), "check", e.CheckID, "err", err)
			}
		}
	}
	return errors.Join(errs...)
}

// FromConfig builds the notifiers listed in the configuration.
func FromConfig(cfg config.Notifiers, client *http.Client) []Notifier {
	var out []Notifier
	if cfg.Telegram != nil {
		out = append(out, &Telegram{Token: cfg.Telegram.BotToken, ChatID: cfg.Telegram.ChatID, Client: client})
	}
	for _, hook := range cfg.Webhooks {
		out = append(out, &Webhook{URL: hook.URL, Format: hook.Format, Client: client})
	}
	if cfg.Email != nil {
		out = append(out, &Email{Config: *cfg.Email})
	}
	return out
}

// TelegramAPI is the Bot API base URL (overridden in tests).
var TelegramAPI = "https://api.telegram.org"

// Telegram sends messages through a Telegram bot.
type Telegram struct {
	Token  string
	ChatID string
	Client *http.Client
}

func (t *Telegram) Name() string { return "telegram" }

func (t *Telegram) Send(ctx context.Context, e Event) error {
	payload := map[string]any{
		"chat_id":                  t.ChatID,
		"text":                     e.Text(),
		"disable_web_page_preview": true,
	}
	return postJSON(ctx, t.Client, fmt.Sprintf("%s/bot%s/sendMessage", TelegramAPI, t.Token), payload)
}

// Webhook posts the event to a URL as Slack, Discord or plain JSON.
type Webhook struct {
	URL    string
	Format string
	Client *http.Client
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) Send(ctx context.Context, e Event) error {
	var payload any
	switch w.Format {
	case "slack":
		payload = map[string]string{"text": e.Text()}
	case "discord":
		payload = map[string]string{"content": e.Text()}
	default:
		payload = e
	}
	return postJSON(ctx, w.Client, w.URL, payload)
}

func postJSON(ctx context.Context, client *http.Client, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return redact(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

// redact hides the URL of a failed request: Telegram puts the bot token in the path.
func redact(err error) error {
	var urlErr interface{ Unwrap() error }
	if errors.As(err, &urlErr) {
		if inner := urlErr.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}

// Email sends alerts over SMTP.
type Email struct {
	Config config.Email
	// send is smtp.SendMail, replaceable in tests.
	send func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func (m *Email) Name() string { return "email" }

func (m *Email) Send(_ context.Context, e Event) error {
	c := m.Config
	var auth smtp.Auth
	if c.Username != "" {
		auth = smtp.PlainAuth("", c.Username, c.Password, c.Host)
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n",
		c.From, strings.Join(c.To, ", "), mimeHeader(e.Subject()), strings.ReplaceAll(e.Text(), "\n", "\r\n"))
	send := m.send
	if send == nil {
		send = smtp.SendMail
	}
	return send(fmt.Sprintf("%s:%d", c.Host, c.Port), auth, c.From, c.To, []byte(msg))
}

// mimeHeader encodes non-ASCII subjects (the status emojis) as RFC 2047.
func mimeHeader(s string) string { return mime.BEncoding.Encode("UTF-8", s) }
