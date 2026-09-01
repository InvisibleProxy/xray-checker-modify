package web

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPISpecParsesAndResolvesLocalComponentReferences(t *testing.T) {
	var document map[string]any
	if err := yaml.Unmarshal(openAPISpec, &document); err != nil {
		t.Fatalf("parse embedded OpenAPI document: %v", err)
	}
	components := objectValue(t, document, "components")
	schemas := objectValue(t, components, "schemas")
	responses := objectValue(t, components, "responses")

	var references []string
	collectOpenAPIReferences(document, &references)
	for _, reference := range references {
		switch {
		case strings.HasPrefix(reference, "#/components/schemas/"):
			name := strings.TrimPrefix(reference, "#/components/schemas/")
			if _, ok := schemas[name]; !ok {
				t.Errorf("unresolved schema reference %q", reference)
			}
		case strings.HasPrefix(reference, "#/components/responses/"):
			name := strings.TrimPrefix(reference, "#/components/responses/")
			if _, ok := responses[name]; !ok {
				t.Errorf("unresolved response reference %q", reference)
			}
		}
	}
}

func TestOpenAPISpecDocumentsEveryForkAdminRoute(t *testing.T) {
	var document map[string]any
	if err := yaml.Unmarshal(openAPISpec, &document); err != nil {
		t.Fatalf("parse embedded OpenAPI document: %v", err)
	}
	paths := objectValue(t, document, "paths")
	want := []string{
		"/api/v1/admin/proxies",
		"/api/v1/admin/proxies/check",
		"/api/v1/admin/subscription/refresh",
		"/api/v1/admin/backup",
		"/api/v1/admin/backup/restore",
		"/api/v1/admin/speed-tests",
		"/api/v1/admin/speed-tests/run",
		"/api/v1/admin/speed-tests/node-url",
		"/api/v1/admin/speed-tests/history",
		"/api/v1/admin/nodes-overview",
		"/api/v1/admin/incidents",
		"/api/v1/admin/nodes-overview/geo",
		"/api/v1/admin/nodes-overview/merge/preview",
		"/api/v1/admin/nodes-overview/merge",
		"/api/v1/admin/nodes-overview/delete",
		"/api/v1/admin/nodes-overview/maintenance",
		"/api/v1/admin/schedules",
		"/api/v1/admin/telegram",
		"/api/v1/admin/telegram/test",
		"/api/v1/admin/remnawave",
		"/api/v1/admin/subscription/refresh/progress",
		"/api/v1/admin/remnawave/announce-base",
		"/api/v1/admin/remnawave/locations/suggest",
		"/api/v1/admin/remnawave/sync",
		"/api/v1/admin/diagnostic-agents",
		"/api/v1/admin/diagnostic-agents/reissue",
		"/api/v1/admin/diagnostic-agents/revoke",
		"/api/v1/admin/diagnostic-agents/delete",
		"/api/v1/admin/diagnostic-sessions",
		"/api/v1/admin/diagnostic-sessions/cancel",
		"/api/v1/admin/diagnostic-sessions/export",
	}
	for _, path := range want {
		if _, ok := paths[path]; !ok {
			t.Errorf("OpenAPI document is missing %s", path)
		}
	}
}

func TestOpenAPISpecDocumentsUnauthenticatedProbeAgentRoutes(t *testing.T) {
	var document map[string]any
	if err := yaml.Unmarshal(openAPISpec, &document); err != nil {
		t.Fatalf("parse embedded OpenAPI document: %v", err)
	}
	paths := objectValue(t, document, "paths")
	for _, path := range []string{"/api/v1/agent/enroll", "/api/v1/agent/heartbeat", "/api/v1/agent/jobs/next", "/api/v1/agent/observations"} {
		pathObject := objectValue(t, paths, path)
		post := objectValue(t, pathObject, "post")
		security, ok := post["security"].([]any)
		if !ok || len(security) != 0 {
			t.Errorf("%s must explicitly use an empty OpenAPI security list", path)
		}
	}
}

func objectValue(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("OpenAPI object is missing %q", key)
	}
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("OpenAPI value %q has type %T, want object", key, value)
	}
	return result
}

func collectOpenAPIReferences(value any, references *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$ref" {
				if reference, ok := child.(string); ok {
					*references = append(*references, reference)
				}
				continue
			}
			collectOpenAPIReferences(child, references)
		}
	case []any:
		for _, child := range typed {
			collectOpenAPIReferences(child, references)
		}
	}
}
