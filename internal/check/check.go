// Package check runs a single probe against an HTTP endpoint or a TCP port.
package check

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/config"
)

// Result is the outcome of one probe.
type Result struct {
	CheckID    string
	At         time.Time
	OK         bool
	Latency    time.Duration
	StatusCode int
	Error      string
	// CertExpiry is when the server's TLS certificate expires (HTTPS only).
	CertExpiry time.Time
}

// maxBody caps how much of a response is read when looking for "contains".
const maxBody = 1 << 20

// UserAgent identifies the probes in server logs.
var UserAgent = "uptimer (+https://github.com/AndreaAcanfora/uptimer)"

// Run probes the target once.
func Run(ctx context.Context, c config.Check, client *http.Client) Result {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout.Std())
	defer cancel()
	start := time.Now()
	var result Result
	switch c.Type {
	case "tcp":
		result = runTCP(ctx, c)
	default:
		result = runHTTP(ctx, c, client)
	}
	result.CheckID = c.ID
	result.At = start
	if result.Latency == 0 {
		result.Latency = time.Since(start)
	}
	return result
}

func runHTTP(ctx context.Context, c config.Check, client *http.Client) Result {
	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, nil)
	if err != nil {
		return Result{Error: err.Error()}
	}
	req.Header.Set("User-Agent", UserAgent)
	for key, value := range c.Headers {
		req.Header.Set(key, value)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{Error: describe(err)}
	}
	defer resp.Body.Close()

	result := Result{StatusCode: resp.StatusCode, Latency: time.Since(start)}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		result.CertExpiry = resp.TLS.PeerCertificates[0].NotAfter
	}
	if !statusOK(c.ExpectStatus, resp.StatusCode) {
		result.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		return result
	}
	if c.Contains != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			result.Error = "reading body: " + describe(err)
			return result
		}
		if !bytes.Contains(body, []byte(c.Contains)) {
			result.Error = fmt.Sprintf("response does not contain %q", c.Contains)
			return result
		}
	}
	result.OK = true
	return result
}

func statusOK(expected []int, got int) bool {
	if len(expected) == 0 {
		return got >= 200 && got < 400
	}
	return slices.Contains(expected, got)
}

func runTCP(ctx context.Context, c config.Check) Result {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", c.Address)
	if err != nil {
		return Result{Error: describe(err)}
	}
	_ = conn.Close()
	return Result{OK: true}
}

// describe turns network errors into short, readable causes for alerts.
func describe(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var opErr *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dnsErr):
		return "DNS lookup failed: " + dnsErr.Err
	case errors.As(err, &certErr):
		return "TLS certificate invalid: " + certErr.Err.Error()
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "connection failed: " + opErr.Err.Error()
	}
	return err.Error()
}

// NewHTTPClient returns the client used for HTTP checks. Redirects are followed (up to
// 10, the net/http default); keep-alives are off so each probe measures a fresh connection.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	return &http.Client{Transport: transport}
}
