package diagnostics

import "testing"

// The catalogue is the single source shared by the controller, the agent and the
// admin UI. A duplicate ID or a method that disagrees with the executor's switch
// surfaces as an opaque configuration failure at probe time, so it is checked here.
func TestProfileCatalogueIsConsistent(t *testing.T) {
	seen := make(map[string]bool)
	methods := make(map[ProbeMethod]string)
	for _, profile := range Profiles() {
		if profile.ID == "" || profile.Label == "" || profile.Summary == "" {
			t.Errorf("profile %+v is missing an identifier or description", profile)
		}
		if seen[profile.ID] {
			t.Errorf("duplicate profile ID %q", profile.ID)
		}
		seen[profile.ID] = true
		if previous, exists := methods[profile.Method]; exists {
			t.Errorf("method %q is claimed by both %q and %q", profile.Method, previous, profile.ID)
		}
		methods[profile.Method] = profile.ID
		if profile.Tunnelled != profile.Method.Tunnelled() {
			t.Errorf("profile %q declares Tunnelled=%v but its method says %v", profile.ID, profile.Tunnelled, profile.Method.Tunnelled())
		}
		switch profile.Capability {
		case CapabilityDiagnosticV1, CapabilityDiagnosticV2:
		default:
			t.Errorf("profile %q requires unknown capability %q", profile.ID, profile.Capability)
		}
	}
	if len(seen) != 7 {
		t.Errorf("catalogue holds %d profiles, want the 7 selectable diagnostics", len(seen))
	}
}

// The original three profiles must keep advertising diagnostic-v1: raising their
// requirement would silently strand every already-deployed agent.
func TestOriginalProfilesStayAvailableToV1Agents(t *testing.T) {
	for _, id := range []string{ProfileIP, ProfileStatus, ProfileDownload} {
		profile, ok := ProfileByID(id)
		if !ok {
			t.Fatalf("profile %q is missing from the catalogue", id)
		}
		if profile.Capability != CapabilityDiagnosticV1 {
			t.Errorf("profile %q requires %q, want %q", id, profile.Capability, CapabilityDiagnosticV1)
		}
	}
}

func TestAlternativeProfilesResolveAndStayTunnelled(t *testing.T) {
	for _, profile := range Profiles() {
		alternativeID, ok := AlternativeFor(profile.ID)
		if !ok {
			continue
		}
		if alternativeID == profile.ID {
			t.Errorf("profile %q falls back to itself", profile.ID)
		}
		alternative, known := ProfileByID(alternativeID)
		if !known {
			t.Fatalf("profile %q falls back to unknown profile %q", profile.ID, alternativeID)
		}
		if !alternative.Tunnelled {
			t.Errorf("profile %q falls back to transport profile %q, which answers a different question", profile.ID, alternativeID)
		}
	}
}

func TestProfileForCheckMethodCoversEveryCheckerMethod(t *testing.T) {
	for method, want := range map[string]string{"ip": ProfileIP, "status": ProfileStatus, "download": ProfileDownload} {
		descriptor, ok := ProfileForCheckMethod(method)
		if !ok || descriptor.ID != want {
			t.Errorf("ProfileForCheckMethod(%q) = %q/%v, want %q", method, descriptor.ID, ok, want)
		}
	}
	if _, ok := ProfileForCheckMethod("nonsense"); ok {
		t.Error("an unsupported check method resolved to a profile")
	}
}
