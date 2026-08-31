package checker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
)

func TestFailureFromErrorClassifiesTransportStages(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "dns", err: &net.DNSError{Err: "no such host", Name: "check.example"}, code: FailureCodeDNS},
		{name: "proxy handshake", err: errors.New("socks connect tcp: authentication failed"), code: FailureCodeProxyHandshake},
		{name: "refused", err: errors.New("connect: connection refused"), code: FailureCodeTCPRefused},
		{name: "tls", err: errors.New("tls: failed to verify certificate: x509 unknown authority"), code: FailureCodeTLS},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := failureFromError(test.err)
			if got.Code != test.code || got.Summary == "" {
				t.Fatalf("failure = %+v, want code %q", got, test.code)
			}
		})
	}
}

func TestDiagnoseFailureUsesDirectHostEvidence(t *testing.T) {
	base := FailureDetails{Code: FailureCodeProxyTimeout, Summary: FailureSummary(FailureCodeProxyTimeout)}
	got := DiagnoseFailure(base,
		HostCheckDetails{Checked: true, Online: false, Error: "connect: connection refused"},
		PingCheckDetails{Checked: true, Online: true},
	)
	if got.Code != FailureCodeTCPRefused {
		t.Fatalf("failure = %+v, want TCP refused", got)
	}

	got = DiagnoseFailure(base,
		HostCheckDetails{Checked: true, Online: false, Error: "TCP check timeout"},
		PingCheckDetails{Checked: true, Online: true},
	)
	if got.Code != FailureCodeTCPTimeout || got.Summary != "Хост отвечает, но TCP-порт недоступен" {
		t.Fatalf("failure = %+v, want reachable host with closed/filtered port", got)
	}
}

func TestFailureFromErrorClassifiesTunnelCloseAsStreamClosed(t *testing.T) {
	// Exactly what the checker logs when an xHTTP node accepts the connection and
	// then hangs up: `Get "http://cp.cloudflare.com/generate_204": EOF`.
	err := &url.Error{Op: "Get", URL: "http://cp.cloudflare.com/generate_204", Err: io.EOF}

	failure := failureFromError(err)

	if failure.Code != FailureCodeProxyStreamClosed {
		t.Fatalf("Code = %q, want %q", failure.Code, FailureCodeProxyStreamClosed)
	}
	if failure.Summary == FailureSummary(FailureCodeUnknown) {
		t.Fatalf("Summary must not fall back to the unknown text, got %q", failure.Summary)
	}
	if !strings.Contains(failure.Detail, "cp.cloudflare.com") {
		t.Errorf("Detail = %q, want the original request text preserved", failure.Detail)
	}
}

func TestFailureFromErrorClassifiesResetAndBrokenPipe(t *testing.T) {
	for _, text := range []string{
		"read tcp 127.0.0.1:10005->127.0.0.1:1080: read: connection reset by peer",
		"write tcp 127.0.0.1:10005->127.0.0.1:1080: write: broken pipe",
		"unexpected EOF",
	} {
		failure := failureFromError(&url.Error{Op: "Get", URL: "http://example.com", Err: errors.New(text)})
		if failure.Code != FailureCodeProxyStreamClosed {
			t.Errorf("Code for %q = %q, want %q", text, failure.Code, FailureCodeProxyStreamClosed)
		}
	}
}

func TestFailureFromErrorStillReportsTimeoutForSlowTunnel(t *testing.T) {
	// The reality nodes fail differently: the tunnel is up but no headers arrive.
	err := &url.Error{
		Op:  "Get",
		URL: "http://cp.cloudflare.com/generate_204",
		Err: context.DeadlineExceeded,
	}

	if got := failureFromError(err).Code; got != FailureCodeProxyTimeout {
		t.Fatalf("Code = %q, want %q", got, FailureCodeProxyTimeout)
	}
}
