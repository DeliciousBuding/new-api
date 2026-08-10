import { useQuery } from '@tanstack/react-query'
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
 * Session Detail tab: the summary card of one session, its
 * keyset-paginated turns timeline, and the canonical context of the turn
 * selected on the timeline.
 *
 * pattern: web/src/features/usage-logs/components/usage-logs-table.tsx
 * (useQuery + Card/Table layout, toLocaleString token formatting),
 * web/src/features/observability/components/cursor-pagination.tsx (keyset
 * footer — the shared pagination, NOT the offset-based ui/pagination),
 * web/src/components/status-badge.tsx (success/failure coloring).
 *
 * seam: the session shown here is the one selected
 * on the Sessions tab. The selection arrives through the optional
 * `sessionId` prop, which 's Sessions tab click handler feeds from the
 * route search param `?session=<id>` (URL-state, same pattern as
 * useTableUrlState), wired in the workspace index. With no sessionId this
 * tab renders its default empty state and issues no requests at all.
 *
 * The context panel is on-demand: getTurnContext fires only while a turn
 * is selected (query `enabled`), so browsing the timeline costs nothing.
 * All three queries follow the module rule from query-keys.ts: `retry:
 * false` — the degraded envelope is a deliberate HTTP 200 answer.
 */
import { useEffect, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { Response } from '@/components/ai-elements/response'
import { StatusBadge } from '@/components/status-badge'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from '@/components/ui/empty'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  ChevronDown,
  Terminal,
  TerminalSquare,
  Wrench,
} from 'lucide-react'
import dayjs from '@/lib/dayjs'
import { cn } from '@/lib/utils'

import { getSession, getTurnContext, listTurns } from '../api'
import { CursorPagination } from '../components/cursor-pagination'
import { useCursorPagination } from '../components/use-cursor-pagination'
import { observabilityQueryKeys } from '../query-keys'
import {
  isObserverDegraded,
  type ObserverCanonicalItem,
  type ObserverToolCallRef,
  type ObserverToolResultRef,
  type ObserverTurn,
} from '../types'

export interface SessionDetailTabProps {
  /**
   * seam: the session selected on the Sessions tab (see the module doc).
   * `undefined` renders the default empty state and no queries fire.
   */
  sessionId?: string | null
}

export function SessionDetailTab({ sessionId = null }: SessionDetailTabProps) {
  const { t } = useTranslation()
  const [selectedTurnId, setSelectedTurnId] = useState<string | null>(null)

  // A different session invalidates the previously selected turn.
  useEffect(() => {
    setSelectedTurnId(null)
  }, [sessionId])

  if (!sessionId) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyTitle>{t('Session Detail')}</EmptyTitle>
          <EmptyDescription>
            {t('Select a session from the Sessions tab to view its details.')}
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    )
  }

  return (
    <div className='space-y-4'>
      <SessionSummaryCard sessionId={sessionId} />
      <TurnsTimelineCard
        sessionId={sessionId}
        selectedTurnId={selectedTurnId}
        onSelectTurn={setSelectedTurnId}
      />
      {selectedTurnId && (
        <TurnContextPanel sessionId={sessionId} turnId={selectedTurnId} />
      )}
    </div>
  )
}

// ============================================================================
// Session summary
// ============================================================================

/** Stable skeleton keys, one per summary field (no array-index keys). */
const SUMMARY_SKELETON_FIELDS = [
  'session_id',
  'node_scope',
  'user_id',
  'client_family',
  'first_seen',
  'last_seen',
  'turn_count',
  'gap_count',
]

/** Stable skeleton keys for the turns timeline rows. */
const TURN_SKELETON_KEYS = Array.from(
  { length: 5 },
  (_, i) => `turn-skeleton-${i}`
)

/** Stable skeleton keys for the turn context items. */
const CONTEXT_SKELETON_KEYS = Array.from(
  { length: 3 },
  (_, i) => `context-skeleton-${i}`
)

/** RFC3339 timestamp → `YYYY-MM-DD HH:mm:ss` (pattern:
 * web/src/lib/format.ts formatTimestampToDate). */
function formatTimestamp(iso: string): string {
  return dayjs(iso).format('YYYY-MM-DD HH:mm:ss')
}

