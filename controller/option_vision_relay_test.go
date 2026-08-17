package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func visionRelayBulkBody(overrides map[string]string) string {
	base := map[string]string{
		"enabled":             "false",
		"structured":          "true",
		"structured_prompt":   "",
		"target_models":       "[]",
		"models":              `["gemma-4-31b"]`,
		"base_url":            "http://127.0.0.1:3000",
		"api_key":             "",
		"prompt":              "",
		"timeout_sec":         "15",
		"sidecall_secret":     "",
		"disable_proxy_fetch": "false",
	}
	for k, v := range overrides {
		base[k] = v
	}
	jsonBytes, err := common.Marshal(base)
	if err != nil {
		panic(err)
	}
	return string(jsonBytes)
}

func TestUpdateOptionRejectsNonBoolVisionRelayStructured(t *testing.T) {
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/",
		strings.NewReader(`{"key":"vision_relay.structured","value":"yes"}`),
	)

	UpdateOption(context)

	assert.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.False(t, payload.Success)
	assert.Contains(t, payload.Message, "vision_relay.structured")
}

func TestUpdateOptionRejectsNonBoolVisionRelayDisableProxyFetch(t *testing.T) {
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/",
		strings.NewReader(`{"key":"vision_relay.disable_proxy_fetch","value":"maybe"}`),
	)

	UpdateOption(context)

	assert.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.False(t, payload.Success)
	assert.Contains(t, payload.Message, "vision_relay.disable_proxy_fetch")
}

func TestUpdateOptionRejectsOversizedVisionRelayPrompt(t *testing.T) {
	oversized := strings.Repeat("x", 8193)
	body := `{"key":"vision_relay.prompt","value":"` + oversized + `"}`

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/",
		strings.NewReader(body),
	)

	UpdateOption(context)

	assert.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.False(t, payload.Success)
	assert.Contains(t, payload.Message, "vision_relay.prompt")
}

func TestUpdateVisionRelayOptionsRejectsNonBoolStructured(t *testing.T) {
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/vision_relay",
		strings.NewReader(visionRelayBulkBody(map[string]string{"structured": "yes"})),
	)

	UpdateVisionRelayOptions(context)

	assert.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.False(t, payload.Success)
	assert.Contains(t, payload.Message, "vision_relay.structured")
}

func TestUpdateVisionRelayOptionsRejectsEnabledWithoutSidecallSecret(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	common.OptionMap["vision_relay.sidecall_secret"] = ""
	common.OptionMap["vision_relay.api_key"] = ""
	common.OptionMapRWMutex.Unlock()

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/vision_relay",
		strings.NewReader(visionRelayBulkBody(map[string]string{
			"enabled":         "true",
			"sidecall_secret": "",
			"api_key":         "sk-test-key-1234",
		})),
	)

	UpdateVisionRelayOptions(context)

	assert.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.False(t, payload.Success)
	assert.Contains(t, payload.Message, "sidecall_secret")
}

func TestUpdateVisionRelayOptionsRejectsEnabledWithoutBaseURL(t *testing.T) {
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/vision_relay",
		strings.NewReader(visionRelayBulkBody(map[string]string{
			"enabled":         "true",
			"base_url":        "",
			"sidecall_secret": "this-is-a-valid-secret-key",
			"api_key":         "sk-test-key-1234",
		})),
	)

	UpdateVisionRelayOptions(context)

	assert.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.False(t, payload.Success)
	assert.Contains(t, payload.Message, "base_url")
}
