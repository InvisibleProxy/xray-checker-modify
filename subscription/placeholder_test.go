package subscription

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xray-checker/models"
)

func TestPlaceholderServersAreRecognisedByAddress(t *testing.T) {
	// The notice text differs per panel and per language; the address does not.
	placeholders := []string{
		"203.0.113.1",      // TEST-NET-3, what an exhausted device limit returns
		"192.0.2.7",        // TEST-NET-1
		"198.51.100.42",    // TEST-NET-2
		"2001:db8::1",      // IPv6 documentation range
		"example.com",      // RFC 2606
		"sub.example.net",  // subdomain of a reserved name
		"whatever.invalid", // RFC 6761
	}
	for _, server := range placeholders {
		if !IsPlaceholderServer(server) {
			t.Fatalf("%q should be recognised as a placeholder", server)
		}
	}

	real := []string{
		"198.18.7.42",
		"edge.node-provider.net",
		"api.node-provider.net",
		"2606:4700::1111",
		// Names that merely look like the reserved ones must survive.
		"exampleeee.com",
		"my-example.com",
	}
	for _, server := range real {
		if IsPlaceholderServer(server) {
			t.Fatalf("%q is a usable address and must be kept", server)
		}
	}
}

func TestPlaceholderNodesAreDroppedFromASubscription(t *testing.T) {
	configs := []*models.ProxyConfig{
		{Name: "Real", Server: "node.example-vpn.net", Port: 443},
		{Name: "Лимит устройств исчерпан", Server: "203.0.113.1", Port: 443},
		{Name: "📱 Наш телеграм", Server: "203.0.113.1", Port: 443},
	}

	kept, dropped := dropPlaceholderNodes(configs)
	if len(kept) != 1 || kept[0].Name != "Real" {
		t.Fatalf("kept = %+v, want only the real node", kept)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped = %d, want both notices", len(dropped))
	}
}

// A subscription made entirely of notices is not an empty subscription — it is
// a refusal, and saying so is what lets an operator act on it.
func TestSubscriptionOfOnlyPlaceholdersIsReportedPlainly(t *testing.T) {
	body := []any{
		map[string]any{
			"remarks": "Example VPN",
			"outbounds": []any{
				map[string]any{
					"tag":      "Лимит устройств исчерпан",
					"protocol": "vless",
					"settings": map[string]any{
						"vnext": []any{map[string]any{
							"address": "203.0.113.1",
							"port":    443,
							"users":   []any{map[string]any{"id": "uuid", "encryption": "none"}},
						}},
					},
					"streamSettings": map[string]any{"network": "tcp", "security": "reality"},
				},
			},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()

	_, _, err := ReadFromFeed(Feed{URL: server.URL, Profile: ClientProfile{Profile: ClientProfileHapp}})
	if err == nil {
		t.Fatal("expected an error naming the placeholders")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("error should explain what came back, got %v", err)
	}
	if !strings.Contains(err.Error(), "device limit") {
		t.Fatalf("error should point at the likely cause, got %v", err)
	}
}
