package diagnostics

import "testing"

func onlineObservation(mbps int64, serverID string) Observation {
	return Observation{
		Status:             ProbeStatusOnline,
		DirectConnectivity: CheckEvidence{Checked: true, Online: true},
		Throughput:         &ThroughputEvidence{Mbps: mbps},
		SpeedServerID:      serverID,
	}
}

func TestJudgeSpeedSeparatesTheNodeFromThePathAndFromAnIncomparableServer(t *testing.T) {
	const us = "edis-new-york"
	lowSpeed := func(observed, threshold float64, serverID string) SpeedEvidence {
		return SpeedEvidence{Outcome: AutomationOutcomeLowSpeed, ObservedMbps: observed, ThresholdMbps: threshold, ServerID: serverID}
	}
	technical := func(threshold float64, serverID string) SpeedEvidence {
		return SpeedEvidence{Outcome: AutomationOutcomeTechnical, ThresholdMbps: threshold, ServerID: serverID}
	}
	for _, test := range []struct {
		name        string
		evidence    SpeedEvidence
		observation Observation
		reliable    bool
		want        Verdict
		wantReason  string
	}{
		{
			name:     "an agent whose own connectivity failed proves nothing",
			evidence: lowSpeed(2, 50, us), observation: onlineObservation(40, us), reliable: false,
			want: VerdictUnreliable, wantReason: ReasonDirectControlFailed,
		},
		{
			name:     "the alternative endpoint worked: the endpoint failed, not the node",
			evidence: technical(100, us),
			observation: Observation{
				Status:              ProbeStatusProxyFailure,
				AlternativeEndpoint: &AlternativeEndpointObservation{ProfileID: ProfileStatus, Status: ProbeStatusOnline},
			},
			reliable: true, want: VerdictNotReproduced, wantReason: ReasonAlternativeWorked,
		},
		{
			name:        "the agent could not complete the transfer either",
			evidence:    technical(100, us),
			observation: Observation{Status: ProbeStatusProxyFailure},
			reliable:    true, want: VerdictReproduced,
		},
		{
			name:        "a slowdown answered without a rate settles nothing",
			evidence:    lowSpeed(2, 50, us),
			observation: Observation{Status: ProbeStatusOnline},
			reliable:    true, want: VerdictUnreliable, wantReason: ReasonNoThroughput,
		},
		{
			name:        "a technical failure answered by a working transfer is not reproduced",
			evidence:    technical(100, us),
			observation: Observation{Status: ProbeStatusOnline},
			reliable:    true, want: VerdictNotReproduced,
		},
		{
			// The production shape: the checker got 2 Mbps through a node in the
			// United States, an agent in Europe got 40 from a server in France.
			// Both are under 50, and that agreement used to send the alert at once.
			name:        "twenty times faster and still under the threshold is the path, not a reproduction",
			evidence:    lowSpeed(2, 50, us),
			observation: onlineObservation(40, "ovh-proof"),
			reliable:    true, want: VerdictPathLimited, wantReason: ReasonAgentAlsoBelowThreshold,
		},
		{
			name:        "the node carries the threshold for the agent while the checker gets a trickle",
			evidence:    lowSpeed(3, 100, "leaseweb-amsterdam-ams1"),
			observation: onlineObservation(500, "leaseweb-amsterdam-ams1"),
			reliable:    true, want: VerdictPathLimited,
		},
		{
			name:        "above the threshold and in the run's range is simply not reproduced",
			evidence:    lowSpeed(60, 100, us),
			observation: onlineObservation(110, us),
			reliable:    true, want: VerdictNotReproduced,
		},
		{
			name:        "a rate straddling the threshold cannot tell",
			evidence:    lowSpeed(5, 10.5, us),
			observation: onlineObservation(10, us),
			reliable:    true, want: VerdictInconclusive, wantReason: ReasonNearThreshold,
		},
		{
			name:        "the same server, the same range, under the threshold: reproduced",
			evidence:    lowSpeed(5, 10.5, us),
			observation: onlineObservation(9, us),
			reliable:    true, want: VerdictReproduced,
		},
		{
			name:        "the same range against another server is not evidence about the node",
			evidence:    lowSpeed(5, 10.5, us),
			observation: onlineObservation(9, "ovh-proof"),
			reliable:    true, want: VerdictInconclusive, wantReason: ReasonDifferentServer,
		},
		{
			name:        "a run whose server is not in the catalogue can never be matched",
			evidence:    lowSpeed(5, 10.5, ""),
			observation: onlineObservation(9, ""),
			reliable:    true, want: VerdictInconclusive, wantReason: ReasonDifferentServer,
		},
		{
			name:        "a technical failure and a slow agent on the same server",
			evidence:    technical(100, us),
			observation: onlineObservation(42, us),
			reliable:    true, want: VerdictReproduced,
		},
		{
			name:        "a technical failure and a slow agent on another server",
			evidence:    technical(100, us),
			observation: onlineObservation(42, ""),
			reliable:    true, want: VerdictInconclusive, wantReason: ReasonDifferentServer,
		},
		{
			name:        "without a threshold a working transfer is all there is to say",
			evidence:    lowSpeed(5, 0, us),
			observation: onlineObservation(9, us),
			reliable:    true, want: VerdictNotReproduced,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			verdict, reason := JudgeSpeed(test.evidence, test.observation, test.reliable)
			if verdict != test.want || reason != test.wantReason {
				t.Fatalf("JudgeSpeed = %q, %q; want %q, %q", verdict, reason, test.want, test.wantReason)
			}
		})
	}
}

func TestJudgeAvailabilityAsksOnlyWhetherTheNodeWorksFromElsewhere(t *testing.T) {
	for _, test := range []struct {
		name        string
		observation Observation
		reliable    bool
		want        Verdict
		wantReason  string
	}{
		{"unreliable", Observation{Status: ProbeStatusOnline}, false, VerdictUnreliable, ReasonDirectControlFailed},
		{"works from the agent", Observation{Status: ProbeStatusOnline}, true, VerdictNotReproduced, ""},
		{
			"the tunnel works against the alternative endpoint",
			Observation{Status: ProbeStatusProxyFailure, AlternativeEndpoint: &AlternativeEndpointObservation{Status: ProbeStatusOnline}},
			true, VerdictNotReproduced, ReasonAlternativeWorked,
		},
		{"fails for the agent too", Observation{Status: ProbeStatusOffline}, true, VerdictReproduced, ""},
		{"the tunnel fails for the agent too", Observation{Status: ProbeStatusProxyFailure}, true, VerdictReproduced, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			verdict, reason := JudgeAvailability(test.observation, test.reliable)
			if verdict != test.want || reason != test.wantReason {
				t.Fatalf("JudgeAvailability = %q, %q; want %q, %q", verdict, reason, test.want, test.wantReason)
			}
		})
	}
}
