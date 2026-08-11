package egentcommon

import (
	"testing"
)

func TestFindRepeatedQuestion(t *testing.T) {
	cases := []struct {
		name     string
		messages []ChatCompletionMessage
		want     string
	}{
		{
			name: "repeated question with prior answer",
			messages: []ChatCompletionMessage{
				{Role: "user", Content: "what is 2+2?"},
				{Role: "assistant", Content: "4"},
				{Role: "user", Content: "WHAT IS 2+2 !!!"},
			},
			want: "4",
		},
		{
			name: "no repeat",
			messages: []ChatCompletionMessage{
				{Role: "user", Content: "what is 2+2?"},
				{Role: "assistant", Content: "4"},
				{Role: "user", Content: "what is the capital of France?"},
			},
			want: "",
		},
		{
			name: "repeat with no prior answer",
			messages: []ChatCompletionMessage{
				{Role: "user", Content: "hi"},
				{Role: "user", Content: "hi"},
			},
			want: "",
		},
		{
			name: "repeat of earlier question later in session",
			messages: []ChatCompletionMessage{
				{Role: "user", Content: "what is 2+2?"},
				{Role: "assistant", Content: "4"},
				{Role: "user", Content: "and 3+3?"},
				{Role: "assistant", Content: "6"},
				{Role: "user", Content: "what is 2+2?"},
			},
			want: "4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FindRepeatedQuestion(tc.messages)
			if tc.want == "" && got != nil {
				t.Fatalf("expected nil, got %q", *got)
			}
			if tc.want != "" && (got == nil || *got != tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, got)
			}
		})
	}
}
