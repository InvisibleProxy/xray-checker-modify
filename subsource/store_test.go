package subsource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xray-checker/observation"
	"xray-checker/subscription"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "subscription_sources.json"))
}

func TestAddGeneratesAStableHWIDForImpersonatedClients(t *testing.T) {
	store := newTestStore(t)

	added, err := store.Add(Source{
		URL:     "https://panel.example/sub/token",
		Enabled: true,
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileHapp},
	})
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	if added.Profile.HWID == "" {
		t.Fatal("a client profile that impersonates an app needs an HWID")
	}
	if err := subscription.ValidateHWID(added.Profile.HWID); err != nil {
		t.Fatalf("generated HWID is not one the panel accepts: %v", err)
	}

	// The remote panel ties a device slot to this value, so editing anything
	// else must not release it.
	updated, err := store.Update(added.ID, Source{
		URL:     added.URL,
		Name:    "Third-party panel",
		Enabled: true,
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileHapp},
	})
	if err != nil {
		t.Fatalf("update source: %v", err)
	}
	if updated.Profile.HWID != added.Profile.HWID {
		t.Fatalf("HWID changed on edit: %q -> %q", added.Profile.HWID, updated.Profile.HWID)
	}
}

// The checker's own profile keeps its fixed HWID: the operator's panel should
// see one device for the checker, not a new one per source.
func TestCheckerProfileKeepsItsFixedIdentity(t *testing.T) {
	store := newTestStore(t)

	added, err := store.Add(Source{
		URL:     "https://own.example/sub/token",
		Enabled: true,
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileChecker},
	})
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	if added.Profile.HWID != "" {
		t.Fatalf("checker profile must not get a generated HWID, got %q", added.Profile.HWID)
	}

	headers := added.Profile.Headers()
	if headers["User-Agent"] != "Xray-Checker" {
		t.Fatalf("checker profile must keep its own User-Agent, got %q", headers["User-Agent"])
	}
	if headers["X-Hwid"] == "" {
		t.Fatal("checker profile still sends its fixed HWID")
	}
}

func TestSourcesSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subscription_sources.json")
	store := NewStore(path)
	added, err := store.Add(Source{
		URL:     "https://panel.example/sub/token",
		Name:    "Third-party panel",
		Enabled: true,
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileINCY},
	})
	if err != nil {
		t.Fatalf("add source: %v", err)
	}

	restored := NewStore(path)
	if err := restored.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	sources := restored.List()
	if len(sources) != 1 {
		t.Fatalf("sources after restart = %d, want 1", len(sources))
	}
	if sources[0].Profile.HWID != added.Profile.HWID {
		t.Fatal("HWID must survive a restart, otherwise every restart claims a new device slot")
	}
	if sources[0].Profile.Profile != subscription.ClientProfileINCY {
		t.Fatalf("profile = %q, want incy", sources[0].Profile.Profile)
	}
}

func TestDisabledSourceIsNotFetched(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add(Source{
		URL:     "https://panel.example/a",
		Enabled: true,
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileHapp},
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := store.Add(Source{
		URL:     "https://panel.example/b",
		Enabled: false,
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileHapp},
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	enabled := store.EnabledSources()
	if len(enabled) != 1 || !strings.HasSuffix(enabled[0].URL, "/a") {
		t.Fatalf("enabled sources = %+v, want only the enabled one", enabled)
	}
	if len(store.List()) != 2 {
		t.Fatal("a disabled source is still listed in the panel")
	}
}

func TestRejectsInvalidInput(t *testing.T) {
	store := newTestStore(t)

	if _, err := store.Add(Source{URL: "not-a-url"}); err == nil {
		t.Fatal("a URL without a scheme must be rejected")
	}
	if _, err := store.Add(Source{
		URL:     "https://panel.example/sub",
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileHapp, HWID: "short"},
	}); err == nil {
		t.Fatal("an HWID the panel would reject must not be accepted")
	}
	if _, err := store.Add(Source{
		URL:     "https://panel.example/sub",
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileCustom},
	}); err == nil {
		t.Fatal("a custom profile without a User-Agent must be rejected")
	}

	if _, err := store.Add(Source{
		URL:     "https://panel.example/sub",
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileHapp},
	}); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	if _, err := store.Add(Source{
		URL:     "https://panel.example/sub",
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileHapp},
	}); err == nil {
		t.Fatal("the same URL must not be added twice")
	}
}

