package check

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/config"
)

func httpCheck(url string) config.Check {
	return config.Check{ID: "c", Type: "http", URL: url, Method: "GET", Timeout: config.Duration(2 * time.Second)}
}

func TestHTTPStatusAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != UserAgent || r.Header.Get("X-Token") != "abc" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/teapot":
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewHTTPClient()

	c := httpCheck(server.URL + "/ok")
	c.Headers = map[string]string{"X-Token": "abc"}
	c.Contains = `"status":"ok"`
	if r := Run(context.Background(), c, client); !r.OK || r.StatusCode != 200 || r.CheckID != "c" {
		t.Errorf("ok check: %+v", r)
	}

	c.Contains = "degraded"
	if r := Run(context.Background(), c, client); r.OK || !strings.Contains(r.Error, "does not contain") {
		t.Errorf("contains check: %+v", r)
	}

	missing := httpCheck(server.URL + "/missing")
	missing.Headers = c.Headers
	if r := Run(context.Background(), missing, client); r.OK || r.Error != "unexpected status 404" {
		t.Errorf("404 check: %+v", r)
	}

	teapot := httpCheck(server.URL + "/teapot")
	teapot.Headers = c.Headers
	teapot.ExpectStatus = []int{418}
	if r := Run(context.Background(), teapot, client); !r.OK {
		t.Errorf("expected status 418 should pass: %+v", r)
	}
}

func TestHTTPTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()
	c := httpCheck(server.URL)
	c.Timeout = config.Duration(100 * time.Millisecond)
	r := Run(context.Background(), c, NewHTTPClient())
	if r.OK || r.Error != "timeout" {
		t.Errorf("timeout check: %+v", r)
	}
}

func TestHTTPSCertificateExpiry(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()
	r := Run(context.Background(), httpCheck(server.URL), server.Client())
	if !r.OK || r.CertExpiry.IsZero() {
		t.Errorf("TLS check: %+v", r)
	}
	// The default client does not trust the test certificate.
	r = Run(context.Background(), httpCheck(server.URL), NewHTTPClient())
	if r.OK || !strings.Contains(r.Error, "certificate") {
		t.Errorf("untrusted certificate: %+v", r)
	}
}

func TestTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	c := config.Check{ID: "db", Type: "tcp", Address: address, Timeout: config.Duration(time.Second)}
	if r := Run(context.Background(), c, nil); !r.OK {
		t.Errorf("open port: %+v", r)
	}
	_ = listener.Close()
	if r := Run(context.Background(), c, nil); r.OK || !strings.HasPrefix(r.Error, "connection failed") {
		t.Errorf("closed port: %+v", r)
	}
}

func TestDNSFailure(t *testing.T) {
	r := Run(context.Background(), httpCheck("http://does-not-exist.invalid"), NewHTTPClient())
	if r.OK || !strings.HasPrefix(r.Error, "DNS lookup failed") {
		t.Errorf("DNS failure: %+v", r)
	}
}
