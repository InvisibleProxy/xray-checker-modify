package web

import (
	"bytes"
	"strings"
	"testing"
)

// A Remnawave XRAY_JSON feed names the balancer group each node belongs to, and the
// dashboard renders those as collapsible sections. Feeds without groups must keep
// rendering the flat list, so the group markup has to survive an empty GroupName.
func TestRenderIndexEmitsGroupNames(t *testing.T) {
	data := PageData{
		Version:       "test",
		CheckInterval: 300,
		Endpoints: []EndpointInfo{
			{Name: "NL core", GroupName: "🇳🇱 Нидерланды", StableID: "aaa1", AvailabilityStatus: "online"},
			{Name: "NL xHTTP", GroupName: "🇳🇱 Нидерланды", StableID: "aaa2", AvailabilityStatus: "online"},
			{Name: "DE core", GroupName: "", StableID: "bbb1", AvailabilityStatus: "offline"},
		},
	}

	var buf bytes.Buffer
	if err := RenderIndex(&buf, data); err != nil {
		t.Fatalf("RenderIndex() error = %v", err)
	}
	page := buf.String()

	if !strings.Contains(page, "groupName:") {
		t.Fatal("rendered page carries no groupName field")
	}
	if strings.Count(page, "groupName:") != len(data.Endpoints) {
		t.Errorf("groupName appears %d times, want %d", strings.Count(page, "groupName:"), len(data.Endpoints))
	}
	if !strings.Contains(page, "groupedProxies") {
		t.Error("rendered page has no grouped rendering")
	}
	if !strings.Contains(page, "toggleGroup(group)") {
		t.Error("rendered page has no group toggle")
	}
	// An empty group must still render, headerless, so ungrouped feeds are unchanged.
	if !strings.Contains(page, `groupName: ""`) {
		t.Error("an endpoint without a group did not render an empty groupName")
	}
}
