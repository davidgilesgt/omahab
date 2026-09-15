package api

import (
	"net/http"
	"strings"
	"testing"
)

// The served OpenAPI document must describe the implemented surface:
// companion project creation, the project kind field, and the workspace
// title/metadata fields backed by domain.Workspace.
func TestOpenAPIServesImplementedSurface(t *testing.T) {
	backend := newRealBackend(t, nil)
	server := newRealServer(t, backend)

	res := doGet(t, server, "/api/openapi.yaml")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, want := range []string{
		"operationId: createCompanionProject",
		"kind: { type: string, enum: [code, docs]",
		"title: { type: string }",
		"gateway_key_id: { type: string }",
		"waiting_line: { type: string }",
		"branch: { type: string, description: 'Deprecated: use title.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("served openapi.yaml missing %q", want)
		}
	}
}
