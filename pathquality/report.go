// Package pathquality turns speed-test history into a view of when and where the
// path to each node degrades.
//
// A single slow measurement is an alert; the same node slow every evening is a
// fact about the route to its hoster, and a different decision — where to put
// clients, which provider to keep, what to take to the hoster. The history was
// all there, one node at a time and one measurement at a time; nothing read it
// by hour of day, which is the axis a congested or throttled path shows up on.
//
// Everything here is derived at read time from stored measurements. Nothing is
// persisted, and nothing feeds back into a verdict or an alert.
package pathquality

import (
	"net/url"
	"sort"
	"strings"
	"time"

	"xray-checker/speedtest"
)

// Options shape one report.
type Options struct {
	From, To time.Time
	// Location buckets measurements by hour of day. The peak window is read in
	// PeakLocation instead: the hours that matter are the clients' evening, not
	// the operator's.
	Location     *time.Location
	PeakLocation *time.Location
	// PeakStart and PeakEnd are whole hours, end exclusive; PeakEnd may be 24,
	// and a window that wraps past midnight has PeakEnd below PeakStart.
	PeakStart, PeakEnd int
	// Threshold judges a measurement that predates per-result thresholds.
	Threshold float64
}

// NodeInput is one node's history and how the operator names it.
type NodeInput struct {
	StableID     string
	Name         string
	Subscription string
	// Environment marks a node from a subscription the deployment configures
	// itself, which is the only kind the Telegram digest speaks about.
	Environment bool
	Results     []speedtest.Result
}

// Source builds reports from the live histories. Nodes and Threshold are read on
// every report, so a node added or renamed since the last one reads as it is.
type Source struct {
	Nodes     func() []NodeInput
	Threshold func() float64
	// PeakLocation, PeakStart and PeakEnd define the evening that matters to
	// clients; see Options.
	PeakLocation       *time.Location
	PeakStart, PeakEnd int
}

// Report builds the view for [from, to), bucketed in location — the peak zone
// when nil — over the nodes keep accepts, or all of them when keep is nil.
func (s Source) Report(from, to time.Time, location *time.Location, keep func(NodeInput) bool) Report {
	if location == nil {
		location = s.PeakLocation
	}
	var nodes []NodeInput
	if s.Nodes != nil {
		for _, node := range s.Nodes() {
			if keep == nil || keep(node) {
				nodes = append(nodes, node)
			}
		}
	}
	threshold := 0.0
	if s.Threshold != nil {
		threshold = s.Threshold()
	}
	return Build(nodes, Options{
		From: from, To: to, Location: location, PeakLocation: s.PeakLocation,
		PeakStart: s.PeakStart, PeakEnd: s.PeakEnd, Threshold: threshold,
	})
}

// Cell is one hour of the day for one node, or for the whole fleet.
type Cell struct {
	Hour       int     `json:"hour"`
	Runs       int     `json:"runs"`
	Bad        int     `json:"bad"`
	MedianMbps float64 `json:"medianMbps,omitempty"`
}

// Share is Bad over Runs, or zero when nothing ran.
func (c Cell) Share() float64 {
	if c.Runs == 0 {
		return 0
	}
	return float64(c.Bad) / float64(c.Runs)
}

// Window summarises the peak or the rest of the day.
type Window struct {
	Runs       int     `json:"runs"`
	Bad        int     `json:"bad"`
	BadShare   float64 `json:"badShare"`
	MedianMbps float64 `json:"medianMbps,omitempty"`
}

// NodeReport is one node's row.
type NodeReport struct {
	StableID     string `json:"stableId"`
	Name         string `json:"name"`
	Subscription string `json:"subscription,omitempty"`
	Environment  bool   `json:"environment"`
	Hours        []Cell `json:"hours"`
	Peak         Window `json:"peak"`
	OffPeak      Window `json:"offPeak"`
	// WorstHour is the hour with the highest bad share among hours with at
	// least two runs, or -1 when no hour qualifies.
	WorstHour int `json:"worstHour"`
	// Verdicts counts the agent probes of the measurements in the window, by
	// verdict, so a node whose evening slowdowns the agents keep calling "the
	// path" reads differently from one they reproduce.
	Verdicts map[string]int `json:"verdicts,omitempty"`
	// Servers counts the measurements by the host of their test URL. A node
	// measured against another server than the rest is compared with care.
	Servers map[string]int `json:"servers,omitempty"`
}

// Report is the whole view.
type Report struct {
	From      time.Time    `json:"from"`
	To        time.Time    `json:"to"`
	TimeZone  string       `json:"timeZone"`
	PeakZone  string       `json:"peakTimeZone"`
	PeakStart int          `json:"peakStart"`
	PeakEnd   int          `json:"peakEnd"`
	Nodes     []NodeReport `json:"nodes"`
	Fleet     []Cell       `json:"fleet"`
	// Excluded counts the confirmation re-runs left out: they only happen after
	// a bad measurement, so counting them would weight every bad hour twice.
	Excluded int `json:"excluded"`
}