function SummaryField({
  label,
  value,
  mono = false,
}: {
  label: string
  value: string
  mono?: boolean
}) {
  return (
    <div className='flex min-w-0 flex-col gap-0.5'>
      <dt className='text-muted-foreground text-xs'>{label}</dt>
      <dd className={cn('truncate text-sm font-medium', mono && 'font-mono')}>
        {value}
      </dd>
    </div>
  )
}

function SessionSummaryCard({ sessionId }: { sessionId: string }) {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: observabilityQueryKeys.sessions.detail(sessionId),
    queryFn: () => getSession(sessionId),
    retry: false,
  })
  const data = query.data?.data

  let content: ReactNode
  if (query.isLoading) {
    content = (
      <div className='grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2'>
        {SUMMARY_SKELETON_FIELDS.map((field) => (
          <Skeleton key={field} className='h-9 w-full' />
        ))}
      </div>
    )
  } else if (query.isError) {
    content = (
      <Empty>
        <EmptyTitle>{t('Failed to load session details')}</EmptyTitle>
      </Empty>
    )
  } else if (data && !isObserverDegraded(data)) {
    content = (
      <dl className='grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2'>
        <SummaryField label={t('Session ID')} value={data.session_id} mono />
        <SummaryField label={t('Node Scope')} value={data.node_scope} mono />
        <SummaryField label={t('User ID')} value={String(data.user_id)} />
        <SummaryField label={t('Client Family')} value={data.client_family} />
        <SummaryField
          label={t('First Seen')}
          value={formatTimestamp(data.first_seen)}
        />
        <SummaryField
          label={t('Last Seen')}
          value={formatTimestamp(data.last_seen)}
        />
        <SummaryField
          label={t('Turn Count')}
          value={data.turn_count.toLocaleString()}
        />
        <SummaryField
          label={t('Gap Count')}
          value={data.gap_count.toLocaleString()}
        />
      </dl>
    )
  } else {
    content = <DegradedAlert data={data} />
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('Session Summary')}</CardTitle>
      </CardHeader>
      <CardContent>{content}</CardContent>
    </Card>
  )
}

// ============================================================================
// Turns timeline (keyset-paginated, row click selects a turn)
// ============================================================================

function TurnRow({
  turn,
  selected,
  onSelect,
}: {
  turn: ObserverTurn
  selected: boolean
  onSelect: () => void
}) {
  const { t } = useTranslation()
  const cached = turn.cached_tokens
  const tokenText = `${turn.prompt_tokens.toLocaleString()} / ${turn.completion_tokens.toLocaleString()}`

  return (
    <TableRow
      onClick={onSelect}
      onKeyDown={(event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault()
          onSelect()
        }
      }}
      role='button'
      tabIndex={0}
      aria-selected={selected}
      className={cn(
        'cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset',
        selected && 'bg-accent hover:bg-accent'
      )}
    >
      <TableCell className='font-mono text-xs tabular-nums'>
        {formatTimestamp(turn.occurred_at)}
      </TableCell>
      <TableCell className='max-w-48 truncate text-xs'>{turn.model}</TableCell>
      <TableCell>
        {/* pattern: web/src/components/status-badge.tsx (success/danger) */}
        <StatusBadge variant={turn.success ? 'success' : 'danger'} size='sm'>
          {turn.success ? t('Success') : t('Failed')}
        </StatusBadge>
      </TableCell>
      <TableCell className='font-mono text-xs tabular-nums'>
        {turn.status_code}
      </TableCell>
      <TableCell className='text-xs tabular-nums'>
        {/* pattern: web/src/features/performance-metrics/lib/format.ts
            formatLatency — seconds once >= 1000 ms */}
        {turn.latency_ms >= 1000
          ? `${(turn.latency_ms / 1000).toFixed(2)}s`
          : `${Math.round(turn.latency_ms)}ms`}
      </TableCell>
      <TableCell className='text-xs tabular-nums'>
        {tokenText}
        {cached > 0 && (
          <span className='text-muted-foreground'>
            {' '}
            (cache {cached.toLocaleString()})
          </span>
        )}
      </TableCell>
      <TableCell className='text-xs tabular-nums'>
        {turn.attempts.length > 1 ? (
          <Badge variant='warning'>{turn.attempts.length}</Badge>
        ) : (
          turn.attempts.length
        )}
      </TableCell>
    </TableRow>
  )
}

