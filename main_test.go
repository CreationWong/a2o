package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveTargetModelWithExplicitMapping(t *testing.T) {
	if got := resolveTargetModel("deepseek", "ds:deepseek"); got != "ds" {
		t.Fatalf("expected downstream id to resolve to upstream id ds, got %q", got)
	}

	if got := resolveTargetModel("claude-3-5-sonnet", "ds:deepseek"); got != "ds" {
		t.Fatalf("expected single mapping to preserve FORCE_MODEL override behavior, got %q", got)
	}

	if got := resolveTargetModel("unknown", "ds:deepseek,gpt-4o:openai"); got != "unknown" {
		t.Fatalf("expected unknown model to pass through when multiple mappings exist, got %q", got)
	}
}

func TestTransformModelsResponseMapsIdsAndProvider(t *testing.T) {
	body := []byte(`{
		"object":"list",
		"data":[
			{"id":"ds","object":"model","owned_by":"upstream"},
			{"id":"gpt-4o","object":"model","owned_by":"upstream"}
		]
	}`)
	mappings := []ModelMapping{{Upstream: "ds", Downstream: "deepseek"}}

	got, err := transformModelsResponse(body, ServiceConfig{Comment: "vendor"}, mappings, true)
	if err != nil {
		t.Fatalf("transformModelsResponse returned error: %v", err)
	}

	var payload struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("failed to unmarshal transformed response: %v", err)
	}

	if len(payload.Data) != 1 {
		t.Fatalf("expected only mapped models, got %d", len(payload.Data))
	}
	if payload.Data[0]["id"] != "deepseek" {
		t.Fatalf("expected mapped model id deepseek, got %v", payload.Data[0]["id"])
	}
	if payload.Data[0]["owned_by"] != "vendor" {
		t.Fatalf("expected owned_by vendor, got %v", payload.Data[0]["owned_by"])
	}
}

func TestModelsHandlerRequiresAuthAndUsesServiceComment(t *testing.T) {
	oldConfig := config
	config.AuthToken = "secret"
	t.Cleanup(func() { config = oldConfig })

	handler := makeModelsHandler(func() ServiceConfig {
		return ServiceConfig{
			Comment:    "vendor",
			ForceModel: "ds",
		}
	})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized models request to return 401, got %d", unauthorized.Code)
	}

	authorizedReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	authorizedReq.Header.Set("x-api-key", "secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedReq)
	if authorized.Code != http.StatusOK {
		t.Fatalf("expected authorized models request to return 200, got %d", authorized.Code)
	}

	var payload struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(authorized.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to unmarshal authorized models response: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0]["owned_by"] != "vendor" {
		t.Fatalf("expected SERVICE_COMMENT as owned_by, got %#v", payload.Data)
	}
}
