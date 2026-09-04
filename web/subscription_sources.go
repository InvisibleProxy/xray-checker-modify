package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"xray-checker/observation"
	"xray-checker/subscription"
	"xray-checker/subsource"
)

// AdminSubscriptionSource is one panel-managed subscription as the admin UI
// sees it.
//
// The URL is masked: a subscription URL carries the access token of the
// subscription itself, and it must not leave through the API any more than it
// may reach a log or a backup. What remains is enough to recognise the source —
// host plus the ends of the path — and not enough to fetch it. An operator who
// edits a source therefore re-enters the URL; leaving the field empty keeps the
// stored one.
//
// The HWID is returned in full: it is not a credential but the identifier the
// remote panel already stores against this subscription, and the operator needs
// to see which device slot the source occupies.
type AdminSubscriptionSource struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	Name        string `json:"name,omitempty"`
	Enabled     bool   `json:"enabled"`
	Profile     string `json:"profile"`
	UserAgent   string `json:"userAgent,omitempty"`
	HWID        string `json:"hwid,omitempty"`
	DeviceOS    string `json:"deviceOs,omitempty"`
	OSVersion   string `json:"osVersion,omitempty"`
	DeviceModel string `json:"deviceModel,omitempty"`
	Locale      string `json:"locale,omitempty"`
	// Mode and Unlisted say how this source's nodes are watched. Telegram is
	// not among them: the bot speaks only about the environment's subscription.
	Mode      string `json:"mode"`
	Unlisted  bool   `json:"unlisted"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// AdminSubscriptionSourcesResponse also lists the environment sources, so the
// panel shows the full picture: those cannot be edited here, because they are
// the deployment's own configuration and the checker starts from them.
type AdminSubscriptionSourcesResponse struct {
	Sources            []AdminSubscriptionSource  `json:"sources"`
	EnvironmentSources []string                   `json:"environmentSources"`
	Profiles           []AdminClientProfileInfo   `json:"profiles"`
	Modes              []AdminObservationModeInfo `json:"modes"`
}

// AdminObservationModeInfo describes one selectable observation mode. The panel
// renders this list rather than hard-coding it, so a mode added to the API
// reaches the UI with it.
type AdminObservationModeInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type AdminClientProfileInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	UserAgent   string `json:"userAgent"`
	DeviceOS    string `json:"deviceOs,omitempty"`
	OSVersion   string `json:"osVersion,omitempty"`
	DeviceModel string `json:"deviceModel,omitempty"`
	Locale      string `json:"locale,omitempty"`
	SendsHWID   bool   `json:"sendsHwid"`
}

type AdminSubscriptionSourceRequest struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	Name        string `json:"name"`
	Enabled     *bool  `json:"enabled"`
	Profile     string `json:"profile"`
	UserAgent   string `json:"userAgent"`
	HWID        string `json:"hwid"`
	DeviceOS    string `json:"deviceOs"`
	OSVersion   string `json:"osVersion"`
	DeviceModel string `json:"deviceModel"`
	Locale      string `json:"locale"`
	Mode        string `json:"mode"`
	Unlisted    *bool  `json:"unlisted"`
}

// AdminSubscriptionSourcesHandler lists, adds, edits and removes the sources an
// operator manages from the panel. Changes take effect on the next subscription
// refresh rather than immediately: swapping the node set is what refresh is
// for, and it carries the suspicious-diff guard that protects the archive.
func AdminSubscriptionSourcesHandler(store *subsource.Store, environmentURLs []string, onChange func()) http.HandlerFunc {
	applied := func(w http.ResponseWriter) {
		// The observation mode does not describe what to fetch, so it takes
		// effect at once rather than waiting for the next refresh.
		if onChange != nil {
			onChange()
		}
		writeJSON(w, sourcesResponse(store, environmentURLs))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, sourcesResponse(store, environmentURLs))
		case http.MethodPost, http.MethodPut:
			var req AdminSubscriptionSourceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
			// The API only ever shows a masked URL. A client that echoes back
			// what it was shown — toggling a source re-sends the whole row —
			// must not overwrite the stored URL with the mask, which would
			// leave a source that can never be fetched again. An empty URL
			// already means "keep the stored one"; a masked one means the same.
			if isMaskedSubscriptionURL(req.URL) {
				req.URL = ""
			}
			enabled := true
			if req.Enabled != nil {
				enabled = *req.Enabled
			}
			unlisted := false
			if req.Unlisted != nil {
				unlisted = *req.Unlisted
			}
			source := subsource.Source{
				URL:      req.URL,
				Name:     req.Name,
				Enabled:  enabled,
				Mode:     observation.Mode(req.Mode),
				Unlisted: unlisted,
				Profile: subscription.ClientProfile{
					Profile:     req.Profile,
					UserAgent:   req.UserAgent,
					HWID:        req.HWID,
					DeviceOS:    req.DeviceOS,
					OSVersion:   req.OSVersion,
					DeviceModel: req.DeviceModel,
					Locale:      req.Locale,
				},
			}

			var err error
			if strings.TrimSpace(req.ID) == "" {
				_, err = store.Add(source)
			} else {
				_, err = store.Update(req.ID, source)
			}
			if err != nil {
				writeError(w, err.Error(), http.StatusBadRequest)
				return
			}
			applied(w)
		case http.MethodDelete:
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				var req AdminSubscriptionSourceRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
					id = strings.TrimSpace(req.ID)
				}
			}
			if err := store.Delete(id); err != nil {
				writeError(w, err.Error(), http.StatusBadRequest)
				return
			}
			applied(w)
		default:
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func sourcesResponse(store *subsource.Store, environmentURLs []string) AdminSubscriptionSourcesResponse {
	stored := store.List()
	sources := make([]AdminSubscriptionSource, 0, len(stored))
	for _, source := range stored {
		sources = append(sources, AdminSubscriptionSource{
			ID:          source.ID,
			URL:         MaskSubscriptionURL(source.URL),
			Name:        source.Name,
			Enabled:     source.Enabled,
			Profile:     source.Profile.Profile,
			UserAgent:   source.Profile.UserAgent,
			HWID:        source.Profile.HWID,
			DeviceOS:    source.Profile.DeviceOS,
			OSVersion:   source.Profile.OSVersion,
			DeviceModel: source.Profile.DeviceModel,
			Locale:      source.Profile.Locale,
			Mode:        string(observation.NormalizeMode(source.Mode)),
			Unlisted:    source.Unlisted,
			CreatedAt:   source.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}

	masked := make([]string, 0, len(environmentURLs))
	for _, url := range environmentURLs {
		masked = append(masked, MaskSubscriptionURL(url))
	}

	return AdminSubscriptionSourcesResponse{
		Sources:            sources,
		EnvironmentSources: masked,
		Profiles:           clientProfileCatalog(),
		Modes:              observationModeCatalog(),
	}
}

// isMaskedSubscriptionURL reports whether a value carries the marks
// MaskSubscriptionURL leaves behind. Neither character can appear unescaped in
// a real URL, so this cannot reject one an operator actually typed.
func isMaskedSubscriptionURL(value string) bool {
	return strings.ContainsAny(value, "…•")
}

func observationModeCatalog() []AdminObservationModeInfo {
	catalog := make([]AdminObservationModeInfo, 0, len(observation.Modes()))
	for _, mode := range observation.Modes() {
		info := AdminObservationModeInfo{ID: string(mode)}
		switch mode {
		case observation.ModeAvailability:
			info.Label = "Availability only"
			info.Description = "Checked and alerted on as usual, but scheduled speed tests skip it."
		case observation.ModePaused:
			info.Label = "Paused"
			info.Description = "Nodes stay listed and probed, but nothing is counted: no downtime, no incidents, no alerts, no speed tests."
		default:
			info.Label = "Full"
			info.Description = "Watched like the deployment's own subscription."
		}
		catalog = append(catalog, info)
	}
	return catalog
}

// MaskSubscriptionURL keeps a subscription recognisable without publishing the
// token embedded in it. The scheme and host stay, and the path is reduced to
// its first and last few characters.
func MaskSubscriptionURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// Only a fetched URL carries a token. A local file or folder source is a
	// path on this host, and hiding it would tell the operator nothing.
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "…"
	}
	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	if len(path) <= 8 {
		return parsed.Scheme + "://" + parsed.Host + "/" + strings.Repeat("•", len(path))
	}
	return parsed.Scheme + "://" + parsed.Host + "/" + path[:3] + "…" + path[len(path)-3:]
}

// clientProfileCatalog describes each profile so the panel can show what a
// choice actually sends, instead of asking the operator to trust a label.
func clientProfileCatalog() []AdminClientProfileInfo {
	catalog := make([]AdminClientProfileInfo, 0, 4)
	for _, id := range []string{
		subscription.ClientProfileChecker,
		subscription.ClientProfileHapp,
		subscription.ClientProfileINCY,
		subscription.ClientProfileCustom,
	} {
		headers := subscription.ClientProfile{Profile: id}.Headers()
		label := map[string]string{
			subscription.ClientProfileChecker: "Xray-Checker (own identity)",
			subscription.ClientProfileHapp:    "Happ (iOS)",
			subscription.ClientProfileINCY:    "INCY (iOS)",
			subscription.ClientProfileCustom:  "Custom",
		}[id]
		catalog = append(catalog, AdminClientProfileInfo{
			ID:          id,
			Label:       label,
			UserAgent:   headers["User-Agent"],
			DeviceOS:    headers["X-Device-OS"],
			OSVersion:   headers["X-Ver-OS"],
			DeviceModel: headers["X-Device-Model"],
			Locale:      headers["X-Device-Locale"],
			SendsHWID:   id != subscription.ClientProfileCustom,
		})
	}
	return catalog
}
