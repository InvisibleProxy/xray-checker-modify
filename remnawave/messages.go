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
	messageTokenAffected    = "{affected}"
	messageTokenTotal       = "{total}"

	defaultSingleLocationTemplate               = "⚠️ Временно недоступна локация «{location}». Остальные доступные вам локации работают."
	defaultMultipleLocationsTemplate            = "⚠️ Временно недоступны локации: {locations}. Остальные доступные вам локации работают."
	defaultAllLocationsTemplate                 = "⚠️ Все доступные вам локации временно недоступны. Идёт восстановление работы."
	defaultPartialSingleLocationTemplate        = "⚠️ Часть серверов локации «{location}» временно недоступна. Если подключение не работает, выберите другой сервер этой локации."
	defaultPartialMultipleLocationsTemplate     = "⚠️ Часть серверов временно недоступна в локациях: {locations}. Если подключение не работает, выберите другой сервер."
	defaultMaintenanceSingleLocationTemplate    = "🔧 Серверы локации «{location}» находятся на обслуживании. Могут наблюдаться проблемы в работе."
	defaultMaintenanceMultipleLocationsTemplate = "🔧 Серверы локаций {locations} находятся на обслуживании. Могут наблюдаться проблемы в работе."
	defaultHealthyTemplate                      = "Всё стабильно"
	defaultPartialFallbackTemplate              = "⚠️ Временно недоступны несколько локаций. Остальные доступные вам локации работают."
	defaultPartialAvailabilityFallbackTemplate  = "⚠️ Часть серверов в нескольких локациях временно недоступна. Если подключение не работает, выберите другой сервер."
	defaultMaintenanceFallbackTemplate          = "🔧 Серверы в {affected} локациях находятся на обслуживании. Могут наблюдаться проблемы в работе."
	defaultMaintenanceMixedFallbackTemplate     = "⚠️ Недоступны {unavailable} локаций; ещё {affected} локаций находятся на обслуживании. Могут наблюдаться проблемы в работе."
)

type messageTemplateData struct {
	labels      []string
	unavailable int
	affected    int
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
		PartialSingleLocation: MessageScenario{
			Enabled:  true,
			Template: defaultPartialSingleLocationTemplate,
		},
		PartialMultipleLocations: MessageScenario{
			Enabled:  true,
			Template: defaultPartialMultipleLocationsTemplate,
		},
		MaintenanceSingleLocation: MessageScenario{
			Enabled:  true,
			Template: defaultMaintenanceSingleLocationTemplate,
		},
		MaintenanceMultipleLocations: MessageScenario{
			Enabled:  true,
			Template: defaultMaintenanceMultipleLocationsTemplate,
		},
		Healthy: MessageScenario{
			Enabled:  false,
			Template: defaultHealthyTemplate,
		},
		PartialFallback:             defaultPartialFallbackTemplate,
		PartialAvailabilityFallback: defaultPartialAvailabilityFallbackTemplate,
		MaintenanceFallback:         defaultMaintenanceFallbackTemplate,
		MaintenanceMixedFallback:    defaultMaintenanceMixedFallbackTemplate,
	}
}

func messageScenariosMissing(messages MessageScenarios) bool {
	return strings.TrimSpace(messages.SingleLocation.Template) == "" &&
		strings.TrimSpace(messages.MultipleLocations.Template) == "" &&
		strings.TrimSpace(messages.AllLocations.Template) == "" &&
		strings.TrimSpace(messages.PartialSingleLocation.Template) == "" &&
		strings.TrimSpace(messages.PartialMultipleLocations.Template) == "" &&
		strings.TrimSpace(messages.MaintenanceSingleLocation.Template) == "" &&
		strings.TrimSpace(messages.MaintenanceMultipleLocations.Template) == "" &&
		strings.TrimSpace(messages.Healthy.Template) == "" &&
		strings.TrimSpace(messages.PartialFallback) == "" &&
		strings.TrimSpace(messages.PartialAvailabilityFallback) == "" &&
		strings.TrimSpace(messages.MaintenanceFallback) == "" &&
		strings.TrimSpace(messages.MaintenanceMixedFallback) == ""
}

func partialAvailabilityScenariosMissing(messages MessageScenarios) bool {
	return strings.TrimSpace(messages.PartialSingleLocation.Template) == "" &&
		strings.TrimSpace(messages.PartialMultipleLocations.Template) == "" &&
		strings.TrimSpace(messages.PartialAvailabilityFallback) == ""
}

