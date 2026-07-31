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
			name: "codex cli originator",
			headers: map[string]string{
				"Originator": "codex_cli_rs",
			},
			want: "codex_cli",
		},
		{
			name: "codex desktop originator",
			headers: map[string]string{
				"Originator": "codex_desktop",
			},
			want: "codex_desktop",
		},
		{
			name: "codex turn state header with cli ua",
			headers: map[string]string{
				"X-Codex-Turn-State": "abc",
				"User-Agent":         "codex_cli_rs/0.1",
			},
			want: "codex_cli",
		},
		{
			name: "arbitrary originator is not codex",
			headers: map[string]string{
				"Originator": "curl/8.0",
			},
			want: "chat",
		},
		{
			name: "claude cli x-app",
			headers: map[string]string{
				"X-App": "cli",
			},
			want: "claude_cli",
		},
		{
			name: "claude desktop x-app",
			headers: map[string]string{
				"X-App": "desktop",
			},
			want: "claude_desktop",
		},
		{
			name: "claude vscode plugin x-app",
			headers: map[string]string{
				"X-App": "vscode",
			},
			want: "claude_plugin",
		},
		{
			name: "anthropic sdk version header only",
			headers: map[string]string{
				"Anthropic-Version": "2023-06-01",
			},
			want: "claude_sdk",
		},
		{
			name: "openai sdk stainless headers only",
			headers: map[string]string{
				"X-Stainless-Lang":            "python",
				"X-Stainless-Runtime":         "CPython",
				"X-Stainless-Package-Version": "1.0.0",
			},
			want: "openai_sdk",
		},
		{
			name: "go http client ua",
			headers: map[string]string{
				"User-Agent": "Go-http-client/1.1",
			},
			want: "gohttp",
		},
		{
			name: "cliproxyapi ua",
			headers: map[string]string{
				"User-Agent": "cliproxyapi/1.2.3",
			},
			want: "cliproxyapi",
		},
		{
			name: "newapi relay ua",
			headers: map[string]string{
				"User-Agent": "new-api/1.0.0",
			},
			want: "newapi",
		},
		{
			name: "codex header wins over generic ua",
			headers: map[string]string{
				"X-Codex-Turn-State": "abc",
				"User-Agent":         "Go-http-client/1.1",
			},
			want: "codex_cli",
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
