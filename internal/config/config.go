// Package config loads and validates the uptimer YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that also accepts a "d" suffix for days ("90d").
type Duration time.Duration

// UnmarshalYAML parses values such as "30s", "5m", "1h30m" or "90d".
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ParseDuration is time.ParseDuration plus whole days ("7d").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return parsed, nil
}

// Config is the whole configuration file.
type Config struct {
	Title     string    `yaml:"title"`
	Listen    string    `yaml:"listen"`
	Database  string    `yaml:"database"`
	Retention Duration  `yaml:"retention"`
	Defaults  Defaults  `yaml:"defaults"`
	Checks    []Check   `yaml:"checks"`
	Notify    Notifiers `yaml:"notify"`
	// HeartbeatKey is the secret part of heartbeat ping URLs
	// (/ping/<key>/<check id>). Required when a check has type heartbeat.
	HeartbeatKey string `yaml:"heartbeat_key"`
}

// Defaults apply to every check that does not set its own value.
type Defaults struct {
	Interval         Duration `yaml:"interval"`
	Timeout          Duration `yaml:"timeout"`
	FailureThreshold int      `yaml:"failure_threshold"`
	CertExpiryDays   int      `yaml:"cert_expiry_days"`
}

// Check is one thing to monitor.
type Check struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
	// Type is "http" (default when URL is set), "tcp" or "heartbeat".
	Type string `yaml:"type"`

	// Heartbeat checks are passive: the job pings uptimer (Open Heartbeat
	// Protocol) and is down when it reports a failure or misses Period + Grace.
	Period Duration `yaml:"period"`
	Grace  Duration `yaml:"grace"`

	// HTTP checks.
	URL          string            `yaml:"url"`
	Method       string            `yaml:"method"`
	Headers      map[string]string `yaml:"headers"`
	ExpectStatus []int             `yaml:"expect_status"`
	Contains     string            `yaml:"contains"`

	// TCP checks: "host:port".
	Address string `yaml:"address"`

	Interval         Duration `yaml:"interval"`
	Timeout          Duration `yaml:"timeout"`
	FailureThreshold int      `yaml:"failure_threshold"`
	CertExpiryDays   int      `yaml:"cert_expiry_days"`
}

// Target is what the check points at, for display.
func (c Check) Target() string {
	switch c.Type {
	case "tcp":
		return c.Address
	case "heartbeat":
		return "heartbeat every " + HumanPeriod(c.Period.Std())
	}
	return c.URL
}

// HumanPeriod formats a period as "24h", "30m" or "7d".
func HumanPeriod(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	return strings.TrimSuffix(s, "0m")
}

// Notifiers lists where alerts go. Every configured channel receives every alert.
type Notifiers struct {
	Telegram *Telegram `yaml:"telegram"`
	Webhooks []Webhook `yaml:"webhooks"`
	Email    *Email    `yaml:"email"`
}

// Telegram sends alerts through a bot (create one with @BotFather).
type Telegram struct {
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
}

// Webhook posts alerts to a URL. Format is "slack", "discord" or "json" (default).
type Webhook struct {
	URL    string `yaml:"url"`
	Format string `yaml:"format"`
}

