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
/**
 * Agent Timeline — the session rendered as an agent task execution flow,
 * not an LLM trace. Reuses the playground bubble vocabulary (Message +
 * getMessageContentStyles) for the chat surface, then layers agent-product
 * semantics on top: a task summary card, role-coded timeline nodes, and
 * collapsible tool nodes that hide raw JSON behind a "View raw" toggle.
 *
 * Design source of truth:
 *  - features/playground/components/message/playground-message-content.tsx
 *    (Message + MessageContent + Response, alignment vocabulary)
 *  - features/playground/lib/message/message-styles.ts (getMessageContentStyles)
 *  - components/ai-elements/code-block.tsx (CodeBlock for raw JSON inspection)
 */
import { useQuery } from '@tanstack/react-query'
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import {
  AlertTriangle,
  Check,
  ChevronRight,
  ChevronsUp,
  Loader2,
  Terminal,
  Wrench,
  Zap,
} from 'lucide-react'

import { CodeBlockFrame } from '@/components/ai-elements/code-block'
import { Message, MessageContent } from '@/components/ai-elements/message'
import { Response } from '@/components/ai-elements/response'
import {
  getMessageAlignmentClass,
  type MessageAlignment,
} from '@/features/playground/lib/message/message-layout-utils'
import { getMessageContentStyles } from '@/features/playground/lib/message/message-styles'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from '@/components/ui/empty'
import { Skeleton } from '@/components/ui/skeleton'
import { ClientProfileBadge } from '@/features/usage-logs/components/client-profile-badge'
import type { ClientProfile } from '@/features/usage-logs/types'
import dayjs from '@/lib/dayjs'
import { cn } from '@/lib/utils'

import { getSession, getSessionTranscript } from '../api'
import { observabilityQueryKeys } from '../query-keys'
import {
  isObserverDegraded,
  type ObserverSession,
  type ObserverTranscriptMessage,
  type ObserverToolCallRef,
  type ObserverToolResultRef,
} from '../types'

export interface SessionDetailTabProps {
  /** The session selected on the sessions list. `undefined` renders the
   * default empty state and no queries fire. */
  sessionId?: string | null
}

export function SessionDetailTab({ sessionId = null }: SessionDetailTabProps) {
  const { t } = useTranslation()

  if (!sessionId) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyTitle>{t('Session Detail')}</EmptyTitle>
          <EmptyDescription>
            {t('Select a session from the list to view its agent timeline.')}
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    )
  }

  return <AgentTimeline sessionId={sessionId} />
}

// ============================================================================
// Agent Timeline container: task summary + scrollable execution flow
// ============================================================================

const TRANSCRIPT_PAGE_SIZE = 50

