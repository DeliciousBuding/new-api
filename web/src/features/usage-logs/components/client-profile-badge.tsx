import { Braces, MessageSquare, Terminal } from 'lucide-react'
import type { ComponentType } from 'react'

import { StatusBadge } from '@/components/status-badge'
import { getLobeIcon } from '@/lib/lobe-icon'
import { cn } from '@/lib/utils'

import { clientProfileLabel } from '../lib/format'
import type { ClientProfile } from '../types'

// Brand icons come from the same @lobehub/icons set the Model badge uses.
// Profiles without a brand (gohttp / cliproxyapi / chat) fall back to a
// neutral lucide glyph so the chip still reads as a client identity.
const BRAND_ICONS: Partial<Record<ClientProfile, string>> = {
  codex_cli: 'Codex.Color',
  codex_desktop: 'Codex.Color',
  codex_app: 'Codex.Color',
  claude_cli: 'Claude.Color',
  claude_desktop: 'Claude.Color',
  claude_plugin: 'Claude.Color',
  claude_app: 'Claude.Color',
  claude_sdk: 'Claude.Color',
  openai_sdk: 'OpenAI.Color',
}

const FALLBACK_ICONS: Partial<Record<ClientProfile, ComponentType<{ className?: string }>>> = {
  gohttp: Terminal,
  cliproxyapi: Braces,
  chat: MessageSquare,
}

interface ClientProfileBadgeProps {
  profile: ClientProfile
  className?: string
}

export function ClientProfileBadge({ profile, className }: ClientProfileBadgeProps) {
  const brandIcon = BRAND_ICONS[profile]
  const FallbackIcon = FALLBACK_ICONS[profile]

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
      <span className='flex max-w-none items-center gap-1.5'>
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
      </span>
    </StatusBadge>
  )
}
