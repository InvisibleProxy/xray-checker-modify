package telegram

import (
	"strings"
	"testing"
	"time"

	"xray-checker/pathquality"
	"xray-checker/probeagent"
	"xray-checker/speedtest"
	"xray-checker/verdictlog"
)

func TestDigestIsDueOnMondayMorningInTheOperatorsZone(t *testing.T) {
	krasnoyarsk := time.FixedZone("KRAT", 7*3600)
	for _, test := range []struct {
		now  time.Time
		want time.Time
	}{
		// Monday 10:30 local: this Monday's slot.
		{time.Date(2026, 9, 28, 10, 30, 0, 0, krasnoyarsk), time.Date(2026, 9, 28, 10, 0, 0, 0, krasnoyarsk)},
		// Monday 09:59 local: still last week's slot.
		{time.Date(2026, 9, 28, 9, 59, 0, 0, krasnoyarsk), time.Date(2026, 9, 21, 10, 0, 0, 0, krasnoyarsk)},
		// Thursday: the Monday before.
		{time.Date(2026, 10, 1, 18, 0, 0, 0, krasnoyarsk), time.Date(2026, 9, 28, 10, 0, 0, 0, krasnoyarsk)},
	} {
		if got := digestDueAt(test.now, krasnoyarsk); !got.Equal(test.want) {
			t.Errorf("digestDueAt(%s) = %s, want %s", test.now, got, test.want)
		}
	}
}

func digestTestReport() pathquality.Report {
	hours := func(bad, runs int) []pathquality.Cell {
		cells := make([]pathquality.Cell, 24)
		for hour := range cells {
			cells[hour].Hour = hour
		}
		cells[20] = pathquality.Cell{Hour: 20, Runs: runs, Bad: bad}
		return cells
	}
	fleet := make([]pathquality.Cell, 24)
	for hour := range fleet {
		fleet[hour].Hour = hour
	}
	fleet[20] = pathquality.Cell{Hour: 20, Runs: 20, Bad: 5}
	fleet[4] = pathquality.Cell{Hour: 4, Runs: 20, Bad: 0}
	return pathquality.Report{
		PeakZone: "Europe/Moscow", PeakStart: 18, PeakEnd: 24,
		Nodes: []pathquality.NodeReport{
			{
				Name: "Нидерланды #3 xHTTP", Hours: hours(4, 7), WorstHour: 20,
				Peak:     pathquality.Window{Runs: 7, Bad: 4, BadShare: 4.0 / 7, MedianMbps: 3},
				OffPeak:  pathquality.Window{Runs: 20, Bad: 2, BadShare: 0.1},
				Verdicts: map[string]int{speedtest.AgentDiagnosticPathLimited: 3, speedtest.AgentDiagnosticReproduced: 1},
			},
			{Name: "Финляндия", Hours: hours(0, 7), WorstHour: -1, Peak: pathquality.Window{Runs: 7}, OffPeak: pathquality.Window{Runs: 20}},
			{Name: "Мало данных", Hours: hours(1, 1), WorstHour: -1, Peak: pathquality.Window{Runs: 1, Bad: 1, BadShare: 1}},
		},
		Fleet: fleet,
	}
}

func TestDigestNamesTheWorstEveningsTheCleanNodesTheVerdictsAndTheAgents(t *testing.T) {
	service := &Service{}
	service.SetDigestSources(DigestSources{
		PathQuality: func(time.Time, time.Time) (pathquality.Report, bool) { return digestTestReport(), true },
		Verdicts: func(time.Time, time.Time) []verdictlog.Entry {
			return []verdictlog.Entry{
				{Source: verdictlog.SourceAvailability, Verdict: speedtest.AgentDiagnosticNotReproduced},
				{Source: verdictlog.SourceAvailability, Verdict: speedtest.AgentDiagnosticUnavailable},
				{Source: verdictlog.SourceSpeed, Verdict: speedtest.AgentDiagnosticReproduced},
			}
		},
		Agents: func() []probeagent.AgentSnapshot {
			return []probeagent.AgentSnapshot{
				{DisplayName: "DE-01", Enabled: true, Connected: true},
				{DisplayName: "RU-Krsk", Enabled: true, LastSeenAt: time.Date(2026, 9, 5, 3, 12, 0, 0, time.UTC)},
				{DisplayName: "old", Enabled: false, Revoked: true},
			}
		},
	})
	message, ok := service.buildDigestMessage(time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("digest reported nothing to say")
	}
	for _, want := range []string{
		"Качество пути за неделю", "Europe/Moscow",
		"Нидерланды #3 xHTTP", "57% плохих (4 из 7)", "вне пика 10%", "хуже всего в 20:00", "путь checker-а 3", "воспроизвели 1",
		"Без просадок в пик</b>: Финляндия",
		"20 — 25%", "04 — 0%",
		"у агента работает 1", "нет агента 1",
		"DE-01 на связи", "RU-Krsk — нет связи с",
	} {
		if !strings.Contains(message.HTML, want) {
			t.Errorf("digest is missing %q:\n%s", want, message.HTML)
		}
	}
	if strings.Contains(message.HTML, "Мало данных") || strings.Contains(message.HTML, "old") {
		t.Errorf("digest lists a node with one evening measurement or a revoked agent:\n%s", message.HTML)
	}
}

