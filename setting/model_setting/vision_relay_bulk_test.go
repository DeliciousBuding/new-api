package model_setting

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func validEnabledSnapshot() map[string]string {
	return map[string]string{
		"vision_relay.enabled":             "true",
		"vision_relay.structured":          "true",
		"vision_relay.structured_prompt":   "",
		"vision_relay.target_models":       `["deepseek*"]`,
		"vision_relay.models":              `["gemma-4-31b"]`,
		"vision_relay.base_url":            "http://127.0.0.1:3000",
		"vision_relay.api_key":             "sk-test-key-1234",
		"vision_relay.prompt":              "",
		"vision_relay.timeout_sec":         "15",
		"vision_relay.sidecall_secret":     "this-is-a-valid-secret-key",
		"vision_relay.disable_proxy_fetch": "false",
	}
}

func TestValidateVisionRelayBulkSnapshotDisabledAllowsMalformedEndpoint(t *testing.T) {
	resolved := map[string]string{
		"vision_relay.enabled":             "false",
		"vision_relay.structured":          "not-a-bool",
		"vision_relay.structured_prompt":   "",
		"vision_relay.target_models":       "not-json",
		"vision_relay.models":              "also-not-json",
		"vision_relay.base_url":            "not-a-url",
		"vision_relay.api_key":             "",
		"vision_relay.prompt":              "",
		"vision_relay.timeout_sec":         "0",
		"vision_relay.sidecall_secret":     "",
		"vision_relay.disable_proxy_fetch": "maybe",
	}

	err := ValidateVisionRelayBulkSnapshot(resolved)
	assert.NoError(t, err)
}

func TestValidateVisionRelayBulkSnapshotEnabledAcceptsCompleteEndpoint(t *testing.T) {
	err := ValidateVisionRelayBulkSnapshot(validEnabledSnapshot())
	assert.NoError(t, err)
}

func TestValidateVisionRelayBulkSnapshotEnabledRejectsMissingSidecallSecret(t *testing.T) {
	resolved := validEnabledSnapshot()
	resolved["vision_relay.sidecall_secret"] = ""

	err := ValidateVisionRelayBulkSnapshot(resolved)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "sidecall_secret")
}

func TestValidateVisionRelayBulkSnapshotEnabledRejectsMissingBaseURL(t *testing.T) {
	resolved := validEnabledSnapshot()
	resolved["vision_relay.base_url"] = ""

	err := ValidateVisionRelayBulkSnapshot(resolved)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "base_url")
}

func TestValidateVisionRelayBulkSnapshotEnabledRejectsMalformedTargetModels(t *testing.T) {
	resolved := validEnabledSnapshot()
	resolved["vision_relay.target_models"] = "not-a-json-array"

	err := ValidateVisionRelayBulkSnapshot(resolved)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "target_models")
}

func TestValidateVisionRelayBulkSnapshotEnabledRejectsNonBoolStructured(t *testing.T) {
	resolved := validEnabledSnapshot()
	resolved["vision_relay.structured"] = "yes"

	err := ValidateVisionRelayBulkSnapshot(resolved)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "structured")
}

func TestValidateVisionRelayWriteRejectsOversizedPrompt(t *testing.T) {
	oversized := strings.Repeat("x", 8193)
	err := ValidateVisionRelayWrite("vision_relay.prompt", oversized)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "vision_relay.prompt")
}

func TestValidateVisionRelayWriteAcceptsNormalPrompt(t *testing.T) {
	err := ValidateVisionRelayWrite("vision_relay.prompt", "Describe this image.")
	assert.NoError(t, err)
}

func TestIsVisionRelaySecretKey(t *testing.T) {
	assert.True(t, IsVisionRelaySecretKey("vision_relay.api_key"))
	assert.True(t, IsVisionRelaySecretKey("vision_relay.sidecall_secret"))
	assert.False(t, IsVisionRelaySecretKey("vision_relay.enabled"))
	assert.False(t, IsVisionRelaySecretKey("vision_relay.prompt"))
}