func maintenanceScenariosMissing(messages MessageScenarios) bool {
	return strings.TrimSpace(messages.MaintenanceSingleLocation.Template) == "" &&
		strings.TrimSpace(messages.MaintenanceMultipleLocations.Template) == "" &&
		strings.TrimSpace(messages.MaintenanceFallback) == "" &&
		strings.TrimSpace(messages.MaintenanceMixedFallback) == ""
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
	normalizeScenario(&messages.PartialSingleLocation, defaults.PartialSingleLocation)
	normalizeScenario(&messages.PartialMultipleLocations, defaults.PartialMultipleLocations)
	normalizeScenario(&messages.MaintenanceSingleLocation, defaults.MaintenanceSingleLocation)
	normalizeScenario(&messages.MaintenanceMultipleLocations, defaults.MaintenanceMultipleLocations)
	normalizeScenario(&messages.Healthy, defaults.Healthy)
	messages.PartialFallback = strings.TrimSpace(messages.PartialFallback)
	if messages.PartialFallback == "" {
		messages.PartialFallback = defaults.PartialFallback
	}
	messages.PartialAvailabilityFallback = strings.TrimSpace(messages.PartialAvailabilityFallback)
	if messages.PartialAvailabilityFallback == "" {
		messages.PartialAvailabilityFallback = defaults.PartialAvailabilityFallback
	}
	messages.MaintenanceFallback = strings.TrimSpace(messages.MaintenanceFallback)
	if messages.MaintenanceFallback == "" {
		messages.MaintenanceFallback = defaults.MaintenanceFallback
	}
	messages.MaintenanceMixedFallback = strings.TrimSpace(messages.MaintenanceMixedFallback)
	if messages.MaintenanceMixedFallback == "" {
		messages.MaintenanceMixedFallback = defaults.MaintenanceMixedFallback
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
		{"messages.partialSingleLocation.template", messages.PartialSingleLocation.Template, []string{messageTokenLocation, messageTokenAffected, messageTokenTotal}},
		{"messages.partialMultipleLocations.template", messages.PartialMultipleLocations.Template, []string{messageTokenLocations, messageTokenAffected, messageTokenTotal}},
		{"messages.maintenanceSingleLocation.template", messages.MaintenanceSingleLocation.Template, []string{messageTokenLocation, messageTokenAffected, messageTokenTotal}},
		{"messages.maintenanceMultipleLocations.template", messages.MaintenanceMultipleLocations.Template, []string{messageTokenLocations, messageTokenAffected, messageTokenTotal}},
		{"messages.healthy.template", messages.Healthy.Template, []string{messageTokenUnavailable, messageTokenTotal}},
		{"messages.partialFallback", messages.PartialFallback, []string{messageTokenUnavailable, messageTokenTotal}},
		{"messages.partialAvailabilityFallback", messages.PartialAvailabilityFallback, []string{messageTokenAffected, messageTokenTotal}},
		{"messages.maintenanceFallback", messages.MaintenanceFallback, []string{messageTokenAffected, messageTokenTotal}},
		{"messages.maintenanceMixedFallback", messages.MaintenanceMixedFallback, []string{messageTokenUnavailable, messageTokenAffected, messageTokenTotal}},
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

func partialAvailabilityMessage(messages MessageScenarios, groups map[string]string, totalGroups int) (string, error) {
	labels := make([]string, 0, len(groups))
	for _, label := range groups {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	data := messageTemplateData{labels: labels, affected: len(labels), total: totalGroups}

	if len(labels) == 1 {
		if !messages.PartialSingleLocation.Enabled {
			return "", nil
		}
		return renderMessageTemplate("partial single-location announce", messages.PartialSingleLocation.Template, data)
	}
	if !messages.PartialMultipleLocations.Enabled {
		return "", nil
	}
	if len(labels) <= 3 {
		if message, err := renderMessageTemplate("partial multiple-locations announce", messages.PartialMultipleLocations.Template, data); err == nil {
			return message, nil
		}
	}
	return renderMessageTemplate("partial-availability fallback announce", messages.PartialAvailabilityFallback, data)
}

func maintenanceMessage(messages MessageScenarios, groups map[string]string, totalGroups int) (string, error) {
	labels := make([]string, 0, len(groups))
	for _, label := range groups {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	data := messageTemplateData{labels: labels, affected: len(labels), total: totalGroups}

	if len(labels) == 1 {
		if !messages.MaintenanceSingleLocation.Enabled {
			return "", nil
		}
		return renderMessageTemplate("maintenance single-location announce", messages.MaintenanceSingleLocation.Template, data)
	}
	if !messages.MaintenanceMultipleLocations.Enabled {
		return "", nil
	}
	if len(labels) <= 3 {
		if message, err := renderMessageTemplate("maintenance multiple-locations announce", messages.MaintenanceMultipleLocations.Template, data); err == nil {
			return message, nil
		}
	}
	return renderMessageTemplate("maintenance fallback announce", messages.MaintenanceFallback, data)
}

func combineOutageAndMaintenanceMessages(messages MessageScenarios, outage, maintenance string, unavailable, affected, total int) (string, error) {
	if outage == "" {
		return maintenance, nil
	}
	if maintenance == "" {
		return outage, nil
	}
	combined := outage + " " + maintenance
	if validateDisplayText("combined outage and maintenance announce", combined, maxMessageRunes) == nil {
		return combined, nil
	}
	return renderMessageTemplate("mixed outage and maintenance fallback announce", messages.MaintenanceMixedFallback, messageTemplateData{
		unavailable: unavailable,
		affected:    affected,
		total:       total,
	})
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
		messageTokenAffected, strconv.Itoa(data.affected),
		messageTokenTotal, strconv.Itoa(data.total),
	).Replace(template)
	if err := validateDisplayText(name, message, maxMessageRunes); err != nil {
		return "", err
	}
	return message, nil
}
