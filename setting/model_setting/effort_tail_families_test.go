package model_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ShouldPreserveEffortTail decides whether a model name keeps its effort-like
// tail. Preserving a synthetic alias silently drops the client's requested
// reasoning effort; parsing a real model ID invents an effort the client never
// asked for and, on a mapped channel, 400s with "reasoning settings conflict".
func TestShouldPreserveEffortTail(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		preserve bool
	}{
		// Real Qwen tier IDs shipped by the vendor (live catalog shapes).
		{name: "qwen3.8-max is a real model", model: "qwen3.8-max", preserve: true},
		{name: "qwen3.7-max is a real model", model: "qwen3.7-max", preserve: true},
		{name: "qwen-vl-max is a real model", model: "qwen-vl-max", preserve: true},
		{name: "future qwen tier is covered", model: "qwen4.2-max", preserve: true},
		{name: "vendor prefixed real model", model: "alibaba/qwen3.8-max", preserve: true},
		// Exact-match whitelist entries keep working.
		{name: "whitelisted codex max", model: "gpt-5.1-codex-max", preserve: true},
		{name: "whitelisted yi medium", model: "yi-medium", preserve: true},
		// Synthetic effort aliases must still parse, including Qwen bases on a
		// tail the vendor does not ship.
		{name: "grok effort alias", model: "grok-4.20-multi-agent-high", preserve: false},
		{name: "gpt effort alias", model: "gpt-5.1-high", preserve: false},
		{name: "qwen alias on another tail", model: "qwen3.8-plus-high", preserve: false},
		// Dated/pinned IDs carry no effort tail, so the rule must not match them.
		{name: "dated qwen id", model: "qwen3.8-max-0902", preserve: false},
		{name: "empty name", model: "", preserve: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.preserve, ShouldPreserveEffortTail(tt.model))
		})
	}
}
