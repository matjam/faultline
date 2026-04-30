package main

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 0},      // 3/4 = 0
		{"abcd", 1},     // 4/4 = 1
		{"abcdefgh", 2}, // 8/4 = 2
		{strings.Repeat("x", 400), 100},
	}
	for _, tt := range tests {
		if got := EstimateTokens(tt.in); got != tt.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestEstimateMessagesTokens_Empty(t *testing.T) {
	if got := EstimateMessagesTokens(nil); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestEstimateMessagesTokens_PerMessageOverhead(t *testing.T) {
	// Each message has a +4 overhead beyond its content.
	msgs := []Message{
		{Role: RoleUser, Content: ""},
		{Role: RoleUser, Content: ""},
	}
	if got := EstimateMessagesTokens(msgs); got != 8 {
		t.Errorf("got %d, want 8 (2 messages * 4 overhead)", got)
	}
}

func TestEstimateMessagesTokens_IncludesContentAndToolCalls(t *testing.T) {
	msgs := []Message{
		{
			Role:    RoleAssistant,
			Content: strings.Repeat("a", 400), // 100 tokens
			ToolCalls: []ToolCall{
				{
					Function: FunctionCall{
						Name:      strings.Repeat("n", 16), // 4 tokens
						Arguments: strings.Repeat("g", 40), // 10 tokens
					},
				},
			},
		},
	}
	// 100 (content) + 4 (msg overhead) + 4 (name) + 10 (args) + 4 (tool overhead) = 122
	if got := EstimateMessagesTokens(msgs); got != 122 {
		t.Errorf("got %d, want 122", got)
	}
}

func TestEstimateMessagesTokens_GrowsWithMessages(t *testing.T) {
	// Sanity: more messages -> more tokens, linearly.
	one := []Message{{Content: strings.Repeat("x", 100)}}
	two := append(one, Message{Content: strings.Repeat("x", 100)})

	if EstimateMessagesTokens(two) <= EstimateMessagesTokens(one) {
		t.Error("expected more tokens with two messages than one")
	}
}
