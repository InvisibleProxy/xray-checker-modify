package probeagent

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientIdentitySurvivesContainerStyleRestart(t *testing.T) {
	identityDir := t.TempDir()
	config := ClientConfig{
		AgentID: "agent_test", ControllerURL: "https://checker.example.com",
		ControllerIP: "198.51.100.10", IdentityDir: identityDir,
		AgentVersion: "test", Capabilities: []string{"control-v1"},
	}
	first, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity := first.identity
	second, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if second.identity.IdentityPrivateKey != firstIdentity.IdentityPrivateKey || second.identity.ObservationPrivateKey != firstIdentity.ObservationPrivateKey {
		t.Fatal("client generated new keys instead of reusing the persistent identity")
	}
	info, err := os.Stat(filepath.Join(identityDir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("identity file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestPinnedHTTPClientUsesPinnedAddressAndRefusesRedirects(t *testing.T) {
	var targetHits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/target" {
			targetHits.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, request, "/target", http.StatusFound)
	}))
	defer server.Close()

	certificate := server.Certificate()
	caPath := filepath.Join(t.TempDir(), "controller-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(certificate.Raw)
	if err != nil || parsed == nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	client, err := newPinnedHTTPClient(server.URL, "127.0.0.1", caPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound || targetHits.Load() != 0 {
		t.Fatalf("redirect status = %d, target hits = %d", response.StatusCode, targetHits.Load())
	}
}
