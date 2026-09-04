package subscription

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"xray-checker/config"
)

// Client profiles decide which application a subscription request impersonates.
//
// This matters because a panel answers differently depending on who is asking.
// Remnawave matches Subscription Response Rules on the User-Agent, and treats
// Happ and INCY as "extended clients" that may receive JSON where another client
// receives base64. A panel with the HWID device limit switched on refuses the
// subscription outright — HTTP 404 with x-hwid-not-supported: true — unless the
// request carries an x-hwid header.
//
// The profile therefore belongs to the source, not to the checker: the operator's
// own panel is keyed on the checker's own User-Agent, while a third-party panel
// has to be approached as one of the clients it knows.
const (
	ClientProfileChecker = "checker"
	ClientProfileHapp    = "happ"
	ClientProfileINCY    = "incy"
	ClientProfileCustom  = "custom"
)

const (
	// The checker's own identity. Response rules on the operator's panel are
	// matched against this User-Agent, so it must not drift.
	checkerUserAgent   = "Xray-Checker"
	checkerDeviceOS    = "CheckerOS"
	checkerDeviceModel = "Xray-Checker Pro Max"
	// A fixed HWID keeps the checker to a single device slot. Generating one per
	// start would consume a new slot on every restart until the limit is hit.
	checkerHWID = "0JLQq9Ca0JvQrtCn0Jgg0JHQm9Cp0KLQrCBIV0lE"

	happUserAgent  = "Happ/3.13.0"
	happClientName = "Happ"
	happVersion    = "3.13.0"

	incyUserAgent  = "INCY/2.5.5/ios CFNetwork/3860.700.1 Darwin/25.6.0"
	incyClientName = "INCY"
	incyVersion    = "2.5.5"

	// Defaults taken from what these clients actually send on iOS.
	defaultIOSVersion     = "26.6"
	defaultIOSDeviceModel = "iPhone 16 Pro"
	defaultDeviceLocale   = "ru-RU"
	iosDeviceOS           = "iOS"
)

// hwidPattern is Remnawave's own validation rule for the header. Sending a
// value it rejects is the same as sending nothing at all.
var hwidPattern = regexp.MustCompile(`^[a-zA-Z0-9=-]{10,64}$`)

// ClientProfile is the set of headers one source sends. Empty fields fall back
// to the profile's defaults, so an operator only fills in what they want to
// differ.
type ClientProfile struct {
	Profile     string `json:"profile"`
	UserAgent   string `json:"userAgent,omitempty"`
	HWID        string `json:"hwid,omitempty"`
	DeviceOS    string `json:"deviceOs,omitempty"`
	OSVersion   string `json:"osVersion,omitempty"`
	DeviceModel string `json:"deviceModel,omitempty"`
	Locale      string `json:"locale,omitempty"`
}

func NormalizeClientProfileName(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ClientProfileHapp:
		return ClientProfileHapp
	case ClientProfileINCY:
		return ClientProfileINCY
	case ClientProfileCustom:
		return ClientProfileCustom
	default:
		return ClientProfileChecker
	}
}

// Headers renders the request headers for this profile. The checker profile
// reproduces exactly what the checker has always sent, so an existing
// deployment keeps working byte for byte.
func (p ClientProfile) Headers() map[string]string {
	profile := NormalizeClientProfileName(p.Profile)

	userAgent := checkerUserAgent
	deviceOS := checkerDeviceOS
	osVersion := config.Version
	deviceModel := checkerDeviceModel
	hwid := checkerHWID
	locale := ""
	clientName := ""
	appVersion := ""

	switch profile {
	case ClientProfileHapp, ClientProfileINCY:
		// Both are impersonated as iOS builds: that is the platform the
		// operator asked for, and the panel records it verbatim. The client and
		// version headers travel with the real apps; Remnawave itself reads only
		// the device headers, but a panel is free to match a response rule on
		// anything, so a half-dressed request is a request that may be answered
		// differently.
		userAgent = happUserAgent
		clientName = happClientName
		appVersion = happVersion
		if profile == ClientProfileINCY {
			userAgent = incyUserAgent
			clientName = incyClientName
			appVersion = incyVersion
		}
		deviceOS = iosDeviceOS
		osVersion = defaultIOSVersion
		deviceModel = defaultIOSDeviceModel
		locale = defaultDeviceLocale
		hwid = ""
	case ClientProfileCustom:
		userAgent = ""
		deviceOS = ""
		osVersion = ""
		deviceModel = ""
		hwid = ""
	}

	if value := strings.TrimSpace(p.UserAgent); value != "" {
		userAgent = value
	}
	if value := strings.TrimSpace(p.DeviceOS); value != "" {
		deviceOS = value
	}
	if value := strings.TrimSpace(p.OSVersion); value != "" {
		osVersion = value
	}
	if value := strings.TrimSpace(p.DeviceModel); value != "" {
		deviceModel = value
	}
	if value := strings.TrimSpace(p.HWID); value != "" {
		hwid = value
	}
	if value := strings.TrimSpace(p.Locale); value != "" {
		locale = value
	}

	headers := map[string]string{"Accept": "*/*"}
	setIfNotEmpty(headers, "User-Agent", userAgent)
	setIfNotEmpty(headers, "X-Device-OS", deviceOS)
	setIfNotEmpty(headers, "X-Ver-OS", osVersion)
	setIfNotEmpty(headers, "X-Device-Model", deviceModel)
	setIfNotEmpty(headers, "X-Device-Locale", locale)
	setIfNotEmpty(headers, "X-Client", clientName)
	setIfNotEmpty(headers, "X-App-Version", appVersion)
	setIfNotEmpty(headers, "X-Hwid", hwid)
	return headers
}

func setIfNotEmpty(headers map[string]string, name string, value string) {
	if strings.TrimSpace(value) != "" {
		headers[name] = value
	}
}

// ValidateHWID rejects values the panel would ignore, so a typo surfaces when
// the operator saves the source rather than as an unexplained 404 later.
func ValidateHWID(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !hwidPattern.MatchString(value) {
		return fmt.Errorf("HWID must be 10-64 characters of latin letters, digits, '=' or '-'")
	}
	return nil
}

// GenerateHWID produces a stable identifier for a new source. It is persisted
// with the source: a device slot on the remote panel belongs to this HWID, and
// a value that changed between restarts would claim a new slot every time.
//
// The shape follows what the iOS clients send — an upper-case UUID — so the
// value does not stand out in a panel's device list next to real phones.
func GenerateHWID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate HWID: %w", err)
	}
	encoded := strings.ToUpper(hex.EncodeToString(buffer))
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}