function TurnsTimelineCard({
  sessionId,
  selectedTurnId,
  onSelectTurn,
}: {
  sessionId: string
  selectedTurnId: string | null
  onSelectTurn: (turnId: string) => void
}) {
  const { t } = useTranslation()
  const pagination = useCursorPagination()
  const query = useQuery({
    queryKey: observabilityQueryKeys.turns.list(sessionId, {
      cursor: pagination.cursor,
    }),
    queryFn: () => listTurns(sessionId, { cursor: pagination.cursor }),
    retry: false,
  })
  const data = query.data?.data

  // Keyset pagination: a different session invalidates the cursor stack
  // (pattern: cursor-pagination.tsx — reset whenever filters change).
  useEffect(() => {
    pagination.reset()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId])

  const turnsData = data && !isObserverDegraded(data) ? data : null
  const turns = turnsData?.items ?? []

  let content: ReactNode
  if (query.isLoading) {
    content = (
      <div className='space-y-2'>
        {TURN_SKELETON_KEYS.map((key) => (
          <Skeleton key={key} className='h-10 w-full' />
        ))}
      </div>
    )
  } else if (query.isError) {
    content = (
      <Empty>
        <EmptyTitle>{t('Failed to load turns')}</EmptyTitle>
      </Empty>
    )
  } else if (turnsData === null) {
    content = <DegradedAlert data={data} />
  } else if (turns.length === 0) {
    content = (
      <Empty>
        <EmptyTitle>{t('No turns recorded for this session yet.')}</EmptyTitle>
      </Empty>
    )
  } else {
    content = (
      <>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('Time')}</TableHead>
              <TableHead>{t('Model')}</TableHead>
              <TableHead>{t('Status')}</TableHead>
              <TableHead>{t('Code')}</TableHead>
              <TableHead>{t('Latency')}</TableHead>
              <TableHead>{t('Tokens')}</TableHead>
              <TableHead>{t('Attempts')}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {turns.map((turn) => (
              <TurnRow
                key={turn.turn_id}
                turn={turn}
                selected={turn.turn_id === selectedTurnId}
                onSelect={() => onSelectTurn(turn.turn_id)}
              />
            ))}
          </TableBody>
        </Table>
        <CursorPagination
          pageIndex={pagination.pageIndex}
          canGoBack={pagination.canGoBack}
          hasMore={turnsData.meta.has_more}
          loading={query.isFetching}
          onBack={pagination.back}
          // User-driven keyset advance: the fetched page's next_cursor is
          // committed only when "Next" is clicked (see cursor-stack.ts).
          onNext={() => pagination.push(turnsData.meta.next_cursor)}
        />
      </>
    )
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('Turns')}</CardTitle>
      </CardHeader>
      <CardContent className='space-y-3'>{content}</CardContent>
    </Card>
  )
}

// ============================================================================
// Turn context (on-demand canonical content reconstruction)
// ============================================================================


function ToolCallCard({ call }: { call: ObserverToolCallRef }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const argsText =
    call.arguments == null
      ? ''
      : typeof call.arguments === 'string'
        ? call.arguments
        : (JSON.stringify(call.arguments, null, 2) ?? '')
  const name = call.name ?? call.id ?? t('Tool call')

  return (
    <div className='min-w-0'>
      <button
        type='button'
        onClick={() => setOpen((v) => !v)}
        className='flex w-full items-center gap-1.5 py-0.5 text-left'
        aria-expanded={open}
      >
        <Wrench
          className='text-muted-foreground size-3.5 shrink-0'
          aria-hidden='true'
        />
        <span className='min-w-0 truncate font-mono text-xs font-medium'>
          {name}
        </span>
        {argsText && (
          <ChevronDown
            className={cn(
              'text-muted-foreground ml-auto size-3.5 shrink-0 transition-transform',
              open && 'rotate-180'
            )}
            aria-hidden='true'
          />
        )}
      </button>
      {open && argsText && (
        <pre className='bg-muted/40 mt-1 max-h-64 overflow-auto p-2 break-all font-mono text-[11px] whitespace-pre-wrap'>
          {argsText}
        </pre>
      )}
    </div>
  )
}

