package remnawave

import (
	"strings"
	"testing"
)

func TestOutageMessageUsesConfiguredScenarioTemplates(t *testing.T) {
	messages := defaultMessageScenarios()
	messages.SingleLocation.Template = "ONE {location} {unavailable}/{total}"
	messages.MultipleLocations.Template = "MANY {locations} {unavailable}/{total}"
	messages.AllLocations.Template = "ALL {unavailable}/{total}"
	messages.PartialFallback = "FALLBACK {unavailable}/{total}"

	tests := []struct {
		name   string
		groups map[string]string
		total  int
		want   string
	}{
		{"single", map[string]string{"de": "Германия"}, 3, "ONE Германия 1/3"},
		{"multiple", map[string]string{"nl": "Нидерланды", "de": "Германия"}, 3, "MANY «Германия», «Нидерланды» 2/3"},
		{"all", map[string]string{"de": "Германия", "nl": "Нидерланды"}, 2, "ALL 2/2"},
		{"fallback", map[string]string{"a": "A", "b": "B", "c": "C", "d": "D"}, 5, "FALLBACK 4/5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := outageMessage(messages, test.groups, test.total)
			if err != nil {
				t.Fatalf("outageMessage: %v", err)
			}
			if got != test.want {
				t.Fatalf("message = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOutageMessageHonorsDisabledScenario(t *testing.T) {
	messages := defaultMessageScenarios()
	messages.SingleLocation.Enabled = false
	got, err := outageMessage(messages, map[string]string{"de": "Германия"}, 3)
	if err != nil {
		t.Fatalf("outageMessage: %v", err)
	}
	if got != "" {
		t.Fatalf("disabled scenario message = %q", got)
	}
}

func TestOutageMessageUsesFallbackWhenRenderedListIsTooLong(t *testing.T) {
	messages := defaultMessageScenarios()
	messages.MultipleLocations.Template = strings.Repeat("x", 220) + "{locations}"
	messages.PartialFallback = "FALLBACK {unavailable}/{total}"
	groups := map[string]string{
		"a": strings.Repeat("A", 80),
		"b": strings.Repeat("B", 80),
	}
	got, err := outageMessage(messages, groups, 3)
	if err != nil {
		t.Fatalf("outageMessage: %v", err)
	}
	if got != "FALLBACK 2/3" {
		t.Fatalf("oversized list message = %q", got)
	}
}

func TestHealthyMessageUsesCounts(t *testing.T) {
	messages := defaultMessageScenarios()
	messages.Healthy = MessageScenario{Enabled: true, Template: "Стабильно: {unavailable}/{total}"}
	got, err := healthyMessage(messages, 3)
	if err != nil {
		t.Fatalf("healthyMessage: %v", err)
	}
	if got != "Стабильно: 0/3" {
		t.Fatalf("healthy message = %q", got)
	}
}