function AgentTimeline({ sessionId }: { sessionId: string }) {
  const { t } = useTranslation()
  const [messages, setMessages] = useState<ObserverTranscriptMessage[]>([])
  const [prevCursor, setPrevCursor] = useState<number | null>(null)
  const [hasOlder, setHasOlder] = useState(false)
  const [isLoadingOlder, setIsLoadingOlder] = useState(false)
  const [loadError, setLoadError] = useState(false)
  // Bumped on every session switch so an in-flight loadOlder from the
  // previous session cannot splice its page into the new timeline (F-1:
  // cross-session race).
  const sessionEpoch = useRef(0)

  const latestQuery = useQuery({
    queryKey: observabilityQueryKeys.transcript.list(sessionId),
    queryFn: () =>
      getSessionTranscript(sessionId, { page_size: TRANSCRIPT_PAGE_SIZE }),
    retry: false,
  })

  useEffect(() => {
    sessionEpoch.current += 1
    setMessages([])
    setPrevCursor(null)
    setHasOlder(false)
    setLoadError(false)
  }, [sessionId])

  useEffect(() => {
    const page = latestQuery.data?.data
    if (!page || isObserverDegraded(page)) return
    setMessages(page.items)
    setPrevCursor(page.meta.prev_cursor)
    setHasOlder(page.meta.has_older)
  }, [latestQuery.data])

  const loadOlder = async () => {
    if (prevCursor == null || isLoadingOlder) return
    const epoch = sessionEpoch.current
    setIsLoadingOlder(true)
    setLoadError(false)
    try {
      const page = (
        await getSessionTranscript(sessionId, {
          direction: 'older',
          cursor: prevCursor,
          page_size: TRANSCRIPT_PAGE_SIZE,
        })
      ).data
      if (sessionEpoch.current !== epoch) return
      if (!page || isObserverDegraded(page)) {
        setLoadError(true)
        return
      }
      setMessages((prev) => [...page.items, ...prev])
      setPrevCursor(page.meta.prev_cursor)
      setHasOlder(page.meta.has_older)
    } catch {
      if (sessionEpoch.current === epoch) setLoadError(true)
    } finally {
      if (sessionEpoch.current === epoch) setIsLoadingOlder(false)
    }
  }

  const taskMeta = useMemo(() => deriveTaskMeta(messages), [messages])
  const groups = useMemo(() => groupTranscriptMessages(messages), [messages])

  // A degraded envelope keeps whatever messages are already on screen; the
  // alert tells the user the store was unavailable.
  const latestDegraded =
    latestQuery.data && isObserverDegraded(latestQuery.data.data)
      ? latestQuery.data.data
      : undefined

  // Session-level metadata feeds the task summary card.
  const sessionQuery = useQuery({
    queryKey: observabilityQueryKeys.sessions.detail(sessionId),
    queryFn: () => getSession(sessionId),
    retry: false,
  })
  const session =
    sessionQuery.data && !isObserverDegraded(sessionQuery.data.data)
      ? (sessionQuery.data.data ?? null)
      : null

  let content: ReactNode
  if (latestQuery.isLoading) {
    content = (
      <div className='space-y-3'>
        {Array.from({ length: 5 }, (_, i) => (
          <Skeleton
            key={`timeline-skeleton-${i}`}
            className={i % 2 === 0 ? 'h-16 w-2/3' : 'h-12 w-full'}
          />
        ))}
      </div>
    )
  } else if (latestQuery.isError) {
    content = (
      <Empty>
        <EmptyTitle>{t('Failed to load agent timeline')}</EmptyTitle>
      </Empty>
    )
  } else if (messages.length === 0) {
    content = (
      <Empty>
        <EmptyTitle>{t('No conversation recorded for this session yet.')}</EmptyTitle>
      </Empty>
    )
  } else {
    content = (
      <div className='flex flex-col gap-2'>
        {hasOlder && (
          <div className='flex justify-center pb-1'>
            <Button
              type='button'
              variant='ghost'
              size='sm'
              onClick={loadOlder}
              disabled={isLoadingOlder}
              className='text-muted-foreground gap-1.5 text-xs'
            >
              {isLoadingOlder ? (
                <>
                  <Loader2 className='size-3 animate-spin' aria-hidden='true' />
                  {t('Loading')}
                </>
              ) : (
                <>
                  <ChevronsUp className='size-3' aria-hidden='true' />
                  {t('Load earlier')}
                </>
              )}
            </Button>
          </div>
        )}
        {loadError && (
          <div className='flex justify-center pb-1'>
            <span className='text-muted-foreground text-xs'>
              {t('Failed to load earlier messages')}
            </span>
          </div>
        )}
        {groups.map((group) => (
          <TimelineNode
            key={`${group.item.hmac}-${group.item.turn_seq}-${group.item.seq}`}
            item={group.item}
            attachedResults={group.attachedResults}
          />
        ))}
      </div>
    )
  }

  return (
    <div className='space-y-3'>
      <TaskSummaryCard
        sessionId={sessionId}
        session={session}
        taskMeta={taskMeta}
      />
      {latestDegraded && <DegradedAlert data={latestDegraded} />}
      <Card>
        <CardContent className='py-4'>{content}</CardContent>
      </Card>
    </div>
  )
}

