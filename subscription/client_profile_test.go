package subscription

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The checker's own identity must not drift: response rules on the operator's
// panel are matched against this exact User-Agent, and the fixed HWID is what
// keeps the checker to a single device slot.
func TestCheckerProfileSendsTheHistoricHeaders(t *testing.T) {
	headers := ClientProfile{}.Headers()

	if headers["User-Agent"] != "Xray-Checker" {
		t.Fatalf("User-Agent = %q, want Xray-Checker", headers["User-Agent"])
	}
	if headers["X-Device-OS"] != "CheckerOS" {
		t.Fatalf("X-Device-OS = %q", headers["X-Device-OS"])
	}
	if headers["X-Device-Model"] != "Xray-Checker Pro Max" {
		t.Fatalf("X-Device-Model = %q", headers["X-Device-Model"])
	}
	if headers["X-Hwid"] != checkerHWID {
		t.Fatalf("X-Hwid = %q, want the fixed checker HWID", headers["X-Hwid"])
	}
}

func TestImpersonatedClientsSendIOSHeaders(t *testing.T) {
	happ := ClientProfile{Profile: ClientProfileHapp}.Headers()
	if happ["User-Agent"] != "Happ/3.13.0" {
		t.Fatalf("Happ User-Agent = %q", happ["User-Agent"])
	}

	incy := ClientProfile{Profile: ClientProfileINCY}.Headers()
	if incy["User-Agent"] != "INCY/2.5.5/ios CFNetwork/3860.700.1 Darwin/25.6.0" {
		t.Fatalf("INCY User-Agent = %q", incy["User-Agent"])
	}

	if incy["X-Client"] != "INCY" || incy["X-App-Version"] != "2.5.5" {
		t.Fatalf("INCY must send its client headers: %+v", incy)
	}
	if happ["X-Client"] != "Happ" || happ["X-App-Version"] != "3.13.0" {
		t.Fatalf("Happ must send its client headers: %+v", happ)
	}

	for name, headers := range map[string]map[string]string{"happ": happ, "incy": incy} {
		if headers["X-Device-OS"] != "iOS" {
			t.Fatalf("%s X-Device-OS = %q, want iOS", name, headers["X-Device-OS"])
		}
		if headers["X-Ver-OS"] != "26.6" || headers["X-Device-Model"] != "iPhone 16 Pro" {
			t.Fatalf("%s must describe the device these clients actually report: %+v", name, headers)
		}
		if headers["X-Device-Locale"] == "" {
			t.Fatalf("%s must send a device locale like the real app: %+v", name, headers)
		}
		// No HWID of their own: the source supplies a stable one, and sending
		// the checker's would put a third-party panel on the same device slot.
		if headers["X-Hwid"] != "" {
			t.Fatalf("%s must not fall back to the checker HWID", name)
		}
	}
}

func TestExplicitFieldsOverrideProfileDefaults(t *testing.T) {
	headers := ClientProfile{
		Profile:     ClientProfileHapp,
		HWID:        "abcdef0123456789",
		OSVersion:   "17.4",
		DeviceModel: "iPhone 15 Pro",
	}.Headers()

	if headers["X-Hwid"] != "abcdef0123456789" {
		t.Fatalf("X-Hwid = %q", headers["X-Hwid"])
	}
	if headers["X-Ver-OS"] != "17.4" || headers["X-Device-Model"] != "iPhone 15 Pro" {
		t.Fatalf("overrides not applied: %+v", headers)
	}
	if headers["User-Agent"] != "Happ/3.13.0" {
		t.Fatalf("an untouched field must keep the profile default, got %q", headers["User-Agent"])
	}
}

func TestValidateHWIDMatchesThePanelRule(t *testing.T) {
	valid := []string{"abcdefghij", "UE42LJXu4DbiCaBvx-", "AAAA=BBBB=CCCC"}
	for _, value := range valid {
		if err := ValidateHWID(value); err != nil {
			t.Fatalf("%q should be accepted: %v", value, err)
		}
	}
	invalid := []string{"short", "has spaces here", "чересчуркириллица", strings.Repeat("a", 65)}
	for _, value := range invalid {
		if err := ValidateHWID(value); err == nil {
			t.Fatalf("%q should be rejected", value)
		}
	}
	if err := ValidateHWID(""); err != nil {
		t.Fatalf("an empty HWID means 'use the profile default': %v", err)
	}
}

func TestGeneratedHWIDIsAcceptedAndUnique(t *testing.T) {
	first, err := GenerateHWID()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	second, err := GenerateHWID()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if first == second {
		t.Fatal("two sources must not share a device slot")
	}
	if err := ValidateHWID(first); err != nil {
		t.Fatalf("generated HWID must satisfy the panel rule: %v", err)
	}
	// Shaped like the UUID the real iOS clients send, so it does not stand out
	// in a panel's device list.
	if len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Fatalf("generated HWID = %q, want an upper-case UUID", first)
	}
	if first != strings.ToUpper(first) {
		t.Fatalf("generated HWID = %q, want upper case", first)
	}
}

// A panel with the device limit on answers 404 and names the reason in a
// header. Repeating a bare "HTTP 404" would send the operator hunting the URL.
func TestHWIDNotSupportedIsReportedInPlainWords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-hwid-not-supported", "true")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	parser := NewParserForProfile(ClientProfile{Profile: ClientProfileHapp})
	_, err := parser.Parse(server.URL)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "HWID") {
		t.Fatalf("error should explain the HWID requirement, got %v", err)
	}
}

func TestProfileHeadersReachTheRequest(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	parser := NewParserForProfile(ClientProfile{Profile: ClientProfileINCY, HWID: "abcdef0123456789"})
	_, _ = parser.Parse(server.URL)

	if got := seen.Get("User-Agent"); !strings.HasPrefix(got, "INCY/") {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := seen.Get("X-Hwid"); got != "abcdef0123456789" {
		t.Fatalf("X-Hwid = %q", got)
	}
	if got := seen.Get("X-Device-OS"); got != "iOS" {
		t.Fatalf("X-Device-OS = %q", got)
	}
}