function ToolResultCard({ result }: { result: ObserverToolResultRef }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const outputText =
    result.output == null
      ? ''
      : typeof result.output === 'string'
        ? result.output
        : (JSON.stringify(result.output, null, 2) ?? '')

  return (
    <div className='min-w-0'>
      <button
        type='button'
        onClick={() => setOpen((v) => !v)}
        className='flex w-full items-center gap-1.5 py-0.5 text-left'
        aria-expanded={open}
      >
        <TerminalSquare
          className='text-muted-foreground size-3.5 shrink-0'
          aria-hidden='true'
        />
        <span className='min-w-0 truncate font-mono text-xs font-medium'>
          {t('Tool result')}
        </span>
        {outputText && (
          <ChevronDown
            className={cn(
              'text-muted-foreground ml-auto size-3.5 shrink-0 transition-transform',
              open && 'rotate-180'
            )}
            aria-hidden='true'
          />
        )}
      </button>
      {open && outputText && (
        <pre className='bg-muted/40 mt-1 max-h-64 overflow-auto p-2 break-all font-mono text-[11px] whitespace-pre-wrap'>
          {outputText}
        </pre>
      )}
    </div>
  )
}

/**
 * Group consecutive assistant(tool_call) items with the role=tool result
 * items that follow them, so each pair renders inside one Assistant bubble
 * instead of two separate cards. The observer stores tool_call and
 * tool_result as separate CanonicalItem rows; without this grouping the
 * user sees the call arguments and the result output in split cards.
 */
interface ContextGroup {
  item: ObserverCanonicalItem
  attachedResults: ObserverCanonicalItem[]
}

function groupContextItems(items: ObserverCanonicalItem[]): ContextGroup[] {
  const groups: ContextGroup[] = []
  for (const item of items) {
    if (item.role === 'tool') {
      const prev = groups[groups.length - 1]
      if (
        prev &&
        prev.item.role === 'assistant' &&
        (prev.item.content ?? []).some((p) => p.type === 'tool_call')
      ) {
        prev.attachedResults.push(item)
        continue
      }
    }
    groups.push({ item, attachedResults: [] })
  }
  return groups
}

