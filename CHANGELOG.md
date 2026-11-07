# Changelog

## 0.1.0 (unreleased)

First version.

- HTTP(S) checks (status codes, body contains, headers, method) and TCP checks.
- Down after N consecutive failures; alerts only on state changes, with downtime on recovery.
- State restored from the database on restart: no duplicate alerts during an ongoing outage.
- TLS certificate expiry warnings, at most once a day per check.
- Notifications: Telegram, Slack, Discord, JSON webhooks, SMTP email.
- Status page with 24h/7d/30d uptime, 90 daily bars, response-time chart, incidents; light and dark theme.
- JSON API, SVG status and uptime badges, health endpoint.
- YAML configuration with `${ENV}` secrets and strict validation; `init`, `check`, `run` commands.
- SQLite storage (pure Go) with automatic pruning; static binaries and a Docker image.
