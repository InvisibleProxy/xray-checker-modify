package web

import (
	"bytes"
	"strings"
	"testing"
)

// The admin page has to keep a shortened transfer readable: the rate it
// measured stays on screen, the shortfall is spelled out where the raw
// transport error used to be, and a probe can be copied out as text.
func TestAdminTemplateReportsShortenedTransfersAndCopiesProbes(t *testing.T) {
	var admin bytes.Buffer
	if err := RenderAdmin(&admin); err != nil {
		t.Fatalf("RenderAdmin() error = %v", err)
	}
	rendered := admin.String()

	for _, marker := range []string{
		`function isShortenedResult(result)`,
		`function formatShortfall(result)`,
		`function formatTimeoutReason(result)`,
		`function agentProbeReportText(result, probe)`,
		`id="copy-agent-probe"`,
		`state.agentProbeDialog = { result, probe };`,
		`copyToClipboard(agentProbeReportText(context.result, context.probe))`,
		// Binary sizes are named for what they are, so the size beside a rate
		// in decimal Mbps divides into it.
		"${(bytes / mb).toFixed(1)} MiB",
	} {
		if !strings.Contains(rendered, marker) {
			t.Fatalf("admin template does not contain %q", marker)
		}
	}

	// The copy has to be built from the same facts the dialog renders, not from
	// its markup, so both read the same a week later in a ticket.
	for _, shared := range []string{
		`agentProbeFactsHTML(agentProbeTaskFacts(task))`,
		`agentProbeFactsHTML(agentProbeObservationFacts(observation))`,
		`agentProbeObservationEvidence(observation).map(evidenceChipHTML).join("")`,
	} {
		if !strings.Contains(rendered, shared) {
			t.Fatalf("admin template does not share probe facts with the copy: %q missing", shared)
		}
	}

	asset, err := staticFiles.ReadFile("static/localization.js")
	if err != nil {
		t.Fatalf("read localization asset: %v", err)
	}
	localization := string(asset)
	for _, marker := range []string{
		`"Timed out": "Таймаут"`,
		`"Agent probe copied!": "Проба агента скопирована!"`,
		`"Copy the probe as text": "Скопировать пробу текстом"`,
		`"Max MiB": "Макс. МиБ"`,
	} {
		if !strings.Contains(localization, marker) {
			t.Fatalf("localization asset does not contain %q", marker)
		}
	}
}
