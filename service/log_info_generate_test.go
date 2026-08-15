package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
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
			name: "codex desktop originator with space",
			headers: map[string]string{
				"Originator": "Codex Desktop",
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
			name: "claude vscode x-app",
			headers: map[string]string{
				"X-App": "vscode",
			},
			want: "claude_vscode",
		},
		{
			name: "claude cli x-app with vscode ua variant",
			headers: map[string]string{
				"X-App":      "cli",
				"User-Agent": "claude-cli/2.1.232 (external, claude-vscode, agent-sdk/0.3.232)",
			},
			want: "claude_vscode",
		},
		{
			name: "claude cli x-app with desktop-3p ua variant",
			headers: map[string]string{
				"X-App":      "cli",
				"User-Agent": "claude-cli/2.1.227 (external, claude-desktop-3p, agent-sdk/0.3.227)",
			},
			want: "claude_desktop_3p",
		},
		{
			name: "claude cli x-app with plain cli ua",
			headers: map[string]string{
				"X-App":      "cli",
				"User-Agent": "claude-cli/2.1.154 (external, cli)",
			},
			want: "claude_cli",
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
			name: "anthropic ts sdk ua beats stainless headers",
			headers: map[string]string{
				"User-Agent":                  "Anthropic/JS 0.39.0",
				"X-Stainless-Lang":            "typescript",
				"X-Stainless-Runtime":         "node",
				"X-Stainless-Package-Version": "0.39.0",
			},
			want: "claude_sdk",
		},
		{
			name: "anthropic python sdk ua beats stainless headers",
			headers: map[string]string{
				"User-Agent":          "Anthropic/Python 0.58.0",
				"X-Stainless-Lang":    "python",
				"X-Stainless-Runtime": "CPython",
			},
			want: "claude_sdk",
		},
		{
			name: "anthropic go sdk ua beats stainless headers",
			headers: map[string]string{
				"User-Agent":          "Anthropic/Go 0.1.0",
				"X-Stainless-Lang":    "go",
				"X-Stainless-Runtime": "go1.23.4",
			},
			want: "claude_sdk",
		},
		{
			name: "anthropic sdk package ua beats stainless headers",
			headers: map[string]string{
				"User-Agent":          "anthropic-sdk-typescript/0.39.0",
				"X-Stainless-Lang":    "typescript",
				"X-Stainless-Runtime": "node",
			},
			want: "claude_sdk",
		},
		{
			name: "anthropic ts sdk ua without stainless headers",
			headers: map[string]string{
				"User-Agent": "Anthropic/JS 0.39.0",
			},
			want: "claude_sdk",
		},
		{
			name: "openai python ua still openai sdk with stainless headers",
			headers: map[string]string{
				"User-Agent":          "OpenAI/Python 1.71.0",
				"X-Stainless-Lang":    "python",
				"X-Stainless-Runtime": "CPython",
			},
			want: "openai_sdk",
		},
		{
			name: "google genai sdk ua",
			headers: map[string]string{
				"User-Agent": "google-genai-sdk/1.68.0 gl-python/3.13.3",
			},
			want: "gemini_sdk",
		},
		{
			name: "google generativeai legacy ua",
			headers: map[string]string{
				"User-Agent": "genai-py/0.8.7",
			},
			want: "gemini_sdk",
		},
		{
			name: "qoder cli ua",
			headers: map[string]string{
				"User-Agent": "Qoder-Cli/1.0",
			},
			want: "qoder",
		},
		{
			name: "codex cli originator with space",
			headers: map[string]string{
				"Originator": "Codex CLI",
			},
			want: "codex_cli",
		},
		{
			name: "mistral sdk ua",
			headers: map[string]string{
				"User-Agent": "mistral-client-python/1.2.0",
			},
			want: "mistral_sdk",
		},
		{
			name: "mistral sdk ua beats stainless headers",
			headers: map[string]string{
				"User-Agent":          "mistralai/1.2.0",
				"X-Stainless-Lang":    "python",
				"X-Stainless-Runtime": "CPython",
			},
			want: "mistral_sdk",
		},
		{
			name: "groq sdk ua beats stainless headers",
			headers: map[string]string{
				"User-Agent":          "Groq/Python 0.15.0",
				"X-Stainless-Lang":    "python",
				"X-Stainless-Runtime": "CPython",
			},
			want: "groq",
		},
		{
			name: "cohere sdk ua",
			headers: map[string]string{
				"User-Agent": "cohere-python/5.9.0",
			},
			want: "cohere_sdk",
		},
		{
			name: "vercel ai sdk ua",
			headers: map[string]string{
				"User-Agent": "@ai-sdk/openai/1.0.0 runtime/node.js/22",
			},
			want: "ai_sdk",
		},
		{
			name: "vertex ai platform ua",
			headers: map[string]string{
				"User-Agent": "google-cloud-aiplatform/1.80.0",
			},
			want: "gemini_sdk",
		},
		{
			name: "litellm ua",
			headers: map[string]string{
				"User-Agent": "litellm/1.60.0",
			},
			want: "litellm",
		},
		{
			name: "litellm header only",
			headers: map[string]string{
				"User-Agent":      "python-httpx/0.28.1",
				"X-LiteLLM-Key":   "sk-123",
				"X-LiteLLM-Model": "gpt-4o",
			},
			want: "litellm",
		},
		{
			name: "grok slash ua",
			headers: map[string]string{
				"User-Agent": "Grok/2.0 (Android)",
			},
			want: "grok",
		},
		{
			name: "llama index underscore ua",
			headers: map[string]string{
				"User-Agent": "llama_index/0.12.5",
			},
			want: "llama_index",
		},
		{
			name: "mcp typescript sdk ua",
			headers: map[string]string{
				"User-Agent": "mcp-typescript-sdk/1.0.0",
			},
			want: "mcp_sdk",
		},
		{
			name: "zapier ua",
			headers: map[string]string{
				"User-Agent": "Zapier/1.0",
			},
			want: "zapier",
		},
		{
			name: "make ua",
			headers: map[string]string{
				"User-Agent": "make.com/1.0",
			},
			want: "make",
		},
		{
			name: "bare make is not make",
			headers: map[string]string{
				"User-Agent": "make/1.0",
			},
			want: "chat",
		},
		{
			name: "bare grok is not grok",
			headers: map[string]string{
				"User-Agent": "grok foo",
			},
			want: "chat",
		},
		{
			name: "n8n via claude protocol stays n8n",
			headers: map[string]string{
				"User-Agent":        "n8n/1.80.0",
				"Anthropic-Version": "2023-06-01",
			},
			want: "n8n",
		},
		{
			name: "langchain via claude protocol stays langchain",
			headers: map[string]string{
				"User-Agent":        "langchain/0.3.14",
				"Anthropic-Version": "2023-06-01",
			},
			want: "langchain",
		},
		{
			name: "unknown ua with anthropic-version header falls back to claude sdk",
			headers: map[string]string{
				"User-Agent":        "CustomApp/1.0",
				"Anthropic-Version": "2023-06-01",
			},
			want: "claude_sdk",
		},
		{
			name: "unknown ua with stainless headers falls back to openai sdk",
			headers: map[string]string{
				"X-Stainless-Lang":    "python",
				"X-Stainless-Runtime": "CPython",
			},
			want: "openai_sdk",
		},
		{
			name: "langchain ua",
			headers: map[string]string{
				"User-Agent": "langchain/0.3.14",
			},
			want: "langchain",
		},
		{
			name: "openai agents sdk ua",
			headers: map[string]string{
				"User-Agent": "Agents/Python 0.1.5",
			},
			want: "openai_agents",
		},
		{
			name: "semantic kernel ua",
			headers: map[string]string{
				"User-Agent": "Semantic Kernel",
			},
			want: "semantic_kernel",
		},
		{
			name: "langchain py ua",
			headers: map[string]string{
				"User-Agent": "langchain-py/0.3.14",
			},
			want: "langchain",
		},
		{
			name: "llama index ua",
			headers: map[string]string{
				"User-Agent": "llama-index/0.12.5",
			},
			want: "llama_index",
		},
		{
			name: "xai grok ua",
			headers: map[string]string{
				"User-Agent": "Grok-User/1.0",
			},
			want: "grok",
		},
		{
			name: "mcp python sdk ua",
			headers: map[string]string{
				"User-Agent": "mcp-python-sdk/1.2.0",
			},
			want: "mcp_sdk",
		},
		{
			name: "n8n ua",
			headers: map[string]string{
				"User-Agent": "n8n/1.80.0",
			},
			want: "n8n",
		},
		{
			name: "python httpx ua",
			headers: map[string]string{
				"User-Agent": "python-httpx/0.28.1",
			},
			want: "http_client",
		},
		{
			name: "node fetch ua",
			headers: map[string]string{
				"User-Agent": "node-fetch/3.3.2",
			},
			want: "http_client",
		},
		{
			name: "codex tui originator",
			headers: map[string]string{
				"Originator": "codex-tui",
			},
			want: "codex_cli",
		},
		{
			name: "cliproxyapi openai-compat ua",
			headers: map[string]string{
				"User-Agent": "cli-proxy-openai-compat",
			},
			want: "cliproxyapi",
		},
		{
			name: "cliproxyapi kimi path ua",
			headers: map[string]string{
				"User-Agent": "CLIProxyAPI/7.2.112",
			},
			want: "cliproxyapi",
		},
		{
			name: "go http client ua",
			headers: map[string]string{
				"User-Agent": "Go-http-client/1.1",
			},
			want: "gohttp",
		},
		{
			name: "go http client h2 ua",
			headers: map[string]string{
				"User-Agent": "Go-http-client/2.0",
			},
			want: "gohttp",
		},
		{
			name: "plain newapi ua is not identified",
			headers: map[string]string{
				"User-Agent": "new-api/1.0.0",
			},
			want: "chat",
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
			name: "hermes agent ua",
			headers: map[string]string{
				"User-Agent": "HermesAgent/0.5.0 (linux; x86_64)",
			},
			want: "hermes_agent",
		},
		{
			name: "workbuddy ua",
			headers: map[string]string{
				"User-Agent": "WorkBuddy/1.2 (win32; x64)",
			},
			want: "workbuddy",
		},
		{
			name: "openclaw ua",
			headers: map[string]string{
				"User-Agent": "OpenClaw/0.9 (agent)",
			},
			want: "openclaw",
		},
		{
			name: "cherry studio ua (official string with space)",
			headers: map[string]string{
				"User-Agent": "Cherry Studio",
			},
			want: "cherry_studio",
		},
		{
			name: "rikkahub ua",
			headers: map[string]string{
				"User-Agent": "RikkaHub/2.0 (Android)",
			},
			want: "rikkahub",
		},
		{
			name: "sub2api discovery ua",
			headers: map[string]string{
				"User-Agent": "Sub2API-Discovery/1.0",
			},
			want: "sub2api",
		},
		{
			name: "opencode ua",
			headers: map[string]string{
				"User-Agent": "opencode/1.17.16 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
			},
			want: "opencode",
		},
		{
			name: "minis ua",
			headers: map[string]string{
				"User-Agent": "Minis/0.20-preview (Android 16; PLQ110)",
			},
			want: "minis",
		},
		{
			name: "trae ua",
			headers: map[string]string{
				"User-Agent": "Trae/1.0.12 (Windows; x86_64) Electron/42",
			},
			want: "trae",
		},
		{
			name: "cursor ua",
			headers: map[string]string{
				"User-Agent": "Cursor/1.4.2 (Windows; x86_64) Electron/37",
			},
			want: "cursor",
		},
		{
			name: "gohttp still wins over unknown brands",
			headers: map[string]string{
				"User-Agent": "Go-http-client/2.0",
			},
			want: "gohttp",
		},
		{
			name:    "windsurf ua",
			headers: map[string]string{"User-Agent": "Windsurf/2.0.1 (Windows; x86_64)"},
			want:    "windsurf",
		},
		{
			name:    "cline ua",
			headers: map[string]string{"User-Agent": "Cline/3.1.0 (VS Code)"},
			want:    "cline",
		},
		{
			name:    "roo code ua",
			headers: map[string]string{"User-Agent": "Roo-Code/3.0.0 (VS Code)"},
			want:    "roo_code",
		},
		{
			name:    "continue ua",
			headers: map[string]string{"User-Agent": "Continue/1.0.0 (VS Code)"},
			want:    "continue",
		},
		{
			name:    "zed ua",
			headers: map[string]string{"User-Agent": "Zed/0.180.0 (macos; x86_64)"},
			want:    "zed",
		},
		{
			name:    "copilot ua",
			headers: map[string]string{"User-Agent": "copilot/1.1 (github)"},
			want:    "copilot",
		},
		{
			name:    "codex desktop ua fallback",
			headers: map[string]string{"User-Agent": "Codex Desktop/0.146.0-alpha.3.1 (Windows 10.0.26200; x86_64)"},
			want:    "codex_desktop",
		},
		{
			name:    "codex vscode ua fallback",
			headers: map[string]string{"User-Agent": "codex_vscode/0.146.0-alpha.3 (Windows 10.0.26200; x86_64)"},
			want:    "codex_vscode",
		},
		{
			name:    "codex browser-use ua fallback",
			headers: map[string]string{"User-Agent": "codex-browser-use/0.146.0-alpha.3.1 (Windows)"},
			want:    "codex_browser",
		},
		{
			name:    "claude cli plain ua",
			headers: map[string]string{"User-Agent": "claude-cli/2.1.205 (external, sdk-ts, agent-sdk/0.3.205)"},
			want:    "claude_cli",
		},
		{
			name:    "claude cli desktop-3p ua",
			headers: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, claude-desktop-3p, agent-sdk/0.3.219)"},
			want:    "claude_desktop_3p",
		},
		{
			name:    "claude cli vscode ua",
			headers: map[string]string{"User-Agent": "claude-cli/2.1.185 (external, claude-vscode, agent-sdk/0.3.185)"},
			want:    "claude_vscode",
		},
		{
			name:    "claude desktop electron msix ua",
			headers: map[string]string{"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Claude/1.24012.1 Chrome/148.0.7778.280 Electron/42.7.0 Safari/537.36 MSIX"},
			want:    "claude_desktop",
		},
		{
			name:    "anthropic js ua",
			headers: map[string]string{"User-Agent": "Anthropic/JS 0.90.0"},
			want:    "claude_sdk",
		},
		{
			name: "claude cli with anthropic-version header stays claude_cli",
			headers: map[string]string{
				"User-Agent":        "claude-cli/2.1.205 (external, cli)",
				"Anthropic-Version": "2023-06-01",
			},
			want: "claude_cli",
		},
		{
			name: "claude desktop-3p ua with anthropic-version header",
			headers: map[string]string{
				"User-Agent":        "claude-cli/2.1.219 (external, claude-desktop-3p, agent-sdk/0.3.219)",
				"Anthropic-Version": "2023-06-01",
			},
			want: "claude_desktop_3p",
		},
		{
			name:    "openai python ua",
			headers: map[string]string{"User-Agent": "OpenAI/Python 2.24.0"},
			want:    "openai_sdk",
		},
		{
			name:    "gemini cli ua",
			headers: map[string]string{"User-Agent": "Gemini-CLI/0.9.0 (linux)"},
			want:    "gemini_cli",
		},
		{
			name:    "gemini code assist ua",
			headers: map[string]string{"User-Agent": "CloudCodeVSCode/1.0 (aidev_client; os_type=linux; host_path=VSCode/1.90)"},
			want:    "gemini_code_assist",
		},
		{
			name:    "perplexity ua",
			headers: map[string]string{"User-Agent": "Perplexity-User/1.0"},
			want:    "perplexity",
		},
		{
			name:    "poe ua",
			headers: map[string]string{"User-Agent": "Poe/1.0 (Android)"},
			want:    "poe",
		},
		{
			name:    "openrouter ua",
			headers: map[string]string{"User-Agent": "OpenRouter/1.0"},
			want:    "openrouter",
		},
		{
			name:    "groq ua",
			headers: map[string]string{"User-Agent": "groq/2.0 (python)"},
			want:    "groq",
		},
		{
			name:    "ollama ua",
			headers: map[string]string{"User-Agent": "ollama/0.5.0 (linux)"},
			want:    "ollama",
		},
		{
			name:    "kimi ua",
			headers: map[string]string{"User-Agent": "Kimi/1.0 (Android)"},
			want:    "kimi",
		},
		{
			name:    "qwen ua",
			headers: map[string]string{"User-Agent": "Qwen/2.5 (Android)"},
			want:    "qwen",
		},
		{
			name:    "doubao ua",
			headers: map[string]string{"User-Agent": "Doubao/1.0 (Android)"},
			want:    "doubao",
		},
		{
			name:    "zhipu ua",
			headers: map[string]string{"User-Agent": "ChatGLM/1.0 (Android)"},
			want:    "zhipu",
		},
		{
			name:    "deepseek harness ua",
			headers: map[string]string{"User-Agent": "deepseek-harness/0.1.0-rc.5"},
			want:    "deepseek_harness",
		},
		{
			name:    "deepseek app ua",
			headers: map[string]string{"User-Agent": "DeepSeek/1.0 (Android)"},
			want:    "deepseek",
		},
		{
			name:    "chatgpt ua",
			headers: map[string]string{"User-Agent": "ChatGPT/1.2026.0 (Android)"},
			want:    "chatgpt",
		},
		{
			name:    "curl ua",
			headers: map[string]string{"User-Agent": "curl/8.7.1"},
			want:    "http_client",
		},
		{
			name:    "python requests ua",
			headers: map[string]string{"User-Agent": "python-requests/2.33.0"},
			want:    "http_client",
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
			require.Equal(t, tc.want, got, "DetectClientProfile(%v)", tc.headers)
		})
	}
}

