package subscription_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"testing"

	"xray-checker/subscription"
	"xray-checker/xray"
)

func TestXHTTPShareLinkExtraSurvivesGeneratedConfig(t *testing.T) {
	extra := `{"headers":{"Referer":"https://front.example.com/"},"xPaddingBytes":"100-1000","noGRPCHeader":true,"xmux":{"maxConcurrency":"16-32"}}`
	query := url.Values{
		"encryption": {"none"},
		"extra":      {extra},
		"fp":         {"chrome"},
		"host":       {"cdn.example.com"},
		"mode":       {"stream-one"},
		"path":       {"/xhttp"},
		"security":   {"tls"},
		"sni":        {"cdn.example.com"},
		"type":       {"xhttp"},
	}
	link := fmt.Sprintf(
		"vless://11111111-1111-4111-8111-111111111111@node.example.com:443?%s#xhttp-extra",
		query.Encode(),
	)

	parsed, err := subscription.NewParser().Parse(link)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.Configs) != 1 {
		t.Fatalf("Parse() configs = %d, want 1", len(parsed.Configs))
	}
	proxy := parsed.Configs[0]
	if proxy.Type != "xhttp" {
		t.Fatalf("parsed transport = %q, want xhttp", proxy.Type)
	}
	if proxy.RawXhttpSettings == "" {
		t.Fatal("parsed xHTTP settings are empty")
	}

	generated, err := xray.NewConfigGenerator().GenerateConfig(parsed.Configs, 10000, "warning")
	if err != nil {
		t.Fatalf("GenerateConfig() error = %v", err)
	}
	var config struct {
		Outbounds []struct {
			StreamSettings struct {
				Network       string         `json:"network"`
				XHTTPSettings map[string]any `json:"xhttpSettings"`
			} `json:"streamSettings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(generated, &config); err != nil {
		t.Fatalf("generated config is invalid JSON: %v", err)
	}

	var settings map[string]any
	for _, outbound := range config.Outbounds {
		if outbound.StreamSettings.Network == "xhttp" {
			settings = outbound.StreamSettings.XHTTPSettings
			break
		}
	}
	if settings == nil {
		t.Fatal("generated config has no xHTTP outbound settings")
	}
	if got := settings["path"]; got != "/xhttp" {
		t.Errorf("xhttpSettings.path = %#v, want /xhttp", got)
	}
	if got := settings["host"]; got != "cdn.example.com" {
		t.Errorf("xhttpSettings.host = %#v, want cdn.example.com", got)
	}
	if got := settings["mode"]; got != "stream-one" {
		t.Errorf("xhttpSettings.mode = %#v, want stream-one", got)
	}
	generatedExtra, ok := settings["extra"].(map[string]any)
	if !ok {
		t.Fatalf("xhttpSettings.extra = %#v, want object", settings["extra"])
	}
	if got := generatedExtra["xPaddingBytes"]; got != "100-1000" {
		t.Errorf("xhttpSettings.extra.xPaddingBytes = %#v, want 100-1000", got)
	}
	if got := generatedExtra["noGRPCHeader"]; got != true {
		t.Errorf("xhttpSettings.extra.noGRPCHeader = %#v, want true", got)
	}
	xmux, ok := generatedExtra["xmux"].(map[string]any)
	if !ok || xmux["maxConcurrency"] != "16-32" {
		t.Errorf("xhttpSettings.extra.xmux = %#v, want maxConcurrency 16-32", generatedExtra["xmux"])
	}
}

// remnawaveOutbound renders one vless/reality outbound the way Remnawave's
// XRAY_JSON generator emits it, with the outbound tag under the caller's control.
func remnawaveOutbound(tag, address string, port int, uuid string) string {
	return fmt.Sprintf(`{
		"tag": %q,
		"protocol": "vless",
		"settings": {"vnext": [{"address": %q, "port": %d, "users": [
			{"id": %q, "encryption": "none", "flow": "xtls-rprx-vision"}
		]}]},
		"streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {
			"serverName": "www.example.com", "fingerprint": "chrome",
			"publicKey": "3Wl5wRVDPBGDBEQpCX5hAWiTVLPRPRHBjBcJKvBBBBB", "shortId": "0123abcd"
		}}
	}`, tag, address, port, uuid)
}

const remnawaveServiceOutbounds = `{"tag": "direct", "protocol": "freedom"},
	{"tag": "block", "protocol": "blackhole"}`

// A balancer group (Remnawave injectHosts with useHostRemarkAsTag) puts several
// proxy outbounds under one "remarks". Each node must keep its own tag as the
// display name, with remarks carried separately as the group.
func TestRemnawaveBalancerGroupKeepsOutboundTags(t *testing.T) {
	sub := fmt.Sprintf(`[{"remarks": "NL", "outbounds": [%s, %s, %s, %s]}]`,
		remnawaveOutbound("NL core", "144.31.86.63", 8443, "11111111-1111-4111-8111-111111111111"),
		remnawaveOutbound("NL xHTTP", "144.31.86.63", 2096, "22222222-2222-4222-8222-222222222222"),
		remnawaveOutbound("NL #3", "138.124.3.225", 8443, "33333333-3333-4333-8333-333333333333"),
		remnawaveServiceOutbounds,
	)

	parsed, err := subscription.NewParser().Parse(sub)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.Configs) != 3 {
		t.Fatalf("Parse() configs = %d, want 3 (service outbounds must be skipped)", len(parsed.Configs))
	}

	wantNames := []string{"NL core", "NL xHTTP", "NL #3"}
	for i, proxy := range parsed.Configs {
		if proxy.Name != wantNames[i] {
			t.Errorf("Configs[%d].Name = %q, want %q", i, proxy.Name, wantNames[i])
		}
		if proxy.GroupName != "NL" {
			t.Errorf("Configs[%d].GroupName = %q, want %q", i, proxy.GroupName, "NL")
		}
	}

	if err := xray.ValidateStableIDs(parsed.Configs); err != nil {
		t.Fatalf("ValidateStableIDs() error = %v", err)
	}
}

// A plain single-host config carries a generated placeholder tag ("proxy"), so the
// operator-authored remarks must win and no group is reported.
func TestRemnawaveSingleHostConfigUsesRemarks(t *testing.T) {
	sub := fmt.Sprintf(`[{"remarks": "DE #2", "outbounds": [%s, %s]}]`,
		remnawaveOutbound("proxy", "31.76.38.179", 443, "44444444-4444-4444-8444-444444444444"),
		remnawaveServiceOutbounds,
	)

	parsed, err := subscription.NewParser().Parse(sub)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.Configs) != 1 {
		t.Fatalf("Parse() configs = %d, want 1", len(parsed.Configs))
	}
	if got := parsed.Configs[0].Name; got != "DE #2" {
		t.Errorf("Name = %q, want %q", got, "DE #2")
	}
	if got := parsed.Configs[0].GroupName; got != "" {
		t.Errorf("GroupName = %q, want empty for a single-node config", got)
	}
}

// Two hosts sharing a remark produce two identical outbound tags; the group must
// still name them apart.
func TestRemnawaveGroupDisambiguatesDuplicateTags(t *testing.T) {
	sub := fmt.Sprintf(`[{"remarks": "EU", "outbounds": [%s, %s]}]`,
		remnawaveOutbound("DE", "31.76.38.179", 443, "55555555-5555-4555-8555-555555555555"),
		remnawaveOutbound("DE", "83.219.249.142", 4443, "66666666-6666-4666-8666-666666666666"),
	)

	parsed, err := subscription.NewParser().Parse(sub)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.Configs) != 2 {
		t.Fatalf("Parse() configs = %d, want 2", len(parsed.Configs))
	}

	wantNames := []string{"DE (31.76.38.179:443)", "DE (83.219.249.142:4443)"}
	for i, proxy := range parsed.Configs {
		if proxy.Name != wantNames[i] {
			t.Errorf("Configs[%d].Name = %q, want %q", i, proxy.Name, wantNames[i])
		}
	}
}

// Several groups in one subscription must not bleed into each other.
func TestRemnawaveMultipleGroupsStayIndependent(t *testing.T) {
	sub := fmt.Sprintf(`[
		{"remarks": "NL", "outbounds": [%s, %s, %s]},
		{"remarks": "GE", "outbounds": [%s, %s]}
	]`,
		remnawaveOutbound("NL core", "144.31.86.63", 8443, "77777777-7777-4777-8777-777777777777"),
		remnawaveOutbound("NL xHTTP", "144.31.86.63", 2096, "88888888-8888-4888-8888-888888888888"),
		remnawaveServiceOutbounds,
		remnawaveOutbound("proxy", "31.76.38.179", 443, "99999999-9999-4999-8999-999999999999"),
		remnawaveServiceOutbounds,
	)

	parsed, err := subscription.NewParser().Parse(sub)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.Configs) != 3 {
		t.Fatalf("Parse() configs = %d, want 3", len(parsed.Configs))
	}

	type want struct{ name, group string }
	wants := []want{
		{"NL core", "NL"},
		{"NL xHTTP", "NL"},
		{"GE", ""},
	}
	for i, w := range wants {
		if parsed.Configs[i].Name != w.name {
			t.Errorf("Configs[%d].Name = %q, want %q", i, parsed.Configs[i].Name, w.name)
		}
		if parsed.Configs[i].GroupName != w.group {
			t.Errorf("Configs[%d].GroupName = %q, want %q", i, parsed.Configs[i].GroupName, w.group)
		}
	}
}
