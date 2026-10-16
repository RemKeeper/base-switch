package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"baseSwitch/internal/config"
	"baseSwitch/internal/provider"
	"baseSwitch/internal/storage"
)

func TestOpenAIChatToAnthropic(t *testing.T) {
	body, err := openAIChatToAnthropic([]byte(`{
		"model": "anyrouter/claude-haiku-4-5-20251001",
		"temperature": 1.4,
		"max_tokens": 32,
		"stop": ["END"],
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": [{"type": "text", "text": "ping"}]},
			{"role": "user", "content": "again"}
		]
	}`), "claude-haiku-4-5-20251001")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "claude-haiku-4-5-20251001" || got["system"] != "be brief" || got["max_tokens"] != float64(32) || got["temperature"] != float64(1) {
		t.Fatalf("request = %#v", got)
	}
	messages := got["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	first := messages[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "ping\nagain" {
		t.Fatalf("message = %#v", first)
	}
	stops := got["stop_sequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop_sequences = %#v", stops)
	}
}

func TestAnthropicMessageToOpenAI(t *testing.T) {
	body, err := anthropicMessageToOpenAI([]byte(`{
		"id": "msg_1",
		"model": "claude-haiku-4-5-20251001",
		"content": [{"type": "text", "text": "pong"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 15, "output_tokens": 5}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["object"] != "chat.completion" || got["model"] != "claude-haiku-4-5-20251001" {
		t.Fatalf("response = %#v", got)
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "pong" || choice["finish_reason"] != "stop" {
		t.Fatalf("choice = %#v", choice)
	}
	usage := got["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(15) || usage["completion_tokens"] != float64(5) || usage["total_tokens"] != float64(20) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestTranslateAnthropicSSE(t *testing.T) {
	input := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":15,"output_tokens":1}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"pong"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	var out bytes.Buffer
	if err := translateAnthropicSSE(strings.NewReader(input), &out, "fallback", nil); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, `"content":"pong"`) || !strings.Contains(text, `"finish_reason":"stop"`) || !strings.Contains(text, `"prompt_tokens":15`) || !strings.Contains(text, `"completion_tokens":5`) || !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("sse = %s", text)
	}
}

func TestHandleChatCompletionsTranslatesAnthropicAndRetries1M(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sk-test" || r.Header.Get("Authorization") != "Bearer sk-test" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("headers = %#v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "claude-haiku-4-5-20251001" {
			t.Errorf("model = %v", body["model"])
		}
		if requests == 1 {
			if beta := r.Header.Get("anthropic-beta"); beta != "" {
				t.Errorf("first beta = %q", beta)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"1m 上下文已经全量可用，请启用 1m 上下文后重试"}`))
			return
		}
		if r.Header.Get("anthropic-beta") != "context-1m-2025-08-07" {
			t.Errorf("retry beta = %q", r.Header.Get("anthropic-beta"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":15,"output_tokens":5}}`)
	}))
	defer upstream.Close()

	store, err := storage.New(filepath.Join(t.TempDir(), "providers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InsertProvider("anyrouter", "anthropic", upstream.URL, "sk-test", "", []string{"claude-haiku-4-5-20251001"}, true); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(provider.NewManager(store), &config.AuthConfig{}, &config.ManagementConfig{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"anyrouter/claude-haiku-4-5-20251001","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "pong" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleChatCompletionsTranslatesAnthropicStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != true {
			t.Fatalf("stream = %v", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	store, err := storage.New(filepath.Join(t.TempDir(), "providers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InsertProvider("anyrouter", "anthropic", upstream.URL, "sk-test", "", []string{"claude-haiku-4-5-20251001"}, true); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(provider.NewManager(store), &config.AuthConfig{}, &config.ManagementConfig{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"pong"`) || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestForwardNonStreamTransportErrorDoesNotPanic(t *testing.T) {
	h := NewHandler(nil, &config.AuthConfig{}, &config.ManagementConfig{}, nil)
	recorder := httptest.NewRecorder()
	h.forwardNonStreamCandidates(recorder, []chatCandidate{{
		provider: &storage.Provider{Name: "down", APIType: "anthropic", BaseURL: "http://127.0.0.1:1", APIKey: "sk-test"},
		model:    "claude-haiku-4-5-20251001",
	}}, "/v1/chat/completions", []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"ping"}]}`))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "上游请求失败") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestFetchProviderModelsAnthropicSendsBearer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" || r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("headers = %#v", r.Header)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-haiku-4-5-20251001"}]}`)
	}))
	defer upstream.Close()

	h := NewHandler(nil, &config.AuthConfig{}, &config.ManagementConfig{}, nil)
	models, err := h.FetchProviderModels("anthropic", upstream.URL, "sk-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "claude-haiku-4-5-20251001" {
		t.Fatalf("models = %#v", models)
	}
}