// An edit that does not retype the URL keeps the stored one: the API only ever
// shows a masked URL, so retyping it would mean pasting the token again.
func TestUpdateWithoutURLKeepsTheStoredOne(t *testing.T) {
	store := newTestStore(t)
	added, err := store.Add(Source{
		URL:     "https://panel.example/sub/secret-token",
		Enabled: true,
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileHapp},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	updated, err := store.Update(added.ID, Source{
		Name:    "Renamed",
		Enabled: false,
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileINCY},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.URL != added.URL {
		t.Fatalf("URL = %q, want the stored one kept", updated.URL)
	}
	if updated.Enabled {
		t.Fatal("update must apply the new enabled state")
	}
}

func TestMissingFileLoadsAsEmpty(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "absent.json"))
	if err := store.Load(); err != nil {
		t.Fatalf("a deployment that never added a source must load cleanly: %v", err)
	}
	if len(store.List()) != 0 {
		t.Fatal("expected no sources")
	}
}

func TestDeleteRemovesTheSource(t *testing.T) {
	store := newTestStore(t)
	added, err := store.Add(Source{
		URL:     "https://panel.example/sub",
		Profile: subscription.ClientProfile{Profile: subscription.ClientProfileHapp},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.Delete(added.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(store.List()) != 0 {
		t.Fatal("source still listed after delete")
	}
	if err := store.Delete(added.ID); err == nil {
		t.Fatal("deleting a missing source must report an error")
	}

	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if strings.Contains(string(data), "panel.example") {
		t.Fatal("the deleted URL is still on disk")
	}
}

// How a source is watched has to survive a restart alongside its identity.
func TestObservationSettingsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subscription_sources.json")
	store := NewStore(path)
	added, err := store.Add(Source{
		URL:      "https://panel.example/sub/token",
		Enabled:  true,
		Mode:     observation.ModeAvailability,
		Silent:   true,
		Unlisted: true,
		Profile:  subscription.ClientProfile{Profile: subscription.ClientProfileHapp},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if policy := added.Policy(); policy.SpeedTest || policy.Alerts || policy.Listed || !policy.AccountAvailability {
		t.Fatalf("policy = %+v, want availability only, silent and unlisted", policy)
	}

	restored := NewStore(path)
	if err := restored.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	sources := restored.List()
	if len(sources) != 1 {
		t.Fatalf("sources after restart = %d, want 1", len(sources))
	}
	if sources[0].Mode != observation.ModeAvailability || !sources[0].Silent || !sources[0].Unlisted {
		t.Fatalf("observation settings were lost: %+v", sources[0])
	}
}

// A file written before observation modes existed describes sources that were
// watched in full, and has to keep behaving that way.
func TestSourcesWithoutAModeReadAsFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subscription_sources.json")
	state := `{"version":1,"sources":[{"id":"src-1","url":"https://panel.example/sub/token","enabled":true,"profile":{"profile":"happ","hwid":"1A2B3C4D-5E6F-7890-ABCD-1234567890AB"}}]}`
	if err := os.WriteFile(path, []byte(state), 0600); err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	if err := store.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	sources := store.List()
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	if sources[0].Mode != observation.ModeFull || sources[0].Silent || sources[0].Unlisted {
		t.Fatalf("old source was not normalized to full: %+v", sources[0])
	}
	if got := sources[0].Policy(); got != observation.Full() {
		t.Fatalf("policy = %+v, want the full one", got)
	}
}
