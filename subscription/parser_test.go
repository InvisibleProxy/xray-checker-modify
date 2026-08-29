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