func TestDetectClientProfileNilCtx(t *testing.T) {
	require.Empty(t, DetectClientProfile(nil))
}

func TestGenerateTextOtherInfoClientUA(t *testing.T) {
	cases := []struct {
		name        string
		ua          string
		wantLen     int
		wantPresent bool
	}{
		{name: "ua present", ua: "curl/8.7.1", wantPresent: true},
		{name: "ua absent", ua: "", wantPresent: false},
		{name: "ua over 256 chars is truncated", ua: "X/" + strings.Repeat("a", 300), wantLen: 256, wantPresent: true},
		// 255 ASCII + 一个 3 字节汉字 "界"（258 字节）：字节截断会切断该字符，
		// UTF-8 安全截断应回退到 255 字节的 ASCII 边界。
		{name: "multibyte ua truncated on utf8 boundary", ua: strings.Repeat("a", 255) + "界", wantLen: 255, wantPresent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tc.ua != "" {
				req.Header.Set("User-Agent", tc.ua)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = req
			// IsModelMapped 等字段经嵌入 *ChannelMeta 提升，零值 RelayInfo 需显式
			// 初始化嵌入指针（生产路径由 InitChannelMeta 设置）。
			other := GenerateTextOtherInfo(c, &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{},
			}, 1, 1, 1, 0, 0, 0, 1)
			adminInfo, ok := other["admin_info"].(map[string]interface{})
			require.True(t, ok, "admin_info should be present")
			uaVal, hasUA := adminInfo["client_ua"]
			if !tc.wantPresent {
				require.False(t, hasUA, "client_ua should be absent without UA")
				return
			}
			require.True(t, hasUA, "client_ua should be present")
			if tc.wantLen > 0 {
				require.Len(t, uaVal, tc.wantLen, "client_ua should be truncated to the expected length")
				require.True(t, utf8.ValidString(uaVal.(string)), "client_ua must stay valid UTF-8 after truncation")
				return
			}
			require.Equal(t, tc.ua, uaVal)
		})
	}
}
