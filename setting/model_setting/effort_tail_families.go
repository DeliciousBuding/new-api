package model_setting

import "strings"

// effortTailRealModelFamilies lists vendor families whose *real* model IDs end
// in a token that the OpenAI effort-suffix parser would otherwise read as a
// reasoning alias. Alibaba's Qwen tiers are the live case: qwen-max,
// qwen3.7-max, qwen3.8-max and qwen-vl-max are genuine model IDs, but
// ParseOpenAIReasoningEffortFromModelSuffix reads qwen3.8-max as base "qwen3.8"
// plus effort "max". That both rejects clients which send their own
// reasoning_effort (400 "reasoning settings conflict") and silently injects
// effort=max into requests that asked for nothing.
//
// Catalog shape cannot separate a real ID from a synthetic alias: an alias such
// as grok-4.20-multi-agent-high is registered in channel model lists exactly
// like a real model, and its base name is not registered either. So the rule is
// deliberately narrow - one vendor prefix plus the one tail that vendor really
// ships - instead of a "is this name routable" heuristic. EffortTailModelIDs
// (exact match, operator-editable) stays the escape hatch for one-off names
// such as gpt-5.1-codex-max.
var effortTailRealModelFamilies = []struct {
	prefix string
	tail   string
}{
	{prefix: "qwen", tail: "-max"},
}

// isEffortTailRealModelFamily reports whether a model name, already stripped of
// any "vendor/" prefix by the caller, is a real model ID from a family whose
// names legitimately end in an effort-like token.
func isEffortTailRealModelFamily(bareModelName string) bool {
	for _, family := range effortTailRealModelFamilies {
		if strings.HasPrefix(bareModelName, family.prefix) && strings.HasSuffix(bareModelName, family.tail) {
			return true
		}
	}
	return false
}
