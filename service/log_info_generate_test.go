package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func ginCtxWithHeaders(headers map[string]string) *gin.Context {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	return c
}

func TestDetectClientProfile(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name: "codex turn state header",
			headers: map[string]string{
				"X-Codex-Turn-State": "abc",
			},
			want: "codex",
		},
		{
			name: "codex originator value prefix",
			headers: map[string]string{
				"Originator": "codex_cli_rs",
			},
			want: "codex",
		},
		{
			name: "arbitrary originator is not codex",
			headers: map[string]string{
				"Originator": "curl/8.0",
			},
			want: "chat",
		},
		{
			name: "anthropic version header",
			headers: map[string]string{
				"Anthropic-Version": "2023-06-01",
			},
			want: "claude",
		},
		{
			name: "openai sdk stainless headers only are not claude",
			headers: map[string]string{
				"X-Stainless-Lang":           "python",
				"X-Stainless-Runtime":        "CPython",
				"X-Stainless-Package-Version": "1.0.0",
			},
			want: "chat",
		},
		{
			name:    "no headers is chat",
			headers: map[string]string{},
			want:    "chat",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectClientProfile(ginCtxWithHeaders(tc.headers))
			if got != tc.want {
				t.Fatalf("DetectClientProfile(%v) = %q, want %q", tc.headers, got, tc.want)
			}
		})
	}
}

func TestDetectClientProfileNilCtx(t *testing.T) {
	if got := DetectClientProfile(nil); got != "" {
		t.Fatalf("DetectClientProfile(nil) = %q, want empty", got)
	}
}