// The availability verdicts sit right under the speed verdicts, and stand apart
// when a week had no speed verdicts to sit under.
func TestDigestAvailabilityVerdictsFollowTheSpeedVerdictsOrStandApart(t *testing.T) {
	availability := func(time.Time, time.Time) []verdictlog.Entry {
		return []verdictlog.Entry{{Source: verdictlog.SourceAvailability, Verdict: speedtest.AgentDiagnosticReproduced}}
	}
	withSpeed := &Service{}
	withSpeed.SetDigestSources(DigestSources{
		PathQuality: func(time.Time, time.Time) (pathquality.Report, bool) { return digestTestReport(), true },
		Verdicts:    availability,
	})
	message, _ := withSpeed.buildDigestMessage(time.Now().Add(-digestPeriod), time.Now())
	if !strings.Contains(message.HTML, "воспроизвели 1\n<b>Пробы агентов по доступности</b>") {
		t.Errorf("availability verdicts are not right under the speed verdicts:\n%s", message.HTML)
	}

	report := digestTestReport()
	report.Nodes[0].Verdicts = nil
	withoutSpeed := &Service{}
	withoutSpeed.SetDigestSources(DigestSources{
		PathQuality: func(time.Time, time.Time) (pathquality.Report, bool) { return report, true },
		Verdicts:    availability,
	})
	message, _ = withoutSpeed.buildDigestMessage(time.Now().Add(-digestPeriod), time.Now())
	if strings.Contains(message.HTML, "Пробы агентов по скорости") || !strings.Contains(message.HTML, "\n\n<b>Пробы агентов по доступности</b>") {
		t.Errorf("availability verdicts without speed verdicts are not a section of their own:\n%s", message.HTML)
	}
}

// One digest per slot, none while switched off, and none for a week that ended
// days ago.
func TestDigestIsSentOncePerSlot(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 5, 0, 0, time.UTC)
	var sent int
	service := &Service{digestNow: func() time.Time { return now }}
	service.digestSendFunc = func(Config, formattedMessage) error { sent++; return nil }
	service.SetDigestSources(DigestSources{PathQuality: func(time.Time, time.Time) (pathquality.Report, bool) { return digestTestReport(), true }})
	cfg := DefaultConfig()
	cfg.Enabled, cfg.ChatID, cfg.BotToken, cfg.TimeZone = true, "chat", "token", "UTC"
	service.setConfig(cfg)

	if !service.sendDigestIfDue() || service.sendDigestIfDue() || sent != 1 {
		t.Fatalf("sent = %d, want exactly one digest for the slot", sent)
	}
	now = now.AddDate(0, 0, 7)
	cfg.WeeklyDigestEnabled = false
	service.setConfig(cfg)
	if service.sendDigestIfDue() {
		t.Fatal("a disabled digest was sent")
	}
	cfg.WeeklyDigestEnabled = true
	service.setConfig(cfg)
	now = now.Add(3 * 24 * time.Hour)
	if service.sendDigestIfDue() || sent != 1 {
		t.Fatalf("sent = %d, want a slot three days old skipped", sent)
	}
}

// An admin page from an older build does not know the setting and must not
// switch the digest off by leaving it out.
func TestAdminConfigWithoutTheDigestSettingKeepsIt(t *testing.T) {
	service := &Service{}
	cfg := DefaultConfig()
	service.setConfig(cfg)
	input := service.AdminConfig()
	input.WeeklyDigestEnabled = nil
	if err := service.UpdateAdminConfig(input); err != nil {
		t.Fatal(err)
	}
	if !service.Config().WeeklyDigestEnabled {
		t.Fatal("the digest was switched off by an update that did not mention it")
	}
	off := false
	input.WeeklyDigestEnabled = &off
	if err := service.UpdateAdminConfig(input); err != nil {
		t.Fatal(err)
	}
	if service.Config().WeeklyDigestEnabled {
		t.Fatal("an explicit false did not switch the digest off")
	}
}
