package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"xray-checker/paneltelemetry"
	"xray-checker/pathquality"
	"xray-checker/verdictlog"
)

// PathQualityService builds the node × hour-of-day view of the speed history.
type PathQualityService interface {
	Report(from, to time.Time, location *time.Location, keep func(pathquality.NodeInput) bool) pathquality.Report
}

const (
	defaultPathQualityDays = 14
	maxPathQualityDays     = 90
)

// AdminPathQualityHandler serves the path-quality report. It is derived from the
// stored speed history on every request and changes nothing.
//
// days picks the period ending now; tz buckets the hours and defaults to the
// peak time zone, so the table reads in the same clients' hours the peak is
// defined in; subscription narrows the rows to one feed.
func AdminPathQualityHandler(service PathQualityService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		days := defaultPathQualityDays
		if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > maxPathQualityDays {
				writeError(w, "days must be between 1 and 90", http.StatusBadRequest)
				return
			}
			days = parsed
		}
		var location *time.Location
		if zone := strings.TrimSpace(r.URL.Query().Get("tz")); zone != "" {
			loaded, err := time.LoadLocation(zone)
			if err != nil {
				writeError(w, "tz must be an IANA time zone", http.StatusBadRequest)
				return
			}
			location = loaded
		}
		subscription := strings.TrimSpace(r.URL.Query().Get("subscription"))
		var keep func(pathquality.NodeInput) bool
		if subscription != "" {
			keep = func(node pathquality.NodeInput) bool { return node.Subscription == subscription }
		}
		to := time.Now()
		writeJSON(w, service.Report(to.Add(-time.Duration(days)*24*time.Hour), to, location, keep))
	}
}

// VerdictJournal is the read side of the agent verdict journal.
type VerdictJournal interface {
	Query(verdictlog.Filter) ([]verdictlog.Entry, error)
}

// AdminDiagnosticVerdictsHandler lists journaled agent verdicts, newest first.
func AdminDiagnosticVerdictsHandler(journal VerdictJournal) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query := r.URL.Query()
		from, err := parseHistoryTime(query.Get("from"), "from")
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		to, err := parseHistoryTime(query.Get("to"), "to")
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		limit := 0
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 {
				writeError(w, "limit must be a positive integer", http.StatusBadRequest)
				return
			}
		}
		source := strings.TrimSpace(query.Get("source"))
		switch source {
		case "", verdictlog.SourceSpeed, verdictlog.SourceAvailability, verdictlog.SourceSweep:
		default:
			writeError(w, "source must be speed, availability or sweep", http.StatusBadRequest)
			return
		}
		entries, err := journal.Query(verdictlog.Filter{
			StableID: strings.TrimSpace(query.Get("stableId")), Source: source,
			Verdict: strings.TrimSpace(query.Get("verdict")), From: from, To: to, Limit: limit,
		})
		if err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if entries == nil {
			entries = []verdictlog.Entry{}
		}
		writeJSON(w, entries)
	}
}

// AdminPanelTelemetryHandler reports whether the Remnawave node telemetry is
// working. A nil state func means the telemetry is not configured.
func AdminPanelTelemetryHandler(state func() paneltelemetry.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if state == nil {
			writeJSON(w, paneltelemetry.State{})
			return
		}
		writeJSON(w, state())
	}
}
