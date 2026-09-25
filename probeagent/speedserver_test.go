package probeagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"xray-checker/diagnostics"
)

// A download job that names a catalogue server is measured against that server,
// and the answer says so. Without it the agent's rate came from its own URL, and
// the controller held it against a run that measured somewhere else entirely.
func TestDownloadProbeMeasuresTheServerTheJobNames(t *testing.T) {
	payload := make([]byte, 64*1024)
	var ownHits, catalogueHits atomic.Int32
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ownHits.Add(1)
		_, _ = w.Write(payload)
	}))
	defer own.Close()
	catalogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		catalogueHits.Add(1)
		_, _ = w.Write(payload)
	}))
	defer catalogue.Close()

	executor := newProbeTestExecutor(t, ExecutorConfig{
		DownloadURL: own.URL, DownloadMinSize: int64(len(payload)),
		ResolveSpeedServer: func(id string) (string, bool) {
			if id == "edis-new-york" {
				return catalogue.URL, true
			}
			return "", false
		},
	})
	profile := diagnostics.TestProfile{ID: diagnostics.ProfileDownload, Method: diagnostics.ProbeMethodDownload, ServerID: "edis-new-york"}
	result := executor.proxyCheck(context.Background(), profile, 0, "")
	if result.status != diagnostics.ProbeStatusOnline || result.speedServerID != "edis-new-york" {
		t.Fatalf("result = %+v, want an online transfer from the named server", result)
	}
	if catalogueHits.Load() != 1 || ownHits.Load() != 0 {
		t.Fatalf("hits: catalogue %d, own %d; want the catalogue server only", catalogueHits.Load(), ownHits.Load())
	}

	// An ID this build does not know is not a failure: the agent measures its own
	// URL and names no server, which marks the rate as not comparable.
	profile.ServerID = "a-server-from-a-newer-controller"
	result = executor.proxyCheck(context.Background(), profile, 0, "")
	if result.status != diagnostics.ProbeStatusOnline || result.speedServerID != "" {
		t.Fatalf("result = %+v, want an online transfer from the agent's own URL with no server named", result)
	}
	if ownHits.Load() != 1 {
		t.Fatalf("own URL hits = %d, want the fallback to the agent's own URL", ownHits.Load())
	}
}

// A failure against the named server is still about that server.
func TestDownloadProbeNamesTheServerEvenWhenTheTransferFails(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	executor := newProbeTestExecutor(t, ExecutorConfig{
		DownloadURL:        failing.URL,
		ResolveSpeedServer: func(string) (string, bool) { return failing.URL, true },
	})
	result := executor.proxyCheck(context.Background(), diagnostics.TestProfile{
		ID: diagnostics.ProfileDownload, Method: diagnostics.ProbeMethodDownload, ServerID: "edis-new-york",
	}, 0, "")
	if result.status != diagnostics.ProbeStatusProxyFailure || result.speedServerID != "edis-new-york" {
		t.Fatalf("result = %+v, want a failure attributed to the named server", result)
	}
}

// Only download probes fetch a speed-test file; a status probe ignores a server.
func TestNonDownloadProbeIgnoresASpeedServer(t *testing.T) {
	status := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer status.Close()
	executor := newProbeTestExecutor(t, ExecutorConfig{
		StatusCheckURL:     status.URL,
		ResolveSpeedServer: func(string) (string, bool) { t.Fatal("a status probe resolved a speed server"); return "", false },
	})
	result := executor.proxyCheck(context.Background(), diagnostics.TestProfile{
		ID: diagnostics.ProfileStatus, Method: diagnostics.ProbeMethodStatus, ServerID: "edis-new-york",
	}, 0, "")
	if result.status != diagnostics.ProbeStatusOnline || result.speedServerID != "" {
		t.Fatalf("result = %+v, want an online status probe without a server", result)
	}
}
