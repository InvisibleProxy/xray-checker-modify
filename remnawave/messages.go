package remnawave

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	messageTokenLocation    = "{location}"
	messageTokenLocations   = "{locations}"
	messageTokenUnavailable = "{unavailable}"
	messageTokenTotal       = "{total}"

	defaultSingleLocationTemplate    = "⚠️ Временно недоступна локация «{location}». Остальные доступные вам локации работают."
	defaultMultipleLocationsTemplate = "⚠️ Временно недоступны локации: {locations}. Остальные доступные вам локации работают."
	defaultAllLocationsTemplate      = "⚠️ Все доступные вам локации временно недоступны. Идёт восстановление работы."
	defaultHealthyTemplate           = "Всё стабильно"
	defaultPartialFallbackTemplate   = "⚠️ Временно недоступны несколько локаций. Остальные доступные вам локации работают."
)

type messageTemplateData struct {
	labels      []string
	unavailable int
	total       int
}

func defaultMessageScenarios() MessageScenarios {
	return MessageScenarios{
		SingleLocation: MessageScenario{
			Enabled:  true,
			Template: defaultSingleLocationTemplate,
		},
		MultipleLocations: MessageScenario{
			Enabled:  true,
			Template: defaultMultipleLocationsTemplate,
		},
		AllLocations: MessageScenario{
			Enabled:  true,
			Template: defaultAllLocationsTemplate,
		},
		Healthy: MessageScenario{
			Enabled:  false,
			Template: defaultHealthyTemplate,
		},
		PartialFallback: defaultPartialFallbackTemplate,
	}
}

func messageScenariosMissing(messages MessageScenarios) bool {
	return strings.TrimSpace(messages.SingleLocation.Template) == "" &&
		strings.TrimSpace(messages.MultipleLocations.Template) == "" &&
		strings.TrimSpace(messages.AllLocations.Template) == "" &&
		strings.TrimSpace(messages.Healthy.Template) == "" &&
		strings.TrimSpace(messages.PartialFallback) == ""
}

func normalizeMessageScenarios(messages *MessageScenarios) {
	defaults := defaultMessageScenarios()
	normalizeScenario := func(scenario *MessageScenario, fallback MessageScenario) {
		scenario.Template = strings.TrimSpace(scenario.Template)
		if scenario.Template == "" {
			*scenario = fallback
		}
	}
	normalizeScenario(&messages.SingleLocation, defaults.SingleLocation)
	normalizeScenario(&messages.MultipleLocations, defaults.MultipleLocations)
	normalizeScenario(&messages.AllLocations, defaults.AllLocations)
	normalizeScenario(&messages.Healthy, defaults.Healthy)
	messages.PartialFallback = strings.TrimSpace(messages.PartialFallback)
	if messages.PartialFallback == "" {
		messages.PartialFallback = defaults.PartialFallback
	}
}

func validateMessageScenarios(messages MessageScenarios) error {
	checks := []struct {
		name    string
		value   string
		allowed []string
	}{
		{"messages.singleLocation.template", messages.SingleLocation.Template, []string{messageTokenLocation, messageTokenUnavailable, messageTokenTotal}},
		{"messages.multipleLocations.template", messages.MultipleLocations.Template, []string{messageTokenLocations, messageTokenUnavailable, messageTokenTotal}},
		{"messages.allLocations.template", messages.AllLocations.Template, []string{messageTokenUnavailable, messageTokenTotal}},
		{"messages.healthy.template", messages.Healthy.Template, []string{messageTokenUnavailable, messageTokenTotal}},
		{"messages.partialFallback", messages.PartialFallback, []string{messageTokenUnavailable, messageTokenTotal}},
	}
	for _, check := range checks {
		if err := validateMessageTemplate(check.name, check.value, check.allowed); err != nil {
			return err
		}
	}
	return nil
}

func validateMessageTemplate(name, value string, allowed []string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if err := validateDisplayText(name, value, maxMessageRunes); err != nil {
		return err
	}
	remaining := value
	for _, token := range allowed {
		remaining = strings.ReplaceAll(remaining, token, "")
	}
	if strings.ContainsAny(remaining, "{}") {
		return fmt.Errorf("%s contains an unknown or unsupported placeholder", name)
	}
	return nil
}

func outageMessage(messages MessageScenarios, groups map[string]string, totalGroups int) (string, error) {
	labels := make([]string, 0, len(groups))
	for _, label := range groups {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	data := messageTemplateData{labels: labels, unavailable: len(labels), total: totalGroups}

	if totalGroups > 0 && len(groups) == totalGroups {
		if !messages.AllLocations.Enabled {
			return "", nil
		}
		return renderMessageTemplate("all-locations announce", messages.AllLocations.Template, data)
	}
	if len(labels) == 1 {
		if !messages.SingleLocation.Enabled {
			return "", nil
		}
		return renderMessageTemplate("single-location announce", messages.SingleLocation.Template, data)
	}
	if !messages.MultipleLocations.Enabled {
		return "", nil
	}
	if len(labels) <= 3 {
		if message, err := renderMessageTemplate("multiple-locations announce", messages.MultipleLocations.Template, data); err == nil {
			return message, nil
		}
	}
	return renderMessageTemplate("partial-outage fallback announce", messages.PartialFallback, data)
}

func healthyMessage(messages MessageScenarios, totalGroups int) (string, error) {
	if !messages.Healthy.Enabled {
		return "", nil
	}
	return renderMessageTemplate("healthy announce", messages.Healthy.Template, messageTemplateData{total: totalGroups})
}

func renderMessageTemplate(name, template string, data messageTemplateData) (string, error) {
	location := ""
	if len(data.labels) > 0 {
		location = data.labels[0]
	}
	quoted := make([]string, 0, len(data.labels))
	for _, label := range data.labels {
		quoted = append(quoted, "«"+label+"»")
	}
	message := strings.NewReplacer(
		messageTokenLocation, location,
		messageTokenLocations, strings.Join(quoted, ", "),
		messageTokenUnavailable, strconv.Itoa(data.unavailable),
		messageTokenTotal, strconv.Itoa(data.total),
	).Replace(template)
	if err := validateDisplayText(name, message, maxMessageRunes); err != nil {
		return "", err
	}
	return message, nil
}
