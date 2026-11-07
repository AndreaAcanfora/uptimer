package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
checks:
  - name: My API (EU)
    url: https://api.example.com/health
  - name: DB
    type: TCP
    address: db.example.com:5432
    interval: 30s
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.Database != "uptimer.db" || cfg.Retention.Std() != 90*24*time.Hour {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	api, db := cfg.Checks[0], cfg.Checks[1]
	if api.ID != "my-api-eu" || api.Type != "http" || api.Method != "GET" {
		t.Errorf("api check: %+v", api)
	}
	if api.Interval.Std() != time.Minute || api.Timeout.Std() != 10*time.Second || api.FailureThreshold != 2 {
		t.Errorf("api check timings: %+v", api)
	}
	if db.Type != "tcp" || db.Interval.Std() != 30*time.Second || db.Target() != "db.example.com:5432" {
		t.Errorf("db check: %+v", db)
	}
}

func TestEnvironmentVariablesOnlyInValues(t *testing.T) {
	t.Setenv("BOT_TOKEN", "secret-token")
	cfg, err := Parse([]byte(`
# ${NOT_SET_BUT_IN_A_COMMENT} must be ignored
checks:
  - name: Site
    url: https://example.com
notify:
  telegram:
    bot_token: ${BOT_TOKEN}
    chat_id: "42"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Telegram.BotToken != "secret-token" {
		t.Errorf("token = %q", cfg.Notify.Telegram.BotToken)
	}
}

func TestMissingEnvironmentVariable(t *testing.T) {
	_, err := Parse([]byte("checks:\n  - name: Site\n    url: https://${UPTIMER_TEST_UNSET}.com\n"))
	if err == nil || !strings.Contains(err.Error(), "UPTIMER_TEST_UNSET") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"no checks":        "title: x\n",
		"unknown field":    "checks:\n  - name: A\n    url: https://a.com\n    retries: 3\n",
		"bad url":          "checks:\n  - name: A\n    url: ftp://a.com\n",
		"bad address":      "checks:\n  - name: A\n    type: tcp\n    address: nope\n",
		"duplicate id":     "checks:\n  - name: A\n    url: https://a.com\n  - name: a\n    url: https://b.com\n",
		"timeout too long": "checks:\n  - name: A\n    url: https://a.com\n    interval: 5s\n    timeout: 10s\n",
		"bad status":       "checks:\n  - name: A\n    url: https://a.com\n    expect_status: [99]\n",
		"bad webhook":      "checks:\n  - name: A\n    url: https://a.com\nnotify:\n  webhooks:\n    - url: https://hooks.example.com\n      format: teams\n",
		"bad duration":     "checks:\n  - name: A\n    url: https://a.com\n    interval: soon\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	for input, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "30s": 30 * time.Second, "1h30m": 90 * time.Minute} {
		got, err := ParseDuration(input)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", input, got, err)
		}
	}
	if _, err := ParseDuration("-3d"); err == nil {
		t.Error("negative days should fail")
	}
}

func TestSlug(t *testing.T) {
	if got := Slug("  Main Website — EU/US!  "); got != "main-website-eu-us" {
		t.Errorf("Slug = %q", got)
	}
}
