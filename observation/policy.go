// Package observation owns what the checker does with the nodes of one
// subscription source.
//
// It exists because a source is not always something an operator wants watched
// the same way. Their own panel is the service they run: every node measured,
// every outage announced, every result published. A third-party panel added
// from the admin UI is usually watched for one specific reason, and the rest —
// speed tests it does not need, alerts at three in the morning about servers it
// does not own, foreign nodes on its public status page — is noise nobody asked
// for.
//
// The mode and the two switches beside it are a property of the source, and the
// node carries only the source id. That is deliberate: changing how a source is
// watched must take effect at once, without refetching the subscription.
package observation

import "strings"

// Mode says how much of the checker's workflow a source's nodes take part in.
type Mode string

const (
	// ModeFull is the default, and what every environment source gets.
	ModeFull Mode = "full"
	// ModeAvailability drops the source out of scheduled speed tests.
	ModeAvailability Mode = "availability"
	// ModePaused measures nothing. The probe still runs — availability is what
	// every other decision rests on — but it stays evidence rather than a
	// judgement: no downtime, no incident, no alert, no speed test. The nodes
	// stay listed in the panel, which is what tells this apart from disabling
	// the source altogether.
	ModePaused Mode = "paused"
)

// Policy is the effective answer for one node, resolved from its source's mode
// plus the switch beside it.
type Policy struct {
	// AccountAvailability turns a probe into a verdict: downtime, incidents,
	// node status and the availability side of Telegram.
	AccountAvailability bool
	// SpeedTest lets a scheduled run select the node.
	SpeedTest bool
	// Listed puts the node on the public dashboard, into Prometheus and behind
	// its own /config endpoint.
	//
	// Telegram is deliberately not one of these switches. The bot is the
	// operator's channel about the service they run, and that service is the
	// subscription the deployment configures itself; a panel-added source never
	// reaches it at all. See ProxyChecker.EnvironmentSourced.
	Listed bool
}

// Full is what a node observes when nothing says otherwise, which includes
// every node of every environment subscription.
func Full() Policy {
	return Policy{AccountAvailability: true, SpeedTest: true, Listed: true}
}

// PolicyFor expands a mode and the switch beside it into the effective policy.
func PolicyFor(mode Mode, unlisted bool) Policy {
	policy := Full()
	switch NormalizeMode(mode) {
	case ModeAvailability:
		policy.SpeedTest = false
	case ModePaused:
		policy.AccountAvailability = false
		policy.SpeedTest = false
	}
	if unlisted {
		policy.Listed = false
	}
	return policy
}

// NormalizeMode maps an empty or unknown mode onto the full one. State written
// before modes existed carries none, and it described sources that observed
// everything.
func NormalizeMode(mode Mode) Mode {
	switch Mode(strings.TrimSpace(string(mode))) {
	case ModeAvailability:
		return ModeAvailability
	case ModePaused:
		return ModePaused
	default:
		return ModeFull
	}
}

// Modes lists every selectable mode in the order the panel offers them.
func Modes() []Mode {
	return []Mode{ModeFull, ModeAvailability, ModePaused}
}