// Email sends alerts over SMTP (STARTTLS when the server offers it).
type Email struct {
	Host     string   `yaml:"host"`
	Port     int      `yaml:"port"`
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads a configuration file. ${VAR} references are replaced with environment
// variables, so secrets such as bot tokens can stay out of the file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes, fills in defaults and validates a configuration.
func Parse(raw []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	// Expand ${VAR} in values only, never in comments or keys.
	var missing []string
	expandNode(&root, &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("environment variables not set: %s", strings.Join(missing, ", "))
	}
	expanded, err := yaml.Marshal(&root)
	if err != nil {
		return nil, err
	}

	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(expanded))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func expandNode(node *yaml.Node, missing *[]string) {
	if node.Kind == yaml.ScalarNode {
		node.Value = envPattern.ReplaceAllStringFunc(node.Value, func(match string) string {
			name := envPattern.FindStringSubmatch(match)[1]
			value, ok := os.LookupEnv(name)
			if !ok && !slices.Contains(*missing, name) {
				*missing = append(*missing, name)
			}
			return value
		})
	}
	for i, child := range node.Content {
		// Mapping nodes alternate key, value: leave the keys alone.
		if node.Kind == yaml.MappingNode && i%2 == 0 {
			continue
		}
		expandNode(child, missing)
	}
}

func (c *Config) applyDefaults() {
	if c.Title == "" {
		c.Title = "Status"
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Database == "" {
		c.Database = "uptimer.db"
	}
	if c.Retention == 0 {
		c.Retention = Duration(90 * 24 * time.Hour)
	}
	if c.Defaults.Interval == 0 {
		c.Defaults.Interval = Duration(time.Minute)
	}
	if c.Defaults.Timeout == 0 {
		c.Defaults.Timeout = Duration(10 * time.Second)
	}
	if c.Defaults.FailureThreshold == 0 {
		c.Defaults.FailureThreshold = 2
	}
	if c.Defaults.CertExpiryDays == 0 {
		c.Defaults.CertExpiryDays = 14
	}
	for i := range c.Checks {
		check := &c.Checks[i]
		if check.Type == "" {
			check.Type = "http"
		}
		check.Type = strings.ToLower(check.Type)
		if check.ID == "" {
			check.ID = Slug(check.Name)
		}
		if check.Method == "" {
			check.Method = "GET"
		}
		check.Method = strings.ToUpper(check.Method)
		if check.Interval == 0 {
			check.Interval = c.Defaults.Interval
		}
		if check.Timeout == 0 {
			check.Timeout = c.Defaults.Timeout
		}
		if check.FailureThreshold == 0 {
			check.FailureThreshold = c.Defaults.FailureThreshold
		}
		if check.CertExpiryDays == 0 {
			check.CertExpiryDays = c.Defaults.CertExpiryDays
		}
		if check.Type == "heartbeat" {
			// One failure report is enough; a quarter of the period, between
			// one minute and one hour, is the default grace.
			check.FailureThreshold = 1
			if check.Grace == 0 {
				check.Grace = Duration(min(max(check.Period.Std()/4, time.Minute), time.Hour))
			}
		}
	}
	for i := range c.Notify.Webhooks {
		hook := &c.Notify.Webhooks[i]
		hook.Format = strings.ToLower(hook.Format)
		if hook.Format == "" {
			hook.Format = "json"
		}
	}
	if c.Notify.Email != nil && c.Notify.Email.Port == 0 {
		c.Notify.Email.Port = 587
	}
}

func (c *Config) validate() error {
	var problems []error
	if len(c.Checks) == 0 {
		problems = append(problems, errors.New("no checks defined"))
	}
	seen := map[string]bool{}
	for i, check := range c.Checks {
		where := fmt.Sprintf("checks[%d]", i)
		if check.Name != "" {
			where = fmt.Sprintf("check %q", check.Name)
		}
		if check.Name == "" {
			problems = append(problems, fmt.Errorf("%s: name is required", where))
		}
		if check.ID == "" || !idPattern.MatchString(check.ID) {
			problems = append(problems, fmt.Errorf("%s: id must be lowercase letters, digits and dashes", where))
		}
		if seen[check.ID] {
			problems = append(problems, fmt.Errorf("%s: duplicate id %q", where, check.ID))
		}
		seen[check.ID] = true
		switch check.Type {
		case "http":
			u, err := url.Parse(check.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				problems = append(problems, fmt.Errorf("%s: url must be an http(s) URL", where))
			}
			for _, status := range check.ExpectStatus {
				if status < 100 || status > 599 {
					problems = append(problems, fmt.Errorf("%s: invalid expect_status %d", where, status))
				}
			}
		case "tcp":
			if _, _, err := net.SplitHostPort(check.Address); err != nil {
				problems = append(problems, fmt.Errorf("%s: address must be host:port", where))
			}
		case "heartbeat":
			if check.Period.Std() < 10*time.Second {
				problems = append(problems, fmt.Errorf("%s: heartbeat checks need a period of at least 10s", where))
			}
			if check.Grace.Std() < 0 {
				problems = append(problems, fmt.Errorf("%s: grace cannot be negative", where))
			}
			if len(c.HeartbeatKey) < 16 || !keyPattern.MatchString(c.HeartbeatKey) {
				problems = append(problems, fmt.Errorf("%s: heartbeat checks need heartbeat_key: at least 16 letters, digits, - or _", where))
			}
			continue // interval and timeout do not apply
		default:
			problems = append(problems, fmt.Errorf("%s: unknown type %q (http, tcp or heartbeat)", where, check.Type))
		}
		if check.Interval.Std() < time.Second {
			problems = append(problems, fmt.Errorf("%s: interval must be at least 1s", where))
		}
		if check.Timeout.Std() <= 0 || check.Timeout.Std() > check.Interval.Std() {
			problems = append(problems, fmt.Errorf("%s: timeout must be positive and not longer than the interval", where))
		}
		if check.FailureThreshold < 1 {
			problems = append(problems, fmt.Errorf("%s: failure_threshold must be at least 1", where))
		}
	}
	if t := c.Notify.Telegram; t != nil && (t.BotToken == "" || t.ChatID == "") {
		problems = append(problems, errors.New("notify.telegram: bot_token and chat_id are required"))
	}
	for i, hook := range c.Notify.Webhooks {
		if u, err := url.Parse(hook.URL); err != nil || u.Host == "" {
			problems = append(problems, fmt.Errorf("notify.webhooks[%d]: invalid url", i))
		}
		switch hook.Format {
		case "json", "slack", "discord":
		default:
			problems = append(problems, fmt.Errorf("notify.webhooks[%d]: format must be json, slack or discord", i))
		}
	}
	if e := c.Notify.Email; e != nil && (e.Host == "" || e.From == "" || len(e.To) == 0) {
		problems = append(problems, errors.New("notify.email: host, from and to are required"))
	}
	return errors.Join(problems...)
}

var (
	idPattern   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	nonSlugChar = regexp.MustCompile(`[^a-z0-9]+`)
)

// Slug turns a display name into an id: "My API (EU)" becomes "my-api-eu".
func Slug(name string) string {
	return strings.Trim(nonSlugChar.ReplaceAllString(strings.ToLower(name), "-"), "-")
}