// Build aggregates the given histories. Nodes keep the order they are given in.
func Build(nodes []NodeInput, options Options) Report {
	if options.Location == nil {
		options.Location = time.UTC
	}
	if options.PeakLocation == nil {
		options.PeakLocation = options.Location
	}
	report := Report{
		From: options.From.UTC(), To: options.To.UTC(),
		TimeZone: options.Location.String(), PeakZone: options.PeakLocation.String(),
		PeakStart: options.PeakStart, PeakEnd: options.PeakEnd,
		Nodes: make([]NodeReport, 0, len(nodes)),
	}
	fleet := newHours()
	fleetRates := make([][]float64, 24)
	for _, node := range nodes {
		row := NodeReport{
			StableID: node.StableID, Name: node.Name, Subscription: node.Subscription, Environment: node.Environment,
			Hours: make([]Cell, 24), WorstHour: -1,
		}
		hours := newHours()
		rates := make([][]float64, 24)
		var peakRates, offRates []float64
		for _, result := range node.Results {
			if !included(result, options) {
				if result.Source == speedtest.ConfirmationRetrySource && inRange(result.CheckedAt, options) {
					report.Excluded++
				}
				continue
			}
			bad := Bad(result, options.Threshold)
			hour := result.CheckedAt.In(options.Location).Hour()
			hours[hour].Runs++
			fleet[hour].Runs++
			if bad {
				hours[hour].Bad++
				fleet[hour].Bad++
			}
			rate, measured := successfulRate(result)
			if measured {
				rates[hour] = append(rates[hour], rate)
				fleetRates[hour] = append(fleetRates[hour], rate)
			}
			window := &row.OffPeak
			if inPeak(result.CheckedAt.In(options.PeakLocation).Hour(), options.PeakStart, options.PeakEnd) {
				window = &row.Peak
			}
			window.Runs++
			if bad {
				window.Bad++
			}
			if measured {
				if window == &row.Peak {
					peakRates = append(peakRates, rate)
				} else {
					offRates = append(offRates, rate)
				}
			}
			if probe := result.AgentDiagnostic; probe != nil && probe.State != "" && probe.State != speedtest.AgentDiagnosticRunning {
				if row.Verdicts == nil {
					row.Verdicts = make(map[string]int)
				}
				row.Verdicts[probe.State]++
			}
			if host := urlHost(result.URL); host != "" {
				if row.Servers == nil {
					row.Servers = make(map[string]int)
				}
				row.Servers[host]++
			}
		}
		for hour := range hours {
			hours[hour].MedianMbps = median(rates[hour])
			row.Hours[hour] = hours[hour]
			if hours[hour].Runs >= 2 && (row.WorstHour < 0 || hours[hour].Share() > row.Hours[row.WorstHour].Share()) {
				row.WorstHour = hour
			}
		}
		if row.WorstHour >= 0 && row.Hours[row.WorstHour].Bad == 0 {
			row.WorstHour = -1
		}
		row.Peak.MedianMbps, row.OffPeak.MedianMbps = median(peakRates), median(offRates)
		row.Peak.BadShare, row.OffPeak.BadShare = share(row.Peak), share(row.OffPeak)
		report.Nodes = append(report.Nodes, row)
	}
	report.Fleet = make([]Cell, 24)
	for hour := range fleet {
		fleet[hour].MedianMbps = median(fleetRates[hour])
		report.Fleet[hour] = fleet[hour]
	}
	return report
}

// Bad reports a measurement a speed test would flag: a failure, a node that was
// down when its turn came, a transfer cut short by the deadline, or a rate below
// the threshold it was judged against.
func Bad(result speedtest.Result, fallbackThreshold float64) bool {
	threshold := result.LowSpeedThresholdMbps
	if threshold <= 0 {
		threshold = fallbackThreshold
	}
	return result.Offline || result.Error != "" || result.TimedOut || (threshold > 0 && result.Mbps < threshold)
}

// included keeps measurements that describe the path. A confirmation re-run is
// left out because it only follows a bad measurement; a maintenance probe is not
// a verdict on the node.
func included(result speedtest.Result, options Options) bool {
	if result.CheckedAt.IsZero() || !inRange(result.CheckedAt, options) {
		return false
	}
	if result.MaintenanceProbe || result.ProjectMaintenanceProbe {
		return false
	}
	return result.Source != speedtest.ConfirmationRetrySource
}

func inRange(at time.Time, options Options) bool {
	if !options.From.IsZero() && at.Before(options.From) {
		return false
	}
	if !options.To.IsZero() && !at.Before(options.To) {
		return false
	}
	return true
}

func inPeak(hour, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

func successfulRate(result speedtest.Result) (float64, bool) {
	if result.Offline || result.Error != "" || result.Mbps <= 0 {
		return 0, false
	}
	return result.Mbps, true
}

func newHours() []Cell {
	hours := make([]Cell, 24)
	for hour := range hours {
		hours[hour].Hour = hour
	}
	return hours
}

func share(window Window) float64 {
	if window.Runs == 0 {
		return 0
	}
	return float64(window.Bad) / float64(window.Runs)
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func urlHost(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// Ranked returns the nodes ordered by how often they were bad in the peak,
// worst first; a node with no peak measurements goes last.
func (r Report) Ranked() []NodeReport {
	ranked := append([]NodeReport(nil), r.Nodes...)
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := ranked[i].Peak, ranked[j].Peak
		if (left.Runs == 0) != (right.Runs == 0) {
			return left.Runs != 0
		}
		if left.BadShare != right.BadShare {
			return left.BadShare > right.BadShare
		}
		return ranked[i].Name < ranked[j].Name
	})
	return ranked
}