function ContextBubble({
  group,
}: {
  group: ContextGroup
}) {
  const { t } = useTranslation()
  const { item, attachedResults } = group
  const parts = item.content ?? []
  const textParts = parts.filter(
    (p) => p.type === 'text' && p.text
  )
  const callParts = parts.filter((p) => p.type === 'tool_call' && p.call)
  const resultParts = parts.filter(
    (p) => p.type === 'tool_result' && p.result
  )
  const mediaParts = parts.filter((p) => p.type === 'media' && p.media)
  const hasTool = callParts.length > 0 || resultParts.length > 0

  // system / unknown / gap: neutral rendering
  if (item.kind === 'system' || item.kind === 'gap' || item.kind === 'unknown') {
    if (item.kind === 'gap') {
      return (
        <div className='flex items-center justify-center gap-2 py-1'>
          <div className='border-border-foreground/20 h-px flex-1 border-t border-dashed' />
          <span className='text-muted-foreground text-xs'>
            {t('Gap')} · {item.logical_bytes.toLocaleString()} {t('bytes')}
          </span>
          <div className='border-border-foreground/20 h-px flex-1 border-t border-dashed' />
        </div>
      )
    }
    return (
      <div className='bg-muted/30 rounded-lg p-3'>
        {item.kind === 'system' && (
          <div className='text-muted-foreground mb-1 flex items-center gap-1.5 text-xs font-medium'>
            <Terminal className='size-3.5' aria-hidden='true' />
            {t('System')}
          </div>
        )}
        <div className='max-h-48 overflow-y-auto text-xs break-all whitespace-pre-wrap'>
          {textParts.map((p) => p.text).join('\n')}
        </div>
      </div>
    )
  }

  const isUser = item.role === 'user'
  const isAssistant = item.role === 'assistant'

  return (
    <div className={cn('flex gap-2', isUser && 'flex-row-reverse')}>
      {isUser && (
        <Avatar className='ring-border/60 size-6 shrink-0 self-start rounded-full ring-1 max-sm:hidden'>
          <AvatarFallback className='bg-primary text-primary-foreground text-[11px] font-semibold'>
            U
          </AvatarFallback>
        </Avatar>
      )}
      {isAssistant && (
        <Avatar className='ring-border/60 size-6 shrink-0 self-start rounded-full ring-1 max-sm:hidden'>
          <AvatarFallback className='bg-muted text-muted-foreground text-[11px] font-semibold'>
            A
          </AvatarFallback>
        </Avatar>
      )}
      <div
        className={cn(
          'min-w-0 max-w-[85%] rounded-xl border px-3 py-2',
          isUser
            ? 'bg-primary text-primary-foreground'
            : 'bg-muted text-foreground'
        )}
      >
        {isAssistant && (
          <div className='text-muted-foreground mb-0.5 text-xs font-medium'>
            {t('Assistant')}
          </div>
        )}
        {/* Text content (markdown for assistant, plain for user) */}
        {textParts.length > 0 && (
          <div className='min-w-0'>
            {isAssistant ? (
              <Response className='text-sm'>
                {textParts.map((p) => p.text).join('\n')}
              </Response>
            ) : (
              <div className='text-sm break-words whitespace-pre-wrap'>
                {textParts.map((p) => p.text).join('\n')}
              </div>
            )}
          </div>
        )}
        {/* Inline tool calls (inside the bubble, no separate card border) */}
        {callParts.map((part) =>
          part.call ? (
            <ToolCallCard key={`call-${part.hmac ?? ''}`} call={part.call} />
          ) : null
        )}
        {/* Inline tool results from this item's own parts */}
        {resultParts.map((part) =>
          part.result ? (
            <ToolResultCard
              key={`result-${part.hmac ?? ''}`}
              result={part.result}
            />
          ) : null
        )}
        {/* Attached role=tool items (merged into this assistant bubble) */}
        {attachedResults.map((resultItem, index) => {
          const rParts = (resultItem.content ?? []).filter(
            (p) => (p.type === 'tool_result' || p.type === 'text') && p.result
          )
          // If the result item has tool_result parts, render them
          if (rParts.length > 0) {
            return rParts.map((part) =>
              part.result ? (
                <ToolResultCard
                  key={`attached-${resultItem.hmac}-${index}`}
                  result={part.result}
                />
              ) : null
            )
          }
          // Otherwise render the text as a tool result
          const textContent = (resultItem.content ?? [])
            .filter((p) => p.type === 'text' && p.text)
            .map((p) => p.text)
            .join('\n')
          if (textContent) {
            return (
              <ToolResultCard
                key={`attached-${resultItem.hmac}-${index}`}
                result={{ output: textContent }}
              />
            )
          }
          return null
        })}
        {/* Media badges */}
        {mediaParts.length > 0 && (
          <div className='space-y-1'>
            {mediaParts.map((part) =>
              part.media ? (
                <div
                  key={`media-${part.hmac ?? ''}`}
                  className='text-muted-foreground font-mono text-xs'
                >
                  {t('Media')} · {part.media.kind} ·{' '}
                  {part.media.logical_bytes.toLocaleString()} {t('bytes')}
                </div>
              ) : null
            )}
          </div>
        )}
        {item.truncated && (
          <Badge variant='warning' className='mt-1'>
            {t('Truncated')}
          </Badge>
        )}
      </div>
    </div>
  )
}

function TurnContextPanel({
  sessionId,
  turnId,
}: {
  sessionId: string
  turnId: string
}) {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: observabilityQueryKeys.context(turnId, sessionId),
    queryFn: () => getTurnContext(turnId, sessionId),
    retry: false,
  })
  const data = query.data?.data

  let content: ReactNode
  if (query.isLoading) {
    content = (
      <div className='space-y-2'>
        {CONTEXT_SKELETON_KEYS.map((key) => (
          <Skeleton key={key} className='h-16 w-full' />
        ))}
      </div>
    )
  } else if (query.isError) {
    content = (
      <Empty>
        <EmptyTitle>{t('Failed to load turn context')}</EmptyTitle>
      </Empty>
    )
  } else if (data && !isObserverDegraded(data)) {
    content =
      data.items.length === 0 ? (
        <Empty>
          <EmptyTitle>{t('No content captured for this turn.')}</EmptyTitle>
        </Empty>
      ) : (
        <div className='flex flex-col gap-3'>
          {groupContextItems(data.items).map((group) => (
            <ContextBubble
              key={`${group.item.kind}-${group.item.hmac}`}
              group={group}
            />
          ))}
        </div>
      )
  } else {
    content = <DegradedAlert data={data} />
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('Turn Context')}</CardTitle>
      </CardHeader>
      <CardContent>{content}</CardContent>
    </Card>
  )
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
