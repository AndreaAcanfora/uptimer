// Command uptimer monitors websites and services and serves a public status page.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/check"
	"github.com/AndreaAcanfora/uptimer/internal/config"
	"github.com/AndreaAcanfora/uptimer/internal/monitor"
	"github.com/AndreaAcanfora/uptimer/internal/notify"
	"github.com/AndreaAcanfora/uptimer/internal/store"
	"github.com/AndreaAcanfora/uptimer/internal/web"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

//go:embed example.yml
var exampleConfig []byte

const usage = `uptimer: monitor websites and services, get alerts, publish a status page.

Usage:
  uptimer run   [-config uptimer.yml]   monitor and serve the status page
  uptimer check [-config uptimer.yml]   probe every check once and print the results
  uptimer init  [-config uptimer.yml]   write an example configuration
  uptimer version
`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("missing command")
	}
	command, rest := args[0], args[1:]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", envOr("UPTIMER_CONFIG", "uptimer.yml"), "configuration file")
	switch command {
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, "uptimer", version)
		return nil
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	case "run", "check", "init":
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}
	if err := flags.Parse(rest); err != nil {
		return err
	}
	switch command {
	case "init":
		return writeExample(*configPath, stdout)
	case "check":
		return checkOnce(ctx, *configPath, stdout)
	default:
		return serve(ctx, *configPath, stderr)
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func writeExample(path string, stdout io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if err := os.WriteFile(path, exampleConfig, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wrote %s. Edit it, then run: uptimer run -config %s\n", path, path)
	return nil
}

// checkOnce probes every check once. It exits with an error if any check fails, so it
// also works as a smoke test in CI or a cron job.
func checkOnce(ctx context.Context, path string, stdout io.Writer) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	client := check.NewHTTPClient()
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "CHECK\tSTATUS\tTIME\tDETAIL")
	failed := 0
	for _, c := range cfg.Checks {
		result := check.Run(ctx, c, client)
		status, detail := "up", ""
		if result.StatusCode != 0 {
			detail = fmt.Sprintf("HTTP %d", result.StatusCode)
		}
		if !result.OK {
			status, detail = "DOWN", result.Error
			failed++
		}
		if !result.CertExpiry.IsZero() {
			days := int(time.Until(result.CertExpiry).Hours() / 24)
			detail += fmt.Sprintf(", cert %dd", days)
		}
		fmt.Fprintf(table, "%s\t%s\t%d ms\t%s\n", c.Name, status, result.Latency.Milliseconds(), detail)
	}
	_ = table.Flush()
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(cfg.Checks))
	}
	return nil
}

func serve(ctx context.Context, path string, stderr io.Writer) error {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		return fmt.Errorf("opening %s: %w", cfg.Database, err)
	}
	defer st.Close()

	notifiers := notify.FromConfig(cfg.Notify, &http.Client{Timeout: 20 * time.Second})
	mon := monitor.New(cfg.Checks, st, notify.Multi{Notifiers: notifiers, Log: log}, log)
	if err := mon.Restore(ctx); err != nil {
		return fmt.Errorf("restoring state: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	site := &web.Server{Title: cfg.Title, Version: version, HeartbeatKey: cfg.HeartbeatKey, Monitor: mon, Store: st, Log: log}
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           site.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Info("status page listening", "addr", cfg.Listen, "checks", len(cfg.Checks), "notifiers", len(notifiers))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()
	go mon.PruneLoop(ctx, cfg.Retention.Std())
	done := make(chan struct{})
	go func() {
		mon.Run(ctx)
		close(done)
	}()

	select {
	case err := <-serverErr:
		stop()
		<-done
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	<-done
	return nil
}
