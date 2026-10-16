package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const defaultAnthropicMaxTokens = 4096

func openAIChatToAnthropic(body []byte, model string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 OpenAI 请求失败: %w", err)
	}

	messages, system, err := anthropicMessagesFromOpenAI(raw["messages"])
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":      model,
		"max_tokens": anthropicMaxTokens(raw),
		"messages":   messages,
	}
	if system != "" {
		out["system"] = system
	}
	if temperature, ok := floatFromAny(raw["temperature"]); ok {
		if temperature < 0 {
			temperature = 0
		}
		if temperature > 1 {
			temperature = 1
		}
		out["temperature"] = temperature
	}
	if topP, ok := floatFromAny(raw["top_p"]); ok {
		out["top_p"] = topP
	}
	if stream, _ := raw["stream"].(bool); stream {
		out["stream"] = true
	}
	if stops := stopSequences(raw["stop"]); len(stops) > 0 {
		out["stop_sequences"] = stops
	}
	return json.Marshal(out)
}

func anthropicMessagesFromOpenAI(value any) ([]map[string]any, string, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, "", fmt.Errorf("messages 不能为空")
	}

	var systemParts []string
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		text := openAIContentText(msg["content"])
		switch role {
		case "system", "developer":
			if text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		case "assistant":
			role = "assistant"
		case "tool":
			role = "user"
			if text != "" {
				text = "[tool result]\n" + text
			}
		default:
			role = "user"
		}
		if text == "" {
			continue
		}
		if len(messages) > 0 && messages[len(messages)-1]["role"] == role {
			messages[len(messages)-1]["content"] = messages[len(messages)-1]["content"].(string) + "\n" + text
			continue
		}
		messages = append(messages, map[string]any{"role": role, "content": text})
	}
	if len(messages) == 0 {
		return nil, "", fmt.Errorf("messages 中没有可发送的用户或助手内容")
	}
	if messages[0]["role"] != "user" {
		messages = append([]map[string]any{{"role": "user", "content": "..."}}, messages...)
	}
	return messages, strings.Join(systemParts, "\n\n"), nil
}

func openAIContentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			text, _ := part["text"].(string)
			if text == "" {
				continue
			}
			partType, _ := part["type"].(string)
			if partType == "" || partType == "text" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

func anthropicMaxTokens(raw map[string]any) int {
	if value, ok := intFromAny(raw["max_tokens"]); ok && value > 0 {
		return value
	}
	if value, ok := intFromAny(raw["max_completion_tokens"]); ok && value > 0 {
		return value
	}
	return defaultAnthropicMaxTokens
}

func intFromAny(value any) (int, bool) {
	switch n := value.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func floatFromAny(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

func stopSequences(value any) []string {
	switch stop := value.(type) {
	case string:
		if stop == "" {
			return nil
		}
		return []string{stop}
	case []any:
		out := make([]string, 0, len(stop))
		for _, item := range stop {
			text, ok := item.(string)
			if ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func anthropicMessageToOpenAI(body []byte) ([]byte, error) {
	var msg struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("解析 Anthropic 响应失败: %w", err)
	}
	if msg.ID == "" && len(msg.Content) == 0 {
		return nil, fmt.Errorf("上游返回的不是 Anthropic message")
	}

	var text strings.Builder
	for _, block := range msg.Content {
		if block.Type == "" || block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	prompt := msg.Usage.InputTokens
	completion := msg.Usage.OutputTokens
	out := map[string]any{
		"id":      msg.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   msg.Model,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text.String(),
			},
			"finish_reason": openAIFinishReason(msg.StopReason),
		}},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		},
	}
	return json.Marshal(out)
}

func openAIFinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

type anthropicSSEEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		InputTokens  *int `json:"input_tokens"`
		OutputTokens *int `json:"output_tokens"`
	} `json:"usage"`
}

func translateAnthropicSSE(r io.Reader, w io.Writer, fallbackModel string, flush func()) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	state := anthropicStreamState{model: fallbackModel, created: time.Now().Unix()}
	var dataBuf []byte
	flushEvent := func() error {
		if len(bytes.TrimSpace(dataBuf)) == 0 {
			dataBuf = dataBuf[:0]
			return nil
		}
		err := state.handle(w, flush, dataBuf)
		dataBuf = dataBuf[:0]
		return err
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if err := flushEvent(); err != nil {
				return err
			}
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data:"))
		if len(payload) > 0 && payload[0] == ' ' {
			payload = payload[1:]
		}
		if len(dataBuf) > 0 {
			dataBuf = append(dataBuf, '\n')
		}
		dataBuf = append(dataBuf, payload...)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := flushEvent(); err != nil {
		return err
	}
	if err := state.finish(w, flush); err != nil {
		return err
	}
	return writeSSEData(w, flush, []byte("[DONE]"))
}

type anthropicStreamState struct {
	id            string
	model         string
	created       int64
	inputTokens   int
	outputTokens  int
	finishReason  string
	started       bool
	emittedFinish bool
}

func (s *anthropicStreamState) handle(w io.Writer, flush func(), payload []byte) error {
	var event anthropicSSEEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil
	}
	switch event.Type {
	case "message_start":
		if event.Message.ID != "" {
			s.id = event.Message.ID
		}
		if event.Message.Model != "" {
			s.model = event.Message.Model
		}
		if event.Message.Usage.InputTokens != nil {
			s.inputTokens = *event.Message.Usage.InputTokens
		}
		if event.Message.Usage.OutputTokens != nil {
			s.outputTokens = *event.Message.Usage.OutputTokens
		}
		return s.ensureStarted(w, flush)
	case "content_block_delta":
		if event.Delta.Type != "" && event.Delta.Type != "text_delta" {
			return nil
		}
		if event.Delta.Text == "" {
			return nil
		}
		if err := s.ensureStarted(w, flush); err != nil {
			return err
		}
		return s.writeChunk(w, flush, map[string]any{"content": event.Delta.Text}, nil, nil)
	case "message_delta":
		if event.Delta.StopReason != "" {
			s.finishReason = openAIFinishReason(event.Delta.StopReason)
		}
		if event.Usage.InputTokens != nil {
			s.inputTokens = *event.Usage.InputTokens
		}
		if event.Usage.OutputTokens != nil {
			s.outputTokens = *event.Usage.OutputTokens
		}
	case "message_stop":
		return s.finish(w, flush)
	}
	return nil
}

func (s *anthropicStreamState) ensureStarted(w io.Writer, flush func()) error {
	if s.started {
		return nil
	}
	s.started = true
	if s.id == "" {
		s.id = fmt.Sprintf("chatcmpl-%d", s.created)
	}
	return s.writeChunk(w, flush, map[string]any{"role": "assistant", "content": ""}, nil, nil)
}

func (s *anthropicStreamState) finish(w io.Writer, flush func()) error {
	if s.emittedFinish {
		return nil
	}
	if err := s.ensureStarted(w, flush); err != nil {
		return err
	}
	s.emittedFinish = true
	if s.finishReason == "" {
		s.finishReason = "stop"
	}
	usage := map[string]any{
		"prompt_tokens":     s.inputTokens,
		"completion_tokens": s.outputTokens,
		"total_tokens":      s.inputTokens + s.outputTokens,
	}
	return s.writeChunk(w, flush, map[string]any{}, s.finishReason, usage)
}

func (s *anthropicStreamState) writeChunk(w io.Writer, flush func(), delta map[string]any, finish any, usage any) error {
	chunk := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return writeSSEData(w, flush, payload)
}

func writeSSEData(w io.Writer, flush func(), payload []byte) error {
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}
