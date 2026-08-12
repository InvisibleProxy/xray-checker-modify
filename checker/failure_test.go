package checker

import (
	"errors"
	"net"
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
