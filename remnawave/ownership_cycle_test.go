package remnawave

import (
	"context"
	"strings"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
)

// The operator text of the Standart and Extand squads: multi-line, so it passes
// none of the rules for taking over a foreign announce.
const (
	cycleHealthy = "Все сервера работают стабильно ✅"
	cycleBase    = "rwEncodeBase64:{{USERNAME}} | Нажми, чтобы перейти в кабинет →\n\nНажми 🔄️, если сервер недоступен\n"
)

type fakeConflictNotifier struct {
	conflicts []string
	resolved  []string
	since     []time.Time
}

func (f *fakeConflictNotifier) AnnounceConflict(squadName, reason string, since time.Time) {
	f.conflicts = append(f.conflicts, squadName+": "+reason)
	f.since = append(f.since, since)
}

func (f *fakeConflictNotifier) AnnounceConflictResolved(squadName string, _ time.Time) {
	f.resolved = append(f.resolved, squadName)
}

type cycleFixture struct {
	t       *testing.T
	now     *time.Time
	service *Service
	api     *fakeAPI
	proxies *fakeProxySource
}

// Two locations, one squad: enough to reach the pass where the checker has no
// status to show - Germany is back, the Netherlands has only just started
// failing - and withdraws its line.
func newCycleFixture(t *testing.T, announce string) *cycleFixture {
	t.Helper()
	now := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	headers := map[string]string{}
	if announce != "" {
		headers[announceHeader] = announce
	}
	api := &fakeAPI{
		hosts: []Host{
			{UUID: "host-de", Remark: "Германия", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-users"}},
			{UUID: "host-nl", Remark: "Нидерланды", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-users"}},
		},
		internal: []InternalSquad{{UUID: "internal-users", Name: "Standart EU", Inbounds: []InternalInbound{{UUID: "inbound-users"}}}},
		external: []ExternalSquad{{UUID: "external-users", Name: "Standart", ResponseHeadersAdd: headers}},
	}
	proxies := &fakeProxySource{
		proxies: []*models.ProxyConfig{
			{StableID: "stable-de", Name: "DE", Server: "10.0.0.1"},
			{StableID: "stable-nl", Name: "NL", Server: "10.0.0.2"},
		},
		statuses: map[string]checker.ProxyStatusDetails{},
	}
	proxies.setOnline("stable-de", "stable-nl")
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	config := audienceConfig(cycleHealthy)
	config.Locations = map[string]AnnounceLocation{
		"de": {PublicLabel: "Германия", Members: map[string]string{"stable-de": "host-de"}},
		"nl": {PublicLabel: "Нидерланды", Members: map[string]string{"stable-nl": "host-nl"}},
	}
	service.config = config
	if _, err := service.SyncNow(context.Background()); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	return &cycleFixture{t: t, now: &now, service: service, api: api, proxies: proxies}
}

func (f *cycleFixture) reconcile() Snapshot {
	f.t.Helper()
	snapshot, err := f.service.ReconcileNow(context.Background())
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return snapshot
}

func (f *cycleFixture) announce() string {
	f.api.mu.Lock()
	defer f.api.mu.Unlock()
	return f.api.external[0].ResponseHeadersAdd[announceHeader]
}

func (f *cycleFixture) managed() bool {
	_, managed := f.service.runtime.Managed["external-users"]
	return managed
}

// germanyOutage publishes an outage and lets it recover up to the pass where
// the checker withdraws its status.
func (f *cycleFixture) germanyOutageUntilWithdrawn() {
	f.t.Helper()
	f.proxies.setProxyFailure(f.now.Add(-20*time.Minute), "stable-de")
	for range 3 {
		f.service.ObserveFullCheck()
	}
	f.reconcile()
	if !strings.Contains(f.announce(), "«Германия»") {
		f.t.Fatalf("outage was not published: %q", f.announce())
	}
	f.proxies.setOnline("stable-de")
	f.reconcile()
	*f.now = f.now.Add(6 * time.Minute)
	f.proxies.setProxyFailure(f.now.Add(-time.Minute), "stable-nl")
	f.reconcile()
	if f.managed() {
		f.t.Fatalf("the checker kept its status while Netherlands was only pending: %q", f.announce())
	}
}

func TestServiceTakesBackTheBaseItRestored(t *testing.T) {
	f := newCycleFixture(t, cycleBase+"\n"+cycleHealthy)
	if !f.managed() {
		t.Fatal("the checker did not take over the known healthy suffix")
	}

	f.germanyOutageUntilWithdrawn()
	if f.announce() != cycleBase {
		t.Fatalf("withdrawn status did not restore the base exactly: %q", f.announce())
	}
	if f.service.runtime.Restored["external-users"] != cycleBase {
		t.Fatalf("the restored base was not remembered: %+v", f.service.runtime.Restored)
	}

	f.proxies.setOnline("stable-nl")
	snapshot := f.reconcile()
	if len(snapshot.Status.Conflicts) != 0 {
		t.Fatalf("the checker refused the base it restored itself: %v", snapshot.Status.Conflicts)
	}
	if f.announce() != cycleBase+"\n"+cycleHealthy || !f.managed() {
		t.Fatalf("status was not appended to the restored base: %q", f.announce())
	}
	if _, remembered := f.service.runtime.Restored["external-users"]; remembered {
		t.Fatal("the restoration is still remembered after the checker owns the squad again")
	}

	persisted, err := readRuntimeFile(f.service.runtimePath)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if len(persisted.Restored) != 0 || persisted.Managed["external-users"].BaseValue != cycleBase {
		t.Fatalf("persisted runtime = %+v", persisted)
	}
}

// Switching automatic announce off hands every squad back its base. Switching
// it on again must not find those squads locked.
func TestServiceTakesBackRestoredBasesAfterPolicyToggle(t *testing.T) {
	f := newCycleFixture(t, cycleBase+"\n"+cycleHealthy)
	f.service.config.Policy.Enabled = false
	f.reconcile()
	if f.announce() != cycleBase || f.managed() {
		t.Fatalf("disabling the policy did not restore the base: %q", f.announce())
	}

	f.service.config.Policy.Enabled = true
	snapshot := f.reconcile()
	if len(snapshot.Status.Conflicts) != 0 || f.announce() != cycleBase+"\n"+cycleHealthy {
		t.Fatalf("re-enabled policy: announce=%q conflicts=%v", f.announce(), snapshot.Status.Conflicts)
	}
}

// Remembering a restoration is not a licence to take over whatever the panel
// holds later: an operator edit is foreign text again.
func TestServiceForgetsRestoredBaseEditedInPanel(t *testing.T) {
	f := newCycleFixture(t, cycleBase+"\n"+cycleHealthy)
	f.germanyOutageUntilWithdrawn()

	edited := cycleBase + "Новая строка оператора"
	f.api.mu.Lock()
	f.api.external[0].ResponseHeadersAdd[announceHeader] = edited
	f.api.mu.Unlock()
	f.proxies.setOnline("stable-nl")
	snapshot := f.reconcile()
	if len(snapshot.Status.Conflicts) != 1 || f.announce() != edited || f.managed() {
		t.Fatalf("edited text: announce=%q conflicts=%v", f.announce(), snapshot.Status.Conflicts)
	}
	if _, remembered := f.service.runtime.Restored["external-users"]; remembered {
		t.Fatal("a restoration that no longer matches the panel is still remembered")
	}
}

// What happened to the production squads: Adopt pressed while the checker's
// status line was on stored that line inside the base.
func TestAdoptWhileManagedTakesOnlyTheOperatorText(t *testing.T) {
	f := newCycleFixture(t, cycleBase+"\n"+cycleHealthy)
	if _, err := f.service.AdoptAnnounceBase("external-users", false); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if got := f.service.config.AnnounceBases["external-users"]; got != cycleBase {
		t.Fatalf("adopted base = %q, want the text before the checker status", got)
	}

	f.germanyOutageUntilWithdrawn()
	delete(f.service.runtime.Restored, "external-users") // leave only the adopted base to rely on
	f.proxies.setOnline("stable-nl")
	snapshot := f.reconcile()
	if len(snapshot.Status.Conflicts) != 0 || f.announce() != cycleBase+"\n"+cycleHealthy {
		t.Fatalf("adopted base: announce=%q conflicts=%v", f.announce(), snapshot.Status.Conflicts)
	}
}

func TestAdoptRefusesAnnounceTheCheckerWroteWhole(t *testing.T) {
	f := newCycleFixture(t, "")
	f.proxies.setProxyFailure(f.now.Add(-20*time.Minute), "stable-de")
	for range 3 {
		f.service.ObserveFullCheck()
	}
	snapshot := f.reconcile()
	if !f.managed() || f.service.runtime.Managed["external-users"].BasePresent {
		t.Fatalf("the checker did not create the announce itself: %q", f.announce())
	}
	if len(snapshot.Status.Announcements) != 1 || snapshot.Status.Announcements[0].Adoptable {
		t.Fatalf("a checker-written announce is offered for adoption: %+v", snapshot.Status.Announcements)
	}
	if _, err := f.service.AdoptAnnounceBase("external-users", false); err == nil {
		t.Fatal("adopted an announce the checker wrote whole")
	}
}

func TestConflictIsReportedOnceAfterDelayAndOnResolution(t *testing.T) {
	foreign := "rwEncodeBase64:чужой текст\nвторая строка"
	f := newCycleFixture(t, foreign)
	notifier := &fakeConflictNotifier{}
	f.service.SetConflictNotifier(notifier)
	startedAt := *f.now

	if snapshot := f.reconcile(); len(snapshot.Status.Conflicts) != 1 || len(notifier.conflicts) != 0 {
		t.Fatalf("first pass: conflicts=%v notified=%v", snapshot.Status.Conflicts, notifier.conflicts)
	}

	// Nothing to show while a location is pending, so nothing is attempted and
	// the conflict drops out of the list. The squad is as blocked as before.
	*f.now = f.now.Add(5 * time.Minute)
	f.proxies.setProxyFailure(f.now.Add(-time.Minute), "stable-nl")
	if snapshot := f.reconcile(); len(snapshot.Status.Conflicts) != 0 || len(notifier.conflicts) != 0 {
		t.Fatalf("pending pass: conflicts=%v notified=%v", snapshot.Status.Conflicts, notifier.conflicts)
	}

	*f.now = f.now.Add(6 * time.Minute)
	f.proxies.setOnline("stable-nl")
	f.reconcile()
	if len(notifier.conflicts) != 1 || !notifier.since[0].Equal(startedAt) || !strings.HasPrefix(notifier.conflicts[0], "Standart: ") {
		t.Fatalf("after the delay: notified=%v since=%v", notifier.conflicts, notifier.since)
	}
	*f.now = f.now.Add(time.Minute)
	f.reconcile()
	if len(notifier.conflicts) != 1 {
		t.Fatalf("the same conflict was reported again: %v", notifier.conflicts)
	}

	if _, err := f.service.AdoptAnnounceBase("external-users", false); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	f.reconcile()
	if !f.managed() || len(notifier.resolved) != 1 {
		t.Fatalf("resolution: managed=%v resolved=%v", f.managed(), notifier.resolved)
	}
}

// Editing a managed announce in the panel drops ownership once; the next pass
// judges the new text on its own. That is not worth a message.
func TestManualEditConflictIsNotEscalated(t *testing.T) {
	f := newCycleFixture(t, cycleBase+"\n"+cycleHealthy)
	notifier := &fakeConflictNotifier{}
	f.service.SetConflictNotifier(notifier)

	f.api.mu.Lock()
	f.api.external[0].ResponseHeadersAdd[announceHeader] = "rwEncodeBase64:новый однострочный текст"
	f.api.mu.Unlock()
	if snapshot := f.reconcile(); len(snapshot.Status.Conflicts) != 1 {
		t.Fatalf("manual edit was not reported: %v", snapshot.Status.Conflicts)
	}
	*f.now = f.now.Add(15 * time.Minute)
	f.reconcile()
	if !f.managed() || len(notifier.conflicts) != 0 {
		t.Fatalf("after the edit: managed=%v notified=%v announce=%q", f.managed(), notifier.conflicts, f.announce())
	}
}

func TestRuntimeRestoredBasesRoundTripAndValidate(t *testing.T) {
	runtime, err := DecodeRuntime([]byte(`{"version":4,"managed":{},"restored":{"external-users":"rwEncodeBase64:база\nвторая строка"}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if runtime.Restored["external-users"] != "rwEncodeBase64:база\nвторая строка" {
		t.Fatalf("restored = %+v", runtime.Restored)
	}
	legacy, err := DecodeRuntime([]byte(`{"version":4,"managed":{}}`))
	if err != nil || legacy.Restored == nil {
		t.Fatalf("a runtime file written before restorations were remembered: %+v, %v", legacy, err)
	}
	if _, err := DecodeRuntime([]byte(`{"version":4,"managed":{},"restored":{"external-users":"plain text"}}`)); err == nil {
		t.Fatal("accepted a restored value without the base prefix")
	}
}
