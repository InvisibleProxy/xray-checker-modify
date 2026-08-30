package probeagent

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
)

const (
	ForwardedIPHeader = "X-Xray-Checker-Client-IP"
	ProxySecretHeader = "X-Xray-Checker-Proxy-Secret"
)

var ErrUntrustedProxy = errors.New("untrusted probe agent reverse proxy")

// RequestSourceIP uses the socket peer unless a reverse-proxy secret is
// configured. Forwarded headers are never trusted without that shared secret.
func RequestSourceIP(request *http.Request, trustedProxySecret string) (netip.Addr, error) {
	if trustedProxySecret != "" {
		provided := request.Header.Get(ProxySecretHeader)
		if len(provided) != len(trustedProxySecret) || subtle.ConstantTimeCompare([]byte(provided), []byte(trustedProxySecret)) != 1 {
			return netip.Addr{}, ErrUntrustedProxy
		}
		return parseExactIP(request.Header.Get(ForwardedIPHeader))
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return netip.Addr{}, err
	}
	return parseExactIP(host)
}
