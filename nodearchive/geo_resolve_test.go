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

// ipinfo.io describes an address; handed a hostname it searches for the name.
// The IP details link of a node published by name has to lead to the address
// the geo lookups resolved, and the link is absent until one has.
func TestIPInfoURLOfAHostnameLeadsToTheResolvedAddress(t *testing.T) {
	store := NewStore("", nil)
	store.nodes["node"] = NodeRecord{StableID: "node", Name: "DE node", Server: "node.example-vpn.net", Port: 443, Active: true}
	store.resolveHost = func(_ context.Context, _ string) (string, error) {
		return "198.51.100.7", nil
	}
	store.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "ifconfig.net" {
			return jsonResponse(`{"ip":"198.51.100.7","country":"Germany","country_iso":"DE"}`), nil
		}
		return jsonResponse(`{"ip":"198.51.100.7","country":"DE"}`), nil
	})}

	if got := store.Summaries(nil)[0].IPInfoURL; got != "" {
		t.Fatalf("before any lookup IPInfoURL = %q, want no link rather than a search for the name", got)
	}
	if _, err := store.RefreshGeo(context.Background(), nil); err != nil {
		t.Fatalf("RefreshGeo() error = %v", err)
	}
	if got := store.Summaries(nil)[0].IPInfoURL; got != "https://ipinfo.io/198.51.100.7" {
		t.Fatalf("IPInfoURL = %q, want the resolved address", got)
	}
}

func TestIPInfoURLNeverCarriesAName(t *testing.T) {
	tests := []struct {
		name   string
		record NodeRecord
		want   string
	}{
		{
			name:   "address published by the subscription",
			record: NodeRecord{Server: "203.0.113.9"},
			want:   "https://ipinfo.io/203.0.113.9",
		},
		{
			name:   "address with a port",
			record: NodeRecord{Server: "203.0.113.9:443"},
			want:   "https://ipinfo.io/203.0.113.9",
		},
		{
			name: "ipinfo failed and still holds an older address",
			record: NodeRecord{
				Server:              "node.example-vpn.net",
				GeoIP:               "192.0.2.1",
				GeoError:            "ipinfo status 429",
				IfconfigIP:          "198.51.100.7",
				IfconfigCountryCode: "DE",
			},
			want: "https://ipinfo.io/198.51.100.7",
		},
		{
			name: "every lookup failed",
			record: NodeRecord{
				Server:        "node.example-vpn.net",
				GeoIP:         "192.0.2.1",
				GeoError:      "resolve node.example-vpn.net: no such host",
				IfconfigIP:    "192.0.2.1",
				IfconfigError: "resolve node.example-vpn.net: no such host",
			},
			want: "",
		},
		{
			name:   "no server",
			record: NodeRecord{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ipInfoURL(tt.record); got != tt.want {
				t.Fatalf("ipInfoURL() = %q, want %q", got, tt.want)
			}
		})
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
