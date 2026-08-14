import { Bot, Braces, Repeat, Smartphone } from 'lucide-react'
import type { ComponentType } from 'react'

import { StatusBadge } from '@/components/status-badge'
import { getLobeIcon } from '@/lib/lobe-icon'
import { cn } from '@/lib/utils'

import { clientProfileLabel } from '../lib/format'
import type { ClientProfile } from '../types'

// Brand icons come from the same @lobehub/icons set the Model badge uses.
// Profiles without a brand icon fall back to a neutral lucide glyph so the
// chip still reads as a client identity.
export const BRAND_ICONS: Partial<Record<ClientProfile, string>> = {
  codex_cli: 'Codex.Color',
  codex_desktop: 'Codex.Color',
  codex_app: 'Codex.Color',
  codex_vscode: 'Codex.Color',
  codex_browser: 'Codex.Color',
  claude_cli: 'ClaudeCode.Color',
  claude_desktop: 'Claude.Color',
  claude_plugin: 'Claude.Color',
  claude_app: 'Claude.Color',
  claude_sdk: 'Claude.Color',
  openai_sdk: 'OpenAI.Color',
  openclaw: 'OpenClaw.Color',
  cherry_studio: 'CherryStudio.Color',
  opencode: 'OpenCode.Color',
  trae: 'Trae.Color',
  cursor: 'Cursor.Color',
  windsurf: 'Windsurf.Color',
  cline: 'Cline.Color',
  roo_code: 'RooCode.Color',
  copilot: 'Copilot.Color',
  gemini_cli: 'GeminiCLI.Color',
  gemini_code_assist: 'Gemini.Color',
  gemini_sdk: 'Gemini.Color',
  qoder: 'Qoder.Color',
  openai_agents: 'OpenAI.Color',
  semantic_kernel: 'Microsoft.Color',
  langchain: 'LangChain.Color',
  llama_index: 'LlamaIndex.Color',
  mistral_sdk: 'Mistral.Color',
  cohere_sdk: 'Cohere.Color',
  // Grok/MCP 无 Color 变体（仅 mono），'Grok.Color' 经 getLobeIcon 静默降级为
  // mono 标志，与 model-badge 的 Grok 用法保持一致；HermesAgent/Vercel 同样无
  // Color，用 .Avatar（圆形彩色头像）替代。
  grok: 'Grok.Color',
  mcp_sdk: 'MCP',
  hermes_agent: 'HermesAgent.Avatar',
  ai_sdk: 'Vercel.Avatar',
  n8n: 'N8n.Color',
  zapier: 'Zapier.Color',
  make: 'Make.Color',
  perplexity: 'Perplexity.Color',
  poe: 'Poe.Color',
  openrouter: 'OpenRouter.Color',
  groq: 'Groq.Color',
  ollama: 'Ollama.Color',
  kimi: 'Kimi.Color',
  qwen: 'Qwen.Color',
  doubao: 'Doubao.Color',
  zhipu: 'Zhipu.Color',
  sub2api: 'Sub2API',
  deepseek_harness: 'DeepSeek.Color',
  deepseek: 'DeepSeek.Color',
  chatgpt: 'ChatGPT.Color',
}

const FALLBACK_ICONS: Partial<
  Record<ClientProfile, ComponentType<{ className?: string }>>
> = {
  gohttp: Bot,
  cliproxyapi: Repeat,
  chat: Smartphone,
  workbuddy: Bot,
  rikkahub: Smartphone,
  minis: Smartphone,
  continue: Braces,
  zed: Braces,
  litellm: Repeat,
  http_client: Braces,
}

interface ClientProfileBadgeProps {
  profile: ClientProfile
  className?: string
  /** Compact text row for the details dialog; no chip border. */
  compact?: boolean
}

export function ClientProfileBadge({
  profile,
  className,
  compact,
}: ClientProfileBadgeProps) {
  const brandIcon = BRAND_ICONS[profile]
  const FallbackIcon = FALLBACK_ICONS[profile]

  const content = (
    <>
      {brandIcon && (
        <span
          className='flex h-[18px] w-[18px] shrink-0 items-center justify-center'
          aria-hidden='true'
        >
          {getLobeIcon(brandIcon, 18)}
        </span>
      )}
      {!brandIcon && FallbackIcon && (
        <span className='text-muted-foreground' aria-hidden='true'>
          <FallbackIcon className='size-3.5 shrink-0' />
        </span>
      )}
      <span className='whitespace-nowrap'>{clientProfileLabel(profile)}</span>
    </>
  )

  if (compact) {
    return (
      <span
        title={`client_profile: ${profile}`}
        className={cn('inline-flex items-center gap-1.5 text-xs', className)}
      >
        {content}
      </span>
    )
  }

  return (
    <StatusBadge
      size='sm'
      showDot={!brandIcon && !FallbackIcon}
      title={`client_profile: ${profile}`}
      className={cn(
        'border-border/60 bg-muted/30 h-6 max-w-none gap-1.5 rounded-md border px-2 [font-family:var(--font-body)]',
        className
      )}
    >
      <span className='flex max-w-none items-center gap-1.5'>{content}</span>
    </StatusBadge>
  )
}
