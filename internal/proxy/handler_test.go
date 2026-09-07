package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"baseSwitch/internal/config"
	"baseSwitch/internal/provider"
	"baseSwitch/internal/storage"
)

func TestCheckSingleModelAPIType(t *testing.T) {
	tests := []struct {
		name             string
		apiType          string
		expectedPath     string
		expectedAuth     string
		expectedAPIKey   string
		expectedVersion  string
		expectedBeta     string
		expectStreamBody bool
	}{
		{
			name:             "openai",
			apiType:          "openai",
			expectedPath:     "/v1/chat/completions",
			expectedAuth:     "Bearer test-key",
			expectStreamBody: true,
		},
		{
			name:            "anthropic",
			apiType:         "anthropic",
			expectedPath:    "/v1/messages",
			expectedAPIKey:  "test-key",
			expectedVersion: "2023-06-01",
		},
		{
			name:            "anthropic 1m",
			apiType:         "anthropic",
			expectedPath:    "/v1/messages",
			expectedAPIKey:  "test-key",
			expectedVersion: "2023-06-01",
			expectedBeta:    "context-1m-2025-08-07",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.expectedPath {
					t.Errorf("path = %q, want %q", r.URL.Path, tt.expectedPath)
				}
				if got := r.Header.Get("Authorization"); got != tt.expectedAuth {
					t.Errorf("Authorization = %q, want %q", got, tt.expectedAuth)
				}
				if got := r.Header.Get("x-api-key"); got != tt.expectedAPIKey {
					t.Errorf("x-api-key = %q, want %q", got, tt.expectedAPIKey)
				}
				if got := r.Header.Get("anthropic-version"); got != tt.expectedVersion {
					t.Errorf("anthropic-version = %q, want %q", got, tt.expectedVersion)
				}
				if got := r.Header.Get("anthropic-beta"); got != tt.expectedBeta {
					t.Errorf("anthropic-beta = %q, want %q", got, tt.expectedBeta)
				}

				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				expectedModel := "test-model"
				if tt.expectedBeta != "" {
					expectedModel = "test-model[1M]"
				}
				if body["model"] != expectedModel || body["max_tokens"] != float64(1) {
					t.Errorf("unexpected request body: %#v", body)
				}
				_, hasStream := body["stream"]
				if hasStream != tt.expectStreamBody {
					t.Errorf("stream field present = %v, want %v", hasStream, tt.expectStreamBody)
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()

			h := NewHandler(nil, &config.AuthConfig{}, &config.ManagementConfig{}, nil)
			model := "test-model"
			if tt.expectedBeta != "" {
				model = "test-model[1M]"
			}
			result := h.checkSingleModel(context.Background(), storage.Provider{
				Name: "test-provider", APIType: tt.apiType, BaseURL: server.URL, APIKey: "test-key",
			}, model, "ping", 5)

			if !result.Alive || result.StatusCode != http.StatusOK || result.Error != "" {
				t.Fatalf("unexpected result: %#v", result)
			}
		})
	}
}

func TestForwardNonStreamCandidatesRetriesAtMostThreeTimes(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	h := NewHandler(nil, &config.AuthConfig{}, &config.ManagementConfig{}, nil)
	candidates := make([]chatCandidate, 5)
	for i := range candidates {
		candidates[i] = chatCandidate{provider: &storage.Provider{Name: "p", BaseURL: server.URL}, model: "model"}
	}
	recorder := httptest.NewRecorder()
	h.forwardNonStreamCandidates(recorder, candidates, "/v1/chat/completions", []byte(`{"model":"group"}`))
	if requests != 4 {
		t.Fatalf("requests = %d, want 4 (initial + 3 retries)", requests)
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
}

func TestForwardNonStreamCandidatesUsesMemberModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "actual-model" {
			t.Errorf("model = %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	h := NewHandler(nil, &config.AuthConfig{}, &config.ManagementConfig{}, nil)
	recorder := httptest.NewRecorder()
	h.forwardNonStreamCandidates(recorder, []chatCandidate{{provider: &storage.Provider{Name: "p", BaseURL: server.URL}, model: "actual-model"}}, "/v1/chat/completions", []byte(`{"model":"group"}`))
	if !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestHandleChatCompletionsUsesResolvedModelNotProviderList(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	store, err := storage.New(filepath.Join(t.TempDir(), "providers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InsertProvider("agnes", "openai", upstream.URL, "sk-test", "", []string{"gpt-4o", "gpt-4o-mini", "claude-3"}, true); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(provider.NewManager(store), &config.AuthConfig{}, &config.ManagementConfig{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"agnes/gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotModel != "gpt-4o" {
		t.Fatalf("upstream model = %q, want gpt-4o (must not send provider model list)", gotModel)
	}
}

func TestNormalizeAPITypeDefaultsToOpenAI(t *testing.T) {
	if got := normalizeAPIType(""); got != "openai" {
		t.Fatalf("normalizeAPIType(\"\") = %q, want openai", got)
	}
}

func TestCheckSingleModelRetriesWhenAnthropicRequires1MContext(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if beta := r.Header.Get("anthropic-beta"); beta != "" {
				t.Errorf("first request anthropic-beta = %q, want empty", beta)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"1m 上下文已经全量可用，请启用 1m 上下文后重试","type":"error"}`))
			return
		}
		if beta := r.Header.Get("anthropic-beta"); beta != "context-1m-2025-08-07" {
			t.Errorf("retry anthropic-beta = %q", beta)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer server.Close()

	h := NewHandler(nil, &config.AuthConfig{}, &config.ManagementConfig{}, nil)
	result := h.checkSingleModel(context.Background(), storage.Provider{
		Name: "test-provider", APIType: "anthropic", BaseURL: server.URL, APIKey: "test-key",
	}, "claude-opus-4-7", "ping", 5)

	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if !result.Alive || result.StatusCode != http.StatusOK || result.Error != "" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestHandleAdminCheckModelsTestsRouteGroupMembers(t *testing.T) {
	requests := make(map[string]int)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		model, _ := body["model"].(string)
		requests[model]++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	store, err := storage.New(filepath.Join(t.TempDir(), "providers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, item := range []struct{ name, model string }{
		{"provider-a", "model-a"},
		{"provider-b", "model-b"},
	} {
		if err := store.InsertProvider(item.name, "openai", upstream.URL, "sk-test", "", []string{item.model}, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveRouteGroup("test-group", []storage.RouteGroupMember{
		{Provider: "provider-a", Model: "model-a"},
		{Provider: "provider-b", Model: "model-b"},
	}, true); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(provider.NewManager(store), &config.AuthConfig{}, &config.ManagementConfig{Enabled: true, Keys: []string{"admin-key"}}, nil)
	req := httptest.NewRequest(http.MethodPost, "/admin/models/check", strings.NewReader(`{"group":"test-group","provider":"ignored","models":["ignored/model"],"timeout_seconds":5}`))
	req.Header.Set("Authorization", "Bearer admin-key")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var results []AdminModelCheckResult
	decoder := json.NewDecoder(recorder.Body)
	for decoder.More() {
		var result AdminModelCheckResult
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2; body = %s", len(results), recorder.Body.String())
	}
	if !results[0].Alive || results[0].ModelID != "provider-a/model-a" || !results[1].Alive || results[1].ModelID != "provider-b/model-b" {
		t.Fatalf("unexpected results: %#v", results)
	}
	if requests["model-a"] != 1 || requests["model-b"] != 1 {
		t.Fatalf("upstream requests = %#v, want one request per group member", requests)
	}
}
