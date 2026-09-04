package nodearchive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A subscription may publish a hostname. The geo services answer about
// addresses, so the name is resolved first and the lookups are made for the
// resulting IP.
func TestGeoLookupResolvesAHostname(t *testing.T) {
	store := NewStore(t.TempDir()+"/node_registry.json", nil)
	store.resolveHost = func(_ context.Context, host string) (string, error) {
		if host != "node.example-vpn.net" {
			t.Fatalf("resolver received %q", host)
		}
		return "198.51.100.7", nil
	}

	var requested []string
	store.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requested = append(requested, request.URL.String())
		if request.URL.Host == "ifconfig.net" {
			return jsonResponse(`{"ip":"198.51.100.7","country":"Germany","country_iso":"DE","asn":"AS64500","asn_org":"Example"}`), nil
		}
		return jsonResponse(`{"ip":"198.51.100.7","country":"DE","org":"AS1 Example"}`), nil
	})}

	record := NodeRecord{StableID: "node", Server: "node.example-vpn.net:443", Active: true}
	updated, successes, errs := store.lookupGeo(context.Background(), record)
	if successes != 2 || len(errs) != 0 {
		t.Fatalf("successes=%d errors=%v, want both lookups to succeed", successes, errs)
	}
	for _, url := range requested {
		if strings.Contains(url, "node.example-vpn.net") {
			t.Fatalf("the geo request still carried the hostname: %s", url)
		}
		if !strings.Contains(url, "198.51.100.7") {
			t.Fatalf("the geo request did not use the resolved address: %s", url)
		}
	}
	if updated.GeoCountryCode != "DE" {
		t.Fatalf("country = %q, want the looked-up one", updated.GeoCountryCode)
	}
}

// An address is already answerable and must not be sent to a resolver.
func TestGeoLookupLeavesAnAddressAlone(t *testing.T) {
	store := NewStore(t.TempDir()+"/node_registry.json", nil)
	store.resolveHost = func(_ context.Context, host string) (string, error) {
		t.Fatalf("an address must not be resolved, got %q", host)
		return "", nil
	}
	store.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if !strings.Contains(request.URL.String(), "203.0.113.9") {
			t.Fatalf("unexpected target: %s", request.URL)
		}
		if request.URL.Host == "ifconfig.net" {
			return jsonResponse(`{"ip":"203.0.113.9","country":"Netherlands","country_iso":"NL"}`), nil
		}
		return jsonResponse(`{"ip":"203.0.113.9","country":"NL"}`), nil
	})}

	record := NodeRecord{StableID: "node", Server: "203.0.113.9:443", Active: true}
	if _, successes, errs := store.lookupGeo(context.Background(), record); successes != 2 || len(errs) != 0 {
		t.Fatalf("successes=%d errors=%v", successes, errs)
	}
}

// A name that does not resolve is reported once, against both services, rather
// than as two separate lookup failures that say nothing about the cause.
func TestGeoLookupReportsAResolveFailureOnce(t *testing.T) {
	store := NewStore(t.TempDir()+"/node_registry.json", nil)
	store.resolveHost = func(_ context.Context, _ string) (string, error) {
		return "", errors.New("no such host")
	}
	store.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("no geo request may be made when the name did not resolve: %s", request.URL)
		return nil, nil
	})}

	record := NodeRecord{StableID: "node", Server: "gone.example-vpn.net:443", Active: true}
	updated, successes, errs := store.lookupGeo(context.Background(), record)
	if successes != 0 || len(errs) != 1 {
		t.Fatalf("successes=%d errors=%v, want a single failure", successes, errs)
	}
	if updated.GeoError == "" || updated.IfconfigError == "" {
		t.Fatalf("both services must record why nothing was looked up: %+v", updated)
	}
}

func TestResolveGeoTargetPassesAddressesThrough(t *testing.T) {
	for _, address := range []string{"203.0.113.9", "2606:4700::1111"} {
		resolved, err := resolveGeoTarget(context.Background(), address)
		if err != nil || resolved != address {
			t.Fatalf("resolveGeoTarget(%q) = %q, %v", address, resolved, err)
		}
	}
}
