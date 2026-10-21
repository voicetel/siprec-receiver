package web

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The served spec must parse and document every route the server
// implements. This is the cheap consistency net; vacuum (see
// vacuum-ruleset.yaml and the Makefile spec target) is the strict
// gate.
func TestOpenAPISpecServedAndComplete(t *testing.T) {
	f := newFixture(t, adminEmail)

	resp, err := http.Get(f.srv.URL + "/openapi.yaml")
	if err != nil {
		t.Fatalf("GET spec: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /openapi.yaml = %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Errorf("Content-Type = %q, want yaml", ct)
	}

	var doc struct {
		OpenAPI string         `yaml:"openapi"`
		Paths   map[string]any `yaml:"paths"`
		Info    map[string]any `yaml:"info"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("spec does not parse: %v", err)
	}

	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi version = %q", doc.OpenAPI)
	}

	wantPaths := []string{
		"/healthz", "/openapi.yaml", "/api/docs",
		"/api/notify/amd", "/api/notify/disposition",
		"/login", "/auth/login", "/auth/callback", "/auth/logout",
		"/", "/recordings", "/recordings/{id}",
		"/recordings/{id}/legs/{legID}/audio",
		"/recordings/{id}/fetch-record", "/recordings/{id}/delete",
		"/admin/users", "/admin/users/delete", "/admin/users/revoke",
		"/admin/audit", "/admin/integrations", "/admin/integrations/delete",
		"/admin/settings", "/admin/settings/purge",
	}
	for _, p := range wantPaths {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("spec is missing documented route %s", p)
		}
	}
}

func TestReDocPageServed(t *testing.T) {
	f := newFixture(t, adminEmail)

	resp, err := http.Get(f.srv.URL + "/api/docs")
	if err != nil {
		t.Fatalf("GET docs: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/docs = %d", resp.StatusCode)
	}

	page := string(body)
	if !strings.Contains(page, "<redoc") || !strings.Contains(page, `spec-url="/openapi.yaml"`) {
		t.Error("docs page does not render the spec with ReDoc")
	}

	// The scoped CSP must allow the ReDoc CDN bundle.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "cdn.jsdelivr.net") {
		t.Errorf("docs CSP does not allow the ReDoc CDN: %q", csp)
	}
}
