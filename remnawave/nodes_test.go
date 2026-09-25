package remnawave

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xray-checker/paneltelemetry"
)

// The node list is decoded straight into the telemetry record, with the panel's
// own field names.
func TestGetNodesDecodesThePanelNodeRecord(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/nodes" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"response":[{
			"uuid":"node-1","name":"Germany-02","address":"31.76.38.179","isConnected":true,"isDisabled":false,
			"isConnecting":false,"lastStatusMessage":null,"xrayUptime":3600,"usersOnline":37,
			"ips":[{"ip":"31.76.38.179","status":"active"}],
			"system":{"info":{"cpus":1,"memoryTotal":2000},"stats":{"memoryFree":400,"memoryUsed":1600,"loadAvg":[0.9,0.5,0.4],
				"interface":{"interface":"eth0","rxBytesPerSec":12500000,"txBytesPerSec":25000000,"rxTotal":1,"txTotal":1}}},
			"versions":{"xray":"26.2.6","node":"2.3.1"}
		}]}`))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := client.GetNodes(context.Background())
	if err != nil || len(nodes) != 1 {
		t.Fatalf("GetNodes = %+v, %v", nodes, err)
	}
	node := nodes[0]
	if node.UUID != "node-1" || node.UsersOnline != 37 || !node.IsConnected || node.System == nil ||
		node.System.Stats.Interface == nil || node.Versions == nil || node.Versions.Xray != "26.2.6" || len(node.IPs) != 1 {
		t.Fatalf("node = %+v", node)
	}
}

// A token without the scope is a configuration problem with one fix, so it is
// reported as such rather than as a broken panel.
func TestGetNodesReportsAMissingScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Forbidden resource"}`))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetNodes(context.Background()); !errors.Is(err, paneltelemetry.ErrForbidden) {
		t.Fatalf("GetNodes error = %v, want ErrForbidden", err)
	}
}
