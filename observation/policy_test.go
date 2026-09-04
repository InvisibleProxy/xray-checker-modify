package observation

import "testing"

func TestModeExpandsIntoWhatIsMeasured(t *testing.T) {
	for _, testCase := range []struct {
		name string
		mode Mode
		want Policy
	}{
		{
			name: "full watches everything",
			mode: ModeFull,
			want: Policy{AccountAvailability: true, SpeedTest: true, Alerts: true, Listed: true},
		},
		{
			name: "availability only drops scheduled speed tests",
			mode: ModeAvailability,
			want: Policy{AccountAvailability: true, SpeedTest: false, Alerts: true, Listed: true},
		},
		{
			// The probe still runs; what stops is the verdict drawn from it.
			name: "paused stops the verdict and the measurement",
			mode: ModePaused,
			want: Policy{AccountAvailability: false, SpeedTest: false, Alerts: true, Listed: true},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := PolicyFor(testCase.mode, false, false); got != testCase.want {
				t.Fatalf("policy = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

// Silence and listing are independent of the mode: an operator can want every
// measurement a full source gives and none of its noise.
func TestSilentAndUnlistedApplyOnTopOfAnyMode(t *testing.T) {
	policy := PolicyFor(ModeFull, true, true)
	if !policy.AccountAvailability || !policy.SpeedTest {
		t.Fatalf("silence or listing changed what is measured: %+v", policy)
	}
	if policy.Alerts || policy.Listed {
		t.Fatalf("policy = %+v, want no alerts and no listing", policy)
	}

	paused := PolicyFor(ModePaused, true, false)
	if paused.Alerts || paused.Listed == false {
		t.Fatalf("paused with silence = %+v, want listing kept", paused)
	}
}

// State written before modes existed carries none, and it described sources
// that observed everything.
func TestAnUnknownOrEmptyModeReadsAsFull(t *testing.T) {
	for _, mode := range []Mode{"", "  ", "speedtest", "nonsense"} {
		if got := NormalizeMode(mode); got != ModeFull {
			t.Fatalf("NormalizeMode(%q) = %q, want full", mode, got)
		}
		if got := PolicyFor(mode, false, false); got != Full() {
			t.Fatalf("PolicyFor(%q) = %+v, want the full policy", mode, got)
		}
	}
}