// ============================================================================
// Task summary card — the "what is this run" header
// ============================================================================

type TaskStatus = 'completed' | 'active' | 'incomplete' | 'truncated'

interface TaskMeta {
  status: TaskStatus
  toolCallCount: number
  turnCount: number
}

function deriveTaskMeta(messages: ObserverTranscriptMessage[]): TaskMeta {
  if (messages.length === 0) {
    return { status: 'active', toolCallCount: 0, turnCount: 0 }
  }
  const toolCallCount = messages.filter(
    (m) => m.role === 'assistant' && (m.content ?? []).some((p) => p.type === 'tool_call')
  ).length
  const hasGap = messages.some((m) => m.kind === 'gap' || m.gap)
  const last = messages.at(-1)
  const endsOnUser = last?.role === 'user'
  let status: TaskStatus = 'completed'
  if (hasGap) status = 'truncated'
  else if (endsOnUser) status = 'incomplete'
  return { status, toolCallCount, turnCount: messages.length }
}

function TaskSummaryCard({
  sessionId,
  session,
  taskMeta,
}: {
  sessionId: string
  session: ObserverSession | null
  taskMeta: TaskMeta
}) {
  const { t } = useTranslation()

  const durationText = useMemo(() => {
    if (!session) return null
    const start = dayjs(session.first_seen)
    const end = dayjs(session.last_seen)
    const seconds = end.diff(start, 'second', true)
    if (seconds < 1) return `<1s`
    if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)}s`
    const minutes = Math.floor(seconds / 60)
    const rem = Math.round(seconds % 60)
    return `${minutes}m${rem}s`
  }, [session])

  const statusBadge = (() => {
    switch (taskMeta.status) {
      case 'completed':
        return (
          <Badge
            variant='outline'
            className='gap-1 border-success/40 bg-success/10 text-success'
          >
            <Check className='size-3' aria-hidden='true' />
            {t('Completed')}
          </Badge>
        )
      case 'active':
        return (
          <Badge variant='secondary' className='gap-1'>
            <Loader2 className='size-3 animate-spin' aria-hidden='true' />
            {t('Active')}
          </Badge>
        )
      case 'incomplete':
        return <Badge variant='warning'>{t('Incomplete')}</Badge>
      case 'truncated':
        return (
          <Badge variant='warning' className='gap-1'>
            <AlertTriangle className='size-3' aria-hidden='true' />
            {t('Truncated')}
          </Badge>
        )
    }
  })()

  const stats: ReactNode[] = []
  if (durationText) stats.push(<span key='dur'>{durationText}</span>)
  if (taskMeta.toolCallCount > 0) {
    stats.push(
      <span key='tools' className='inline-flex items-center gap-1'>
        <Wrench className='size-3' aria-hidden='true' />
        {taskMeta.toolCallCount}
      </span>
    )
  }
  stats.push(
    <span key='turns' className='inline-flex items-center gap-1'>
      <Zap className='size-3' aria-hidden='true' />
      {taskMeta.turnCount} {t('turns')}
    </span>
  )

  return (
    <Card className='py-2'>
      <CardContent className='flex items-center justify-between gap-3 py-1.5'>
        <div className='flex min-w-0 items-center gap-2'>
          {session?.username ? (
            <>
              <span className='truncate text-sm font-medium'>
                {session.username}
              </span>
              {session.client_family && (
                <ClientProfileBadge
                  profile={session.client_family as ClientProfile}
                />
              )}
            </>
          ) : (
            <span className='text-sm font-medium'>{t('Agent Run')}</span>
          )}
          <span className='text-muted-foreground font-mono text-[11px]'>
            {sessionId.slice(0, 8)}
          </span>
        </div>
        <div className='flex shrink-0 items-center gap-2.5'>
          {statusBadge}
          <span className='text-muted-foreground flex items-center gap-x-2.5 font-mono text-xs tabular-nums'>
            {stats}
          </span>
        </div>
      </CardContent>
    </Card>
  )
}

// ============================================================================
// Transcript grouping — assistant text + tool calls + following tool results
// form one semantic turn (Turn Grouping).
// ============================================================================

interface TranscriptGroup {
  item: ObserverTranscriptMessage
  attachedResults: ObserverTranscriptMessage[]
}

function groupTranscriptMessages(
  messages: ObserverTranscriptMessage[]
): TranscriptGroup[] {
  const groups: TranscriptGroup[] = []
  for (const item of messages) {
    const isResultItem = item.role === 'tool' || item.kind === 'tool_result'
    if (isResultItem) {
      const prev = groups.at(-1)
      if (prev && prev.item.role === 'assistant') {
        prev.attachedResults.push(item)
        continue
      }
    }
    groups.push({ item, attachedResults: [] })
  }
  return groups
}

// ============================================================================
// Timeline nodes — role-coded execution flow
// ============================================================================

function TimelineNode({
  item,
  attachedResults = [],
}: {
  item: ObserverTranscriptMessage
  attachedResults?: ObserverTranscriptMessage[]
}) {
  const { t } = useTranslation()

  // Gap marker — truncated tail of an over-limit capture.
  if (item.kind === 'gap' || item.gap) {
    return (
      <div className='flex w-full justify-center py-1'>
        <div className='border-border/60 bg-muted/30 inline-flex items-center gap-1.5 rounded-md border border-dashed px-3 py-1.5 text-xs text-muted-foreground'>
          <AlertTriangle className='size-3.5 shrink-0' aria-hidden='true' />
          <span>{t('Truncated')}</span>
          {item.gap && (
            <span className='font-mono tabular-nums'>
              · {item.gap.omitted_items} {t('items')}
            </span>
          )}
        </div>
      </div>
    )
  }

  // System prompt — a centered context strip, not a speaker turn.
  if (item.kind === 'system') {
    const text = (item.content ?? [])
      .filter((part) => part.type === 'text' && part.text)
      .map((part) => part.text)
      .join('\n\n')
    return (
      <div className='border-border/60 bg-muted/30 rounded-md border px-3 py-2'>
        <div className='text-muted-foreground mb-1 flex items-center gap-1.5 text-xs font-medium'>
          <Terminal className='size-3.5 shrink-0' aria-hidden='true' />
          {t('System')}
        </div>
        {text ? (
          <Response className='text-xs'>{text}</Response>
        ) : (
          <div className='text-muted-foreground text-xs'>
            {t('No text content')}
          </div>
        )}
      </div>
    )
  }

  // Unknown item — explicit placeholder, never silently dropped.
  if (item.kind === 'unknown') {
    return (
      <div className='flex w-full justify-center py-1'>
        <div className='border-border/60 bg-muted/30 inline-flex items-center gap-1.5 rounded-md border px-3 py-1.5 text-xs text-muted-foreground'>
          <AlertTriangle className='size-3.5 shrink-0' aria-hidden='true' />
          <span>{t('Unrecognized content')}</span>
        </div>
      </div>
    )
  }

  const isUser = item.role === 'user'
  const isAssistant = item.role === 'assistant'
  const isToolMessage = item.role === 'tool'
  const isToolItem = item.kind === 'tool_call' || item.kind === 'tool_result'

  // Standalone tool result (page boundary, no preceding assistant) — inline.
  if ((isToolItem && !isAssistant) || isToolMessage) {
    const parts = item.content ?? []
    const resultPart = parts.find(
      (part) => part.type === 'tool_result' && part.result
    )
    const textParts = parts.filter((part) => part.type === 'text' && part.text)
    const result = resultPart?.result ?? {
      output: textParts.map((part) => part.text).join('\n'),
    }
    return (
      <div className='pl-7'>
        <ToolResultNode result={result} />
      </div>
    )
  }

  // Chat bubbles — playground vocabulary, no avatars: user is a compact
  // card bubble on the right, assistant is a plain document column on the
  // left (its tool calls are collapsible inline rows, not cards).
  const from = isUser ? ('user' as const) : ('assistant' as const)
  // Both roles align left and span the full width — the timeline reads as a
  // continuous flow with no ragged right edge.
  const alignment: MessageAlignment = 'left'
  return (
    <Message className='group py-1' from={from}>
      <div className='w-full min-w-0 flex-1 basis-full'>
        <div
          className={cn(
            'flex w-full min-w-0 flex-col',
            getMessageAlignmentClass(alignment)
          )}
        >
          <MessageContent
            variant='flat'
            className={cn(
              getMessageContentStyles(),
              isUser && [
                'group-[.is-user]:w-full',
                'group-[.is-user]:max-w-none',
                'sm:group-[.is-user]:max-w-none',
                'md:group-[.is-user]:max-w-none',
                'lg:group-[.is-user]:max-w-none',
              ]
            )}
          >
            <AssistantTurnBody item={item} attachedResults={attachedResults} />
          </MessageContent>
        </div>
      </div>
    </Message>
  )
}

// ----------------------------------------------------------------------------
// AssistantTurnBody — renders text + tool groups in conversation order so the
// narration stays interleaved and consecutive tool calls collapse into one
// collapsible tree node instead of a wall of rows.
// ----------------------------------------------------------------------------

function AssistantTurnBody({
  item,
  attachedResults,
}: {
  item: ObserverTranscriptMessage
  attachedResults: ObserverTranscriptMessage[]
}) {
  const { t } = useTranslation()
  const parts = item.content ?? []
  const callParts = parts.filter(
    (part): part is typeof part & { call: ObserverToolCallRef } =>
      part.type === 'tool_call' && part.call !== undefined
  )
  const resultParts = parts.filter(
    (part): part is typeof part & { result: ObserverToolResultRef } =>
      part.type === 'tool_result' && part.result !== undefined
  )
  const mediaParts = parts.filter((part) => part.type === 'media' && part.media)

  // Collect every result that belongs to this turn: inline tool_result parts
  // (Claude style) plus attached role=tool messages (ChatCompletions style,
  // plain-text output). Results carry a tool_call_id when the upstream
  // supplied one; pairing prefers that id and only falls back to position.
  const attachedOutputs: ObserverToolResultRef[] = []
  for (const resultItem of attachedResults) {
    for (const part of resultItem.content ?? []) {
      if (part.type === 'tool_result' && part.result) {
        attachedOutputs.push(part.result)
      } else if (part.type === 'text' && part.text) {
        attachedOutputs.push({ output: part.text })
      }
    }
  }

  const allResults = [
    ...resultParts.map((part) => part.result),
    ...attachedOutputs,
  ]
  // Join by tool_call_id when the call and result both carry one; otherwise
  // consume the next unused result positionally (the observer flattens a
  // turn as call → result in order). A missing result never shifts the
  // pairing of the remaining calls.
  const consumed = new Set<number>()
  const callsWithResults = callParts.map((part) => {
    const callId = part.call.id
    if (callId) {
      const match = allResults.findIndex(
        (result, index) => result.tool_call_id === callId && !consumed.has(index)
      )
      if (match >= 0) {
        consumed.add(match)
        return { call: part.call, result: allResults[match] }
      }
    }
    const fallback = allResults.findIndex((_, index) => !consumed.has(index))
    if (fallback >= 0) {
      consumed.add(fallback)
      return { call: part.call, result: allResults[fallback] }
    }
    return { call: part.call, result: undefined }
  })
  const extraResults = allResults.filter((_, index) => !consumed.has(index))

  // Walk the parts in order, splitting into alternating text runs and
  // consecutive tool-call runs — narration stays interleaved, and adjacent
  // tool calls merge into one collapsible tree node.
  type Segment =
    | { kind: 'text'; key: string; text: string }
    | { kind: 'tools'; key: string; calls: typeof callsWithResults }
  const segments: Segment[] = []
  let textRun = ''
  let textKey = ''
  let toolRun: typeof callsWithResults = []
  let toolIndex = 0
  const flushText = () => {
    if (textRun) {
      segments.push({ kind: 'text', key: textKey, text: textRun })
      textRun = ''
      textKey = ''
    }
  }
  const flushTools = () => {
    if (toolRun.length > 0) {
      const firstCall = toolRun[0]?.call
      segments.push({
        kind: 'tools',
        key: firstCall?.id ?? firstCall?.name ?? `tools-${segments.length}`,
        calls: toolRun,
      })
      toolRun = []
    }
  }
  for (const part of parts) {
    if (part.type === 'text' && part.text) {
      flushTools()
      if (!textRun) textKey = part.hmac ?? `text-${segments.length}`
      textRun += (textRun ? '\n\n' : '') + part.text
    } else if (part.type === 'tool_call' && part.call) {
      flushText()
      toolRun.push(callsWithResults[toolIndex])
      toolIndex += 1
    }
  }
  flushText()
  flushTools()

  return (
    <div className='flex flex-col gap-1'>
      {segments.map((segment) =>
        segment.kind === 'text' ? (
          <Response
            key={`text-${segment.key}`}
            className='[&_p]:my-1 [&_ul]:my-1 [&_ol]:my-1 [&_pre]:my-1 [&_h1]:my-1 [&_h2]:my-1 [&_h3]:my-1'
          >
            {segment.text}
          </Response>
        ) : (
          <ToolGroup key={`tools-${segment.key}`} calls={segment.calls} />
        )
      )}

      {/* Results beyond the paired calls (page boundary or an odd count) */}
      {extraResults.map((result, index) => (
        <ToolResultNode
          key={`extra-${result.tool_call_id ?? `${index}-${result.output}`}`}
          result={result}
        />
      ))}

      {mediaParts.length > 0 && (
        <div className='text-muted-foreground flex flex-wrap items-center gap-1 text-xs'>
          {mediaParts.map((part) => (
            <span
              key={part.hmac ?? part.logical_bytes}
              className='border-border/60 bg-background/60 inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 font-mono text-[11px]'
            >
              {part.media?.kind ?? 'media'}
              · {(part.media?.logical_bytes ?? 0).toLocaleString()} bytes
            </span>
          ))}
        </div>
      )}

      {item.truncated && <Badge variant='warning'>{t('Truncated')}</Badge>}
    </div>
  )
}

// ============================================================================
// Tool group — consecutive tool calls collapse into one tree node so a long
// agent turn reads as "Tool Call × N" instead of a wall of rows. Expanding
// reveals the leaf ToolNodes, each independently collapsible for its JSON.
// ============================================================================

function ToolGroup({
  calls,
}: {
  calls: Array<{ call: ObserverToolCallRef; result?: ObserverToolResultRef }>
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)

  // A single call needs no wrapper — the node is already the leaf.
  if (calls.length === 1) {
    return <ToolNode call={calls[0].call} result={calls[0].result} />
  }

  return (
    <div className='my-0.5'>
      <button
        type='button'
        onClick={() => setOpen((v) => !v)}
        className='hover:bg-muted/40 flex w-full items-center gap-1.5 rounded-md px-1 py-1.5 text-left transition-colors'
        aria-expanded={open}
      >
        <ChevronRight
          className={cn(
            'text-muted-foreground size-3.5 shrink-0 transition-transform',
            open && 'rotate-90'
          )}
          aria-hidden='true'
        />
        <Wrench className='text-primary size-3.5 shrink-0' aria-hidden='true' />
        <span className='min-w-0 flex-1 truncate font-mono text-xs font-medium'>
          {t('Tool Call')} × {calls.length}
        </span>
        <Check
          className='text-success size-3.5 shrink-0'
          aria-hidden='true'
        />
      </button>
      {open && (
        <div className='border-border/40 ml-3.5 space-y-0.5 border-l pl-2'>
          {calls.map(({ call, result }, index) => (
            <ToolNode
              key={call.id ?? `${call.name}-${index}`}
              call={call}
              result={result}
            />
          ))}
        </div>
      )}
    </div>
  )
}

// ============================================================================
// Tool node — a collapsible agent step. Hides raw JSON by default; presents
// a compact "tool name + status" header. Expanding reveals semantic input/
// output fields plus a "View raw JSON" CodeBlock for full inspection.
// ============================================================================

function ToolNode({
  call,
  result,
}: {
  call: ObserverToolCallRef
  result?: ObserverToolResultRef
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)

  const signature = useMemo(() => formatCallSignature(call), [call])
  const argsText = useMemo(
    () => stringifyJsonValue(call.arguments),
    [call.arguments]
  )
  const outputText = useMemo(
    () => (result ? stringifyJsonValue(result.output) : ''),
    [result]
  )

  return (
    <div className='my-0.5'>
      <button
        type='button'
        onClick={() => setOpen((v) => !v)}
        className='hover:bg-muted/40 flex w-full items-center gap-1.5 rounded-md px-1 py-1.5 text-left transition-colors'
        aria-expanded={open}
      >
        <ChevronRight
          className={cn(
            'text-muted-foreground size-3.5 shrink-0 transition-transform',
            open && 'rotate-90'
          )}
          aria-hidden='true'
        />
        <Wrench className='text-primary size-3.5 shrink-0' aria-hidden='true' />
        <span className='min-w-0 flex-1 truncate font-mono text-xs font-medium'>
          {signature}
        </span>
        {result && (
          <Check
            className='text-success size-3.5 shrink-0'
            aria-hidden='true'
          />
        )}
      </button>

      {open && (
        <div className='px-1 pb-1'>
          <CodeBlockFrame
            showToolbar
            className='my-0'
            title={
              <span className='truncate font-mono text-[11px]'>
                {call.name || t('Tool Call')}
              </span>
            }
          >
            <div className='divide-y divide-dashed divide-border/60'>
              {argsText && (
                <section className='px-2.5 py-1.5'>
                  <div className='text-muted-foreground mb-0.5 text-[10px] font-medium uppercase tracking-wide'>
                    {t('Input')}
                  </div>
                  <pre className='overflow-auto font-mono text-xs leading-5 break-all whitespace-pre-wrap'>
                    {argsText}
                  </pre>
                </section>
              )}
              {result && outputText && (
                <section className='px-2.5 py-1.5'>
                  <div className='text-muted-foreground mb-0.5 text-[10px] font-medium uppercase tracking-wide'>
                    {t('Output')}
                  </div>
                  <pre className='overflow-auto font-mono text-xs leading-5 break-all whitespace-pre-wrap'>
                    {outputText}
                  </pre>
                </section>
              )}
            </div>
          </CodeBlockFrame>
        </div>
      )}
    </div>
  )
}

function ToolResultNode({ result }: { result: ObserverToolResultRef }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const outputText = useMemo(
    () => stringifyJsonValue(result.output),
    [result.output]
  )

  return (
    <div className='my-0.5'>
      <button
        type='button'
        onClick={() => setOpen((v) => !v)}
        className='hover:bg-muted/40 flex w-full items-center gap-1.5 rounded-md px-1 py-1.5 text-left transition-colors'
        aria-expanded={open}
      >
        <ChevronRight
          className={cn(
            'text-muted-foreground size-3.5 shrink-0 transition-transform',
            open && 'rotate-90'
          )}
          aria-hidden='true'
        />
        <Check className='text-success size-3.5 shrink-0' aria-hidden='true' />
        <span className='min-w-0 flex-1 truncate text-xs font-medium'>
          {t('Tool Result')}
        </span>
      </button>
      {open && outputText && (
        <div className='px-1 pb-1'>
          <CodeBlockFrame
            showToolbar
            className='my-0'
            title={
              <span className='truncate text-[11px]'>
                {t('Tool Result')}
              </span>
            }
          >
            <div className='px-2.5 py-1.5'>
              <pre className='overflow-auto font-mono text-xs leading-5 break-all whitespace-pre-wrap'>
                {outputText}
              </pre>
            </div>
          </CodeBlockFrame>
        </div>
      )}
    </div>
  )
}

// ============================================================================
// Tool call formatting — a generic function-signature preview so the tool
// node header reads like a call site (get_weather(city="Shanghai")) instead
// of a raw JSON blob. No hardcoded field names — works for any tool.
// ============================================================================

function formatCallSignature(call: ObserverToolCallRef): string {
  const name = call.name || 'tool'
  const args = parseJsonArg(call.arguments)
  if (args == null) return name
  if (typeof args === 'string') {
    return args ? `${name}("${truncate(args, 40)}")` : name
  }
  if (Array.isArray(args)) {
    if (args.length === 0) return name
    const parts = args.slice(0, 3).map(formatArgValue)
    const suffix = args.length > 3 ? ', …' : ''
    return `${name}(${parts.join(', ')}${suffix})`
  }
  if (typeof args === 'object') {
    const entries = Object.entries(args as Record<string, unknown>)
    if (entries.length === 0) return name
    const parts = entries.slice(0, 3).map(([k, v]) => `${k}=${formatArgValue(v)}`)
    const suffix = entries.length > 3 ? ', …' : ''
    return `${name}(${parts.join(', ')}${suffix})`
  }
  return name
}

function formatArgValue(value: unknown): string {
  if (value == null) return 'null'
  if (typeof value === 'string') return `"${truncate(value, 30)}"`
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  try {
    return truncate(JSON.stringify(value), 30)
  } catch {
    return '…'
  }
}

function truncate(text: string, max: number): string {
  return text.length > max ? `${text.slice(0, max)}…` : text
}

/** Parse a tool argument value that may arrive as a JSON string (Claude
 * tool_use passes arguments as a stringified JSON) or as a plain object. */
function parseJsonArg(value: unknown): unknown {
  if (value == null) return null
  if (typeof value === 'string') {
    const trimmed = value.trim()
    if (
      (trimmed.startsWith('{') && trimmed.endsWith('}')) ||
      (trimmed.startsWith('[') && trimmed.endsWith(']'))
    ) {
      try {
        return JSON.parse(trimmed)
      } catch {
        return value
      }
    }
    return value
  }
  return value
}

function stringifyJsonValue(value: unknown): string {
  if (value == null) return ''
  if (typeof value === 'string') {
    const parsed = parseJsonArg(value)
    if (typeof parsed === 'object' && parsed !== null) {
      try {
        return JSON.stringify(parsed, null, 2)
      } catch {
        return value
      }
    }
    return value
  }
  try {
    return JSON.stringify(value, null, 2)
  } catch {
    return String(value)
  }
}

// ============================================================================
// Shared degraded envelope presentation (HTTP 200 + data.degraded === true)
// ============================================================================

function DegradedAlert({ data }: { data: unknown }) {
  const { t } = useTranslation()
  if (!isObserverDegraded(data)) return null
  return (
    <Alert>
      <AlertTitle>{t('Degraded')}</AlertTitle>
      <AlertDescription>
        {data.reason === 'timeout'
          ? t('The store timed out')
          : t('The store is temporarily unavailable')}
      </AlertDescription>
    </Alert>
  )
}
