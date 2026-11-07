package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), args, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func TestVersionAndHelp(t *testing.T) {
	if out, _, err := runCLI(t, "version"); err != nil || out != "uptimer dev\n" {
		t.Errorf("version = %q, %v", out, err)
	}
	if out, _, err := runCLI(t, "help"); err != nil || !strings.Contains(out, "uptimer run") {
		t.Errorf("help = %q, %v", out, err)
	}
	if _, _, err := runCLI(t); err == nil {
		t.Error("no command should fail")
	}
	if _, _, err := runCLI(t, "deploy"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown command: %v", err)
	}
}

func TestInitWritesAValidExample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uptimer.yml")
	if _, _, err := runCLI(t, "init", "-config", path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "init", "-config", path); err == nil {
		t.Error("init must not overwrite an existing file")
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Equal(raw, exampleConfig) {
		t.Error("example not written verbatim")
	}
}

func TestCheckCommand(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer healthy.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	dir := t.TempDir()
	good := filepath.Join(dir, "good.yml")
	_ = os.WriteFile(good, []byte("checks:\n  - name: Healthy\n    url: "+healthy.URL+"\n"), 0o644)
	out, _, err := runCLI(t, "check", "-config", good)
	if err != nil || !strings.Contains(out, "Healthy") || !strings.Contains(out, "HTTP 200") {
		t.Errorf("check = %q, %v", out, err)
	}

	bad := filepath.Join(dir, "bad.yml")
	_ = os.WriteFile(bad, []byte("checks:\n  - name: Healthy\n    url: "+healthy.URL+"\n  - name: Broken\n    url: "+broken.URL+"\n"), 0o644)
	out, _, err = runCLI(t, "check", "-config", bad)
	if err == nil || err.Error() != "1 of 2 checks failed" || !strings.Contains(out, "unexpected status 500") {
		t.Errorf("check = %q, %v", out, err)
	}

	if _, _, err := runCLI(t, "check", "-config", filepath.Join(dir, "missing.yml")); err == nil {
		t.Error("missing config should fail")
	}
}
