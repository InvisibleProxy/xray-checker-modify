package paneltelemetry

import (
	"math"
	"time"
)

// Link states of View.Link, in the order they are decided.
const (
	LinkDisabled     = "disabled"
	LinkConnecting   = "connecting"
	LinkDisconnected = "disconnected"
	LinkConnected    = "connected"
)

// Units of Uptime.Unit.
const (
	UptimeMinutes = "min"
	UptimeHours   = "h"
	UptimeDays    = "d"
)

// View is the panel's view of one node as the operator reads it, in Telegram
// and in the admin panel alike. Which facts are shown and how they are rounded
// is decided here once, so the two never tell different stories about the same
// node; each only words and lays the facts out in its own language. A zero or
// absent field is a fact not shown.
type View struct {
	Name string `json:"name"`
	Link string `json:"link"`
	// Message is the panel's own reason, kept only while it cannot reach the node.
	Message     string `json:"message,omitempty"`
	UsersOnline int    `json:"usersOnline"`
	// UsersOnlineBefore is the online count before the node's current failure,
	// present only when there was a sample then and the count has moved since:
	// a count that fell when the failure began says the clients lost it too.
	UsersOnlineBefore *int    `json:"usersOnlineBefore,omitempty"`
	MemoryUsedPercent int     `json:"memoryUsedPercent,omitempty"`
	Load1             float64 `json:"load1,omitempty"`
	// CPUs qualifies Load1 and is kept only alongside it.
	CPUs       int       `json:"cpus,omitempty"`
	RxMbps     int       `json:"rxMbps,omitempty"`
	TxMbps     int       `json:"txMbps,omitempty"`
	XrayUptime *Uptime   `json:"xrayUptime,omitempty"`
	FetchedAt  time.Time `json:"fetchedAt"`
}

// Uptime is a coarse age. An operator reads "restarted an hour ago" or "up for
// twelve days", not a count of minutes on a node that has run for weeks.
type Uptime struct {
	Value int    `json:"value"`
	Unit  string `json:"unit"`
}

// View applies the display rules to a status.
func (s Status) View() View {
	view := View{
		Name:              s.Name,
		UsersOnline:       s.UsersOnline,
		MemoryUsedPercent: int(math.Round(s.MemoryUsedPercent)),
		Load1:             math.Round(s.Load1*100) / 100,
		RxMbps:            int(math.Round(s.RxMbps)),
		TxMbps:            int(math.Round(s.TxMbps)),
		XrayUptime:        coarseUptime(s.XrayUptime),
		FetchedAt:         s.FetchedAt,
	}
	switch {
	case s.Disabled:
		view.Link = LinkDisabled
	case !s.Connected && s.Connecting:
		view.Link = LinkConnecting
	case !s.Connected:
		view.Link = LinkDisconnected
		view.Message = s.Message
	default:
		view.Link = LinkConnected
	}
	if view.Load1 > 0 {
		view.CPUs = s.CPUs
	}
	if s.HasBefore && s.UsersOnlineBefore != s.UsersOnline {
		before := s.UsersOnlineBefore
		view.UsersOnlineBefore = &before
	}
	return view
}

func coarseUptime(value time.Duration) *Uptime {
	switch {
	case value <= 0:
		return nil
	case value >= 48*time.Hour:
		return &Uptime{Value: int(value / (24 * time.Hour)), Unit: UptimeDays}
	case value >= time.Hour:
		return &Uptime{Value: int(value / time.Hour), Unit: UptimeHours}
	default:
		return &Uptime{Value: int(value / time.Minute), Unit: UptimeMinutes}
	}
}
