package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"baseSwitch/internal/config"
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
