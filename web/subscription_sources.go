package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

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
	CreatedAt   string `json:"createdAt,omitempty"`
}

// AdminSubscriptionSourcesResponse also lists the environment sources, so the
// panel shows the full picture: those cannot be edited here, because they are
// the deployment's own configuration and the checker starts from them.
type AdminSubscriptionSourcesResponse struct {
	Sources            []AdminSubscriptionSource `json:"sources"`
	EnvironmentSources []string                  `json:"environmentSources"`
	Profiles           []AdminClientProfileInfo  `json:"profiles"`
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
}

// AdminSubscriptionSourcesHandler lists, adds, edits and removes the sources an
// operator manages from the panel. Changes take effect on the next subscription
// refresh rather than immediately: swapping the node set is what refresh is
// for, and it carries the suspicious-diff guard that protects the archive.
func AdminSubscriptionSourcesHandler(store *subsource.Store, environmentURLs []string) http.HandlerFunc {
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
			enabled := true
			if req.Enabled != nil {
				enabled = *req.Enabled
			}
			source := subsource.Source{
				URL:     req.URL,
				Name:    req.Name,
				Enabled: enabled,
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
			writeJSON(w, sourcesResponse(store, environmentURLs))
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
			writeJSON(w, sourcesResponse(store, environmentURLs))
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
	}
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
