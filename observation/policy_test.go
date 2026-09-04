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
			want: Policy{AccountAvailability: true, SpeedTest: true, Listed: true},
		},
		{
			name: "availability only drops scheduled speed tests",
			mode: ModeAvailability,
			want: Policy{AccountAvailability: true, SpeedTest: false, Listed: true},
		},
		{
			// The probe still runs; what stops is the verdict drawn from it.
			name: "paused stops the verdict and the measurement",
			mode: ModePaused,
			want: Policy{AccountAvailability: false, SpeedTest: false, Listed: true},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := PolicyFor(testCase.mode, false); got != testCase.want {
				t.Fatalf("policy = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

// Listing is independent of the mode: an operator can want every measurement a
// full source gives while keeping its nodes out of what they publish.
func TestUnlistedAppliesOnTopOfAnyMode(t *testing.T) {
	policy := PolicyFor(ModeFull, true)
	if !policy.AccountAvailability || !policy.SpeedTest {
		t.Fatalf("listing changed what is measured: %+v", policy)
	}
	if policy.Listed {
		t.Fatalf("policy = %+v, want no listing", policy)
	}

	paused := PolicyFor(ModePaused, false)
	if !paused.Listed {
		t.Fatalf("paused = %+v, want listing kept", paused)
	}
}

// State written before modes existed carries none, and it described sources
// that observed everything.
func TestAnUnknownOrEmptyModeReadsAsFull(t *testing.T) {
	for _, mode := range []Mode{"", "  ", "speedtest", "nonsense"} {
		if got := NormalizeMode(mode); got != ModeFull {
			t.Fatalf("NormalizeMode(%q) = %q, want full", mode, got)
		}
		if got := PolicyFor(mode, false); got != Full() {
			t.Fatalf("PolicyFor(%q) = %+v, want the full policy", mode, got)
		}
	}
}
