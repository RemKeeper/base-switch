package tokenusage

import "testing"

func TestExtractUsageFromJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Usage
	}{
		{
			name: "chat completions fields",
			body: `{"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`,
			want: Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
		},
		{
			name: "responses fields",
			body: `{"usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17}}`,
			want: Usage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17},
		},
		{
			name: "nested streaming response",
			body: `{"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":3}}}`,
			want: Usage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractUsageFromJSON([]byte(tt.body))
			if got == nil || *got != tt.want {
				t.Fatalf("ExtractUsageFromJSON() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestExtractUsageFromSSE(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Usage
	}{
		{
			name: "single line chat chunk",
			body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\ndata: [DONE]\n\n",
			want: Usage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9},
		},
		{
			name: "multi line event",
			body: "event: response.completed\r\ndata: {\"response\":{\r\ndata: \"usage\":{\"input_tokens\":6,\"output_tokens\":4}}}\r\n\r\n",
			want: Usage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractUsageFromSSE([]byte(tt.body))
			if got == nil || *got != tt.want {
				t.Fatalf("ExtractUsageFromSSE() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
