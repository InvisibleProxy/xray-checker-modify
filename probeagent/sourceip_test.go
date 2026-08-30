package probeagent

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestRequestSourceIPNeverTrustsForwardedHeaderWithoutSecret(t *testing.T) {
	request := httptest.NewRequest("POST", EnrollPath, nil)
	request.RemoteAddr = "203.0.113.40:12345"
	request.Header.Set(ForwardedIPHeader, "198.51.100.70")
	address, err := RequestSourceIP(request, "")
	if err != nil || address != netip.MustParseAddr("203.0.113.40") {
		t.Fatalf("source IP = %v, %v", address, err)
	}
}

func TestRequestSourceIPRequiresTrustedProxySecret(t *testing.T) {
	request := httptest.NewRequest("POST", EnrollPath, nil)
	request.RemoteAddr = "172.18.0.4:12345"
	request.Header.Set(ForwardedIPHeader, "203.0.113.40")
	request.Header.Set(ProxySecretHeader, "wrong")
	if _, err := RequestSourceIP(request, "right"); err != ErrUntrustedProxy {
		t.Fatalf("untrusted proxy error = %v", err)
	}
	request.Header.Set(ProxySecretHeader, "right")
	address, err := RequestSourceIP(request, "right")
	if err != nil || address != netip.MustParseAddr("203.0.113.40") {
		t.Fatalf("trusted forwarded source IP = %v, %v", address, err)
	}
}
