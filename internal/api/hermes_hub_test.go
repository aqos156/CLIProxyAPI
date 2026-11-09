package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestHermesHubRequiresDeviceKeyAndRestrictsQuotaOperations(t *testing.T) {
	server := newTestServer(t)
	claude := &auth.Auth{
		ID: "private@example.test", Provider: "claude",
		Metadata: map[string]any{"access_token": "synthetic-secret", "email": "private@example.test"},
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), claude); err != nil {
		t.Fatal(err)
	}

	request := func(method, path, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		res := httptest.NewRecorder()
		server.engine.ServeHTTP(res, req)
		return res
	}
	for _, key := range []string{"", "wrong-key"} {
		if got := request("GET", "/hermes-hub/auth-files", key, "").Code; got != 401 {
			t.Fatalf("expected 401 without device authentication, got %d", got)
		}
	}
	list := request("GET", "/hermes-hub/auth-files", "test-key", "")
	ids := ojson.GetBytes(list.Body.Bytes(), "files").Array()
	if list.Code != 200 || len(ids) != 1 || ids[0].Get("auth_index").String() == "" ||
		strings.Contains(list.Body.String(), "private@example.test") || strings.Contains(list.Body.String(), "synthetic-secret") {
		t.Fatalf("expected one anonymized credential; got HTTP %d", list.Code)
	}
	for _, body := range []string{
		`{"method":"GET","url":"http://127.0.0.1:8317/v8/management/config"}`,
		`{"method":"POST","url":"https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"}`,
		`{"method":"GET","url":"https://api.anthropic.com/api/oauth/usage","proxy_url":"http://example.test"}`,
		`{"method":"GET","url":"https://api.anthropic.com/api/oauth/usage","data":"anything"}`,
	} {
		if got := request("POST", "/hermes-hub/api-call", "test-key", body).Code; got != 403 {
			t.Fatalf("expected forbidden operation, got %d", got)
		}
	}
	for _, path := range []string{"/hermes-hub/reset-quota", "/hermes-hub/auth-files/download"} {
		if got := request("GET", path, "test-key", "").Code; got != 404 {
			t.Fatalf("unexpected route %s: HTTP %d", path, got)
		}
	}
}

func TestHermesHubQuotaProjection(t *testing.T) {
	source := ojson.ParseBytes([]byte(`{"five_hour":{"utilization":40,"resets_at":null,"private":"hidden"},"seven_day":{"utilization":66,"resets_at":"2026-11-12T03:00:00Z"},"spend":{"used_credits":100},"email":"hidden@example.test"}`))
	projection := projectHermesQuota(source, "claude")
	if ojson.Get(projection, "five_hour.utilization").Int() != 40 ||
		ojson.Get(projection, "seven_day.resets_at").String() != "2026-11-12T03:00:00Z" ||
		strings.Contains(projection, "hidden") || strings.Contains(projection, "spend") {
		t.Fatalf("invalid Claude quota projection: %s", projection)
	}
	codex := ojson.ParseBytes([]byte(`{"rate_limit":{"primary_window":{"used_percent":33,"reset_at":2000000000,"limit_window_seconds":18000,"secret":"hidden"}},"account_id":"hidden"}`))
	projection = projectHermesQuota(codex, "codex")
	if ojson.Get(projection, "rate_limit.primary_window.limit_window_seconds").Int() != 18000 ||
		ojson.Get(projection, "rate_limit.primary_window.used_percent").Int() != 33 || strings.Contains(projection, "hidden") {
		t.Fatalf("invalid Codex quota projection: %s", projection)
	}
}
