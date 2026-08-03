package xray

import (
	"strings"
	"testing"

	"xray-checker/models"
)

func TestPreserveStableIDsKeepsIDWhenNodeParametersChange(t *testing.T) {
	oldProxy := testProxy("NL", "Main", "old.example.com", 443)
	oldProxy.StableID = "existing-stable-id"
	newProxy := testProxy("NL", "Main", "new.example.com", 443)

	preserved := PreserveStableIDs([]*models.ProxyConfig{oldProxy}, []*models.ProxyConfig{newProxy})

	if preserved != 1 {
		t.Fatalf("expected 1 preserved ID, got %d", preserved)
	}
	if newProxy.StableID != oldProxy.StableID {
		t.Fatalf("expected stable ID %q, got %q", oldProxy.StableID, newProxy.StableID)
	}
}

func TestPreserveStableIDsKeepsPreviouslyPreservedIDForUnchangedNode(t *testing.T) {
	oldProxy := testProxy("NL", "Main", "node.example.com", 443)
	oldProxy.StableID = "existing-stable-id"
	newProxy := testProxy("NL", "Main", "node.example.com", 443)

	PreserveStableIDs([]*models.ProxyConfig{oldProxy}, []*models.ProxyConfig{newProxy})

	if newProxy.StableID != oldProxy.StableID {
		t.Fatalf("expected stable ID %q, got %q", oldProxy.StableID, newProxy.StableID)
	}
}

func TestPreserveStableIDsSkipsAmbiguousIdentity(t *testing.T) {
	oldProxyA := testProxy("NL", "Main", "a.example.com", 443)
	oldProxyA.StableID = "stable-a"
	oldProxyB := testProxy("NL", "Main", "b.example.com", 443)
	oldProxyB.StableID = "stable-b"
	newProxy := testProxy("NL", "Main", "c.example.com", 443)
	generatedID := newProxy.GenerateStableID()

	preserved := PreserveStableIDs([]*models.ProxyConfig{oldProxyA, oldProxyB}, []*models.ProxyConfig{newProxy})

	if preserved != 0 {
		t.Fatalf("expected no preserved IDs for ambiguous identity, got %d", preserved)
	}
	if newProxy.StableID != generatedID {
		t.Fatalf("expected generated ID %q, got %q", generatedID, newProxy.StableID)
	}
}

func TestIsConfigsEqualDetectsChangedConfigWithSameStableID(t *testing.T) {
	oldProxy := testProxy("NL", "Main", "old.example.com", 443)
	oldProxy.StableID = "same-stable-id"
	newProxy := testProxy("NL", "Main", "new.example.com", 443)
	newProxy.StableID = "same-stable-id"

	if IsConfigsEqual([]*models.ProxyConfig{oldProxy}, []*models.ProxyConfig{newProxy}) {
		t.Fatal("expected configs with changed proxy parameters to differ")
	}
}

func TestIsConfigsEqualIgnoresOrder(t *testing.T) {
	proxyA := testProxy("A", "Main", "a.example.com", 443)
	proxyA.StableID = "stable-a"
	proxyB := testProxy("B", "Main", "b.example.com", 443)
	proxyB.StableID = "stable-b"

	newProxyA := *proxyA
	newProxyB := *proxyB

	if !IsConfigsEqual([]*models.ProxyConfig{proxyA, proxyB}, []*models.ProxyConfig{&newProxyB, &newProxyA}) {
		t.Fatal("expected same configs in different order to be equal")
	}
}

func TestValidateStableIDsGeneratesAndAcceptsUniqueIDs(t *testing.T) {
	proxyA := testProxy("A", "Main", "a.example.com", 443)
	proxyB := testProxy("B", "Main", "b.example.com", 443)
	if err := ValidateStableIDs([]*models.ProxyConfig{proxyA, proxyB}); err != nil {
		t.Fatalf("ValidateStableIDs() error = %v", err)
	}
	if proxyA.StableID == "" || proxyB.StableID == "" || proxyA.StableID == proxyB.StableID {
		t.Fatalf("generated StableIDs are not unique: %q, %q", proxyA.StableID, proxyB.StableID)
	}
}

func TestValidateStableIDsRejectsExplicitCaseFoldedCollision(t *testing.T) {
	proxyA := testProxy("A", "Main", "a.example.com", 443)
	proxyA.StableID = " Collision-ID "
	proxyB := testProxy("B", "Main", "b.example.com", 443)
	proxyB.StableID = "collision-id"
	err := ValidateStableIDs([]*models.ProxyConfig{proxyA, proxyB})
	if err == nil {
		t.Fatal("explicit StableID collision was accepted")
	}
	if !strings.Contains(err.Error(), "A") || !strings.Contains(err.Error(), "B") {
		t.Fatalf("collision error does not identify both nodes: %v", err)
	}
	if proxyA.StableID != "Collision-ID" {
		t.Fatalf("StableID was not trimmed: %q", proxyA.StableID)
	}
}

func TestValidateStableIDsRejectsGeneratedCollision(t *testing.T) {
	proxyA := testProxy("First label", "One", "same.example.com", 443)
	proxyB := testProxy("Second label", "Two", "same.example.com", 443)
	if err := ValidateStableIDs([]*models.ProxyConfig{proxyA, proxyB}); err == nil {
		t.Fatal("generated StableID collision was accepted")
	}
}

func TestValidateStableIDsRejectsNilProxy(t *testing.T) {
	if err := ValidateStableIDs([]*models.ProxyConfig{nil}); err == nil {
		t.Fatal("nil proxy was accepted")
	}
}

func testProxy(name, subName, server string, port int) *models.ProxyConfig {
	return &models.ProxyConfig{
		Protocol: "vless",
		Name:     name,
		SubName:  subName,
		Server:   server,
		Port:     port,
		UUID:     "00000000-0000-0000-0000-000000000001",
		Security: "reality",
		Type:     "tcp",
	}
}
