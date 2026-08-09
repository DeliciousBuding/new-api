/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import {
  Brain,
  ChevronRight,
  Terminal,
  User,
  Wrench,
  Image as ImageIcon,
  AlertTriangle,
  FileText,
} from 'lucide-react'
import { useState, type ComponentProps, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { CodeBlock } from '@/components/ai-elements/code-block'
import { Badge } from '@/components/ui/badge'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { IconBadge, type IconBadgeTone } from '@/components/ui/icon-badge'
import { Markdown } from '@/components/ui/markdown'
import { cn } from '@/lib/utils'

import type { ObserverCanonicalItem, ObserverCanonicalPart } from '../types'

export type ProviderFamily =
  | 'claude_cli'
  | 'codex_cli'
  | 'codex_desktop'
  | 'unknown'

interface ProviderStyle {
  label: string
  tone: IconBadgeTone
  rail: string
}

function providerStyle(family: ProviderFamily): ProviderStyle {
  switch (family) {
    case 'claude_cli':
      return {
        label: 'Claude Code',
        tone: 'chart-4',
        rail: 'border-chart-4/60',
      }
    case 'codex_cli':
      return { label: 'Codex CLI', tone: 'chart-1', rail: 'border-chart-1/60' }
    case 'codex_desktop':
      return {
        label: 'Codex Desktop',
        tone: 'chart-2',
        rail: 'border-chart-2/60',
      }
    default:
      return { label: 'Unknown', tone: 'neutral', rail: 'border-border' }
  }
}

function ProviderBadge({ family }: { family: ProviderFamily }) {
  const style = providerStyle(family)
  return (
    <Badge variant='outline' className='gap-1.5 text-[.65rem]'>
      <IconBadge tone={style.tone} size='xs' decorative>
        <Brain className='size-3' />
      </IconBadge>
      {style.label}
    </Badge>
  )
}

function shortenHmac(hmac: string): string {
  return hmac.length > 16 ? `${hmac.slice(0, 8)}...${hmac.slice(-4)}` : hmac
}

function stringifyValue(value: unknown): string {
  if (typeof value === 'string') return value
  try {
    return JSON.stringify(value, null, 2) ?? String(value)
  } catch {
    return String(value)
  }
}

interface GapInfo {
  position?: string
  reason?: string
  omitted_items?: number
  logical_bytes?: number
  source_truncated?: boolean
}

function GapMarker({ item }: { item: ObserverCanonicalItem }) {
  const { t } = useTranslation()
  const gap = (item as ObserverCanonicalItem & { gap?: GapInfo }).gap
  const reason = gap?.reason ?? 'unavailable'
  return (
    <div className='border-warning/40 bg-warning/5 text-warning-foreground/80 flex items-center gap-2 rounded-lg border border-dashed px-3 py-2 text-xs'>
      <AlertTriangle className='text-warning size-3.5 shrink-0' />
      <span className='font-medium'>{t('Gap')}</span>
      <span className='text-muted-foreground'>
        {gap?.omitted_items
          ? t('{{n}} items omitted', { n: gap.omitted_items })
          : t('content unavailable')}
      </span>
      {reason ? (
        <span className='text-muted-foreground/70'>- {reason}</span>
      ) : null}
    </div>
  )
}

function SystemBlock({ item }: { item: ObserverCanonicalItem }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const text = item.content?.map((p) => p.text ?? '').join('\n') ?? ''
  const preview = text.slice(0, 120).replaceAll(/\s+/g, ' ')
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className='group bg-muted/40 hover:bg-muted/60 flex w-full items-center gap-2 rounded-lg px-3 py-2 text-left text-xs'>
        <IconBadge tone='neutral' size='xs' decorative>
          <FileText className='size-3' />
        </IconBadge>
        <span className='text-muted-foreground font-medium'>{t('System')}</span>
        <span className='text-muted-foreground/70 truncate'>
          {preview}
          {text.length > 120 ? '...' : ''}
        </span>
        <ChevronRight className='text-muted-foreground ml-auto size-3.5 transition-transform group-data-[state=open]:rotate-90' />
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className='bg-muted/20 mt-2 rounded-lg border p-3'>
          <Markdown className='prose-sm max-w-none'>{text}</Markdown>
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

function MediaPart({ part }: { part: ObserverCanonicalPart }) {
  const { t } = useTranslation()
  const media = part.media
  if (!media) return null
  return (
    <div className='bg-muted/20 text-muted-foreground flex items-center gap-2 rounded-lg border border-dashed px-3 py-2 text-xs'>
      <IconBadge tone='neutral' size='xs' decorative>
        <ImageIcon className='size-3' />
      </IconBadge>
      <span className='font-medium'>{t('Media')}</span>
      <span>{media.kind}</span>
      {media.media_type ? <span>- {media.media_type}</span> : null}
      <span className='tabular-nums'>
        - {media.logical_bytes.toLocaleString()} {t('bytes')}
      </span>
      <span className='text-muted-foreground/60 font-mono text-[.65rem]'>
        {shortenHmac(media.hmac)}
      </span>
    </div>
  )
}

interface ToolCallInfo {
  id?: string
  name?: string
  arguments?: unknown
}
interface ToolResultInfo {
  tool_call_id?: string
  output?: unknown
}

function ToolCallCard({
  call,
  result,
}: {
  call: ToolCallInfo
  result?: ToolResultInfo
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const argsString = stringifyValue(call.arguments)
  const outputString = result ? stringifyValue(result.output) : ''
  const name = call.name ?? call.id ?? t('tool')
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className='group bg-card hover:bg-muted/40 flex w-full items-center gap-2 rounded-lg border px-3 py-2 text-left text-xs'>
        <IconBadge tone='primary' size='xs' decorative>
          <Wrench className='size-3' />
        </IconBadge>
        <span className='font-mono font-medium'>{name}</span>
        <ChevronRight className='text-muted-foreground ml-auto size-3.5 transition-transform group-data-[state=open]:rotate-90' />
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className='mt-2 space-y-2'>
          {argsString ? (
            <div>
              <div className='text-muted-foreground mb-1 text-[.65rem] font-medium tracking-wide uppercase'>
                {t('Arguments')}
              </div>
              <CodeBlock
                code={argsString}
                language='json'
                collapsedLines={12}
                defaultCollapsed={argsString.split('\n').length > 12}
                maxExpandedLines={40}
                showLineNumbers={false}
                showToolbar
              />
            </div>
          ) : null}
          {result ? (
            <div>
              <div className='text-muted-foreground mb-1 text-[.65rem] font-medium tracking-wide uppercase'>
                {t('Output')}
              </div>
              <CodeBlock
                code={outputString}
                language='text'
                collapsedLines={10}
                defaultCollapsed={outputString.split('\n').length > 10}
                maxExpandedLines={40}
                showLineNumbers={false}
                showToolbar
              />
            </div>
          ) : null}
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

function roleStyle(role: string | undefined): {
  label: string
  tone: IconBadgeTone
  icon: typeof User
  align: 'left' | 'right'
} {
  if (role === 'assistant') {
    return { label: 'Assistant', tone: 'primary', icon: Brain, align: 'left' }
  }
  if (role === 'user') {
    return { label: 'User', tone: 'chart-3', icon: User, align: 'right' }
  }
  if (role === 'developer') {
    return {
      label: 'Developer',
      tone: 'info',
      icon: Terminal,
      align: 'left',
    }
  }
  return {
    label: role ?? 'message',
    tone: 'neutral',
    icon: User,
    align: 'left',
  }
}

function MessageCard({ item }: { item: ObserverCanonicalItem }) {
  const { t } = useTranslation()
  const style = roleStyle(item.role)
  const RoleIcon = style.icon
  const parts = item.content ?? []
  const textParts = parts.filter((p) => p.type === 'text' && p.text)
  const mediaParts = parts.filter((p) => p.type === 'media')
  const toolParts = parts.filter(
    (p) => p.type === 'tool_call' || p.type === 'tool_result'
  )
  const text = textParts.map((p) => p.text ?? '').join('\n\n')
  return (
    <div
      className={cn(
        'flex gap-2.5',
        style.align === 'right' && 'flex-row-reverse'
      )}
    >
      <IconBadge tone={style.tone} size='sm' className='mt-0.5 shrink-0'>
        <RoleIcon className='size-3.5' />
      </IconBadge>
      <div
        className={cn(
          'min-w-0 max-w-[85%] rounded-lg border px-3.5 py-2.5',
          style.align === 'right' ? 'bg-accent/60' : 'bg-card'
        )}
      >
        <div className='text-muted-foreground mb-1 text-[.65rem] font-medium tracking-wide uppercase'>
          {style.label}
          {item.truncated ? (
            <Badge variant='warning' className='ml-2 text-[.6rem]'>
              {t('Truncated')}
            </Badge>
          ) : null}
        </div>
        {text ? (
          <Markdown className='prose-sm max-w-none'>{text}</Markdown>
        ) : null}
        {mediaParts.length > 0 ? (
          <div className='mt-2 space-y-1.5'>
            {mediaParts.map((p) => (
              <MediaPart
                key={`${p.type}-${p.hmac ?? p.logical_bytes}`}
                part={p}
              />
            ))}
          </div>
        ) : null}
        {toolParts.length > 0 ? (
          <div className='mt-2 space-y-1.5'>
            {toolParts.map((p) => {
              if (p.type === 'tool_call' && p.call) {
                return (
                  <ToolCallCard
                    key={`tc-${p.hmac ?? p.logical_bytes}`}
                    call={p.call}
                  />
                )
              }
              if (p.type === 'tool_result' && p.result) {
                return (
                  <ToolCallCard
                    key={`tr-${p.hmac ?? p.logical_bytes}`}
                    call={{ id: p.result.tool_call_id }}
                    result={p.result}
                  />
                )
              }
              return null
            })}
          </div>
        ) : null}
        <div className='text-muted-foreground/50 mt-2 font-mono text-[.6rem]'>
          {item.logical_bytes.toLocaleString()} {t('bytes')} -{' '}
          {shortenHmac(item.hmac)}
        </div>
      </div>
    </div>
  )
}

function ItemView({ item }: { item: ObserverCanonicalItem }) {
  switch (item.kind) {
    case 'system':
      return <SystemBlock item={item} />
    case 'gap':
      return <GapMarker item={item} />
    case 'message':
      return <MessageCard item={item} />
    case 'tool_call': {
      const callPart = item.content?.find((p) => p.type === 'tool_call')
      return (
        <ToolCallCard
          call={{
            id: item.tool_call_id ?? callPart?.call?.id,
            name: callPart?.call?.name,
            arguments: callPart?.call?.arguments,
          }}
        />
      )
    }
    case 'tool_result': {
      const resultPart = item.content?.find((p) => p.type === 'tool_result')
      return (
        <ToolCallCard
          call={{ id: resultPart?.result?.tool_call_id }}
          result={resultPart?.result}
        />
      )
    }
    default:
      return (
        <div className='text-muted-foreground rounded-lg border border-dashed px-3 py-2 text-xs'>
          {item.kind} - {item.logical_bytes.toLocaleString()} bytes -{' '}
          {shortenHmac(item.hmac)}
        </div>
      )
  }
}

export interface TurnStreamProps {
  items: ObserverCanonicalItem[]
  family: ProviderFamily
  occurredAt?: string
  model?: string
  success?: boolean
  latencyMs?: number
}

export function TurnStream(props: TurnStreamProps) {
  const { t } = useTranslation()
  const style = providerStyle(props.family)
  return (
    <div
      className={cn(
        'space-y-3 rounded-lg border-l-2 bg-card/30 p-3 pl-4',
        style.rail
      )}
    >
      <div className='text-muted-foreground flex flex-wrap items-center gap-2 text-[.65rem]'>
        <ProviderBadge family={props.family} />
        {props.model ? <span className='font-mono'>{props.model}</span> : null}
        {props.occurredAt ? (
          <span className='tabular-nums'>{props.occurredAt.slice(11, 19)}</span>
        ) : null}
        {props.success === false ? (
          <Badge variant='destructive' className='text-[.6rem]'>
            {t('Failed')}
          </Badge>
        ) : null}
        {props.latencyMs != null && props.latencyMs >= 0 ? (
          <span className='tabular-nums'>
            {props.latencyMs >= 1000
              ? `${(props.latencyMs / 1000).toFixed(2)}s`
              : `${Math.round(props.latencyMs)}ms`}
          </span>
        ) : null}
      </div>
      <div className='space-y-2.5'>
        {props.items.map((item) => (
          <ItemView key={`${item.kind}-${item.hmac}`} item={item} />
        ))}
      </div>
    </div>
  )
}

export type { ComponentProps, ReactNode }
