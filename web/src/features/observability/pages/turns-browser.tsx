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
 * Turns browser — the global turn list across every session, including the
 * per-turn transient sessions of stateless traffic that the session list
 * deliberately hides. Master-detail layout mirroring the sessions page:
 * filter form + keyset-paginated turn table on the left, the selected turn's
 * canonical content reconstruction on the right.
 *
 * Data flow:
 *  - list: GET /api/relay-observer/turns (filters: user_id, model, success)
 *  - detail: GET /api/relay-observer/turns/:id/context?session_id=...
 *    (session_id comes from the turn row; a session-less metadata-only turn
 *    has no content to reconstruct).
 */
import { useQuery } from '@tanstack/react-query'
import {
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
  type RowSelectionState,
} from '@tanstack/react-table'
import {
  useMemo,
  useState,
  type ComponentProps,
  type ReactNode,
} from 'react'
import { useTranslation } from 'react-i18next'

import { DataTableRow, DataTableView } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from '@/components/ui/empty'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { ClientProfileBadge } from '@/features/usage-logs/components/client-profile-badge'
import type { ClientProfile } from '@/features/usage-logs/types'
import { formatTimestampToDate } from '@/lib/format'
import { cn } from '@/lib/utils'

import { getTurnContext, listAllTurns, type TurnQueryParams } from '../api'
import { CursorPagination } from '../components/cursor-pagination'
import { useCursorPagination } from '../components/use-cursor-pagination'
import { observabilityQueryKeys } from '../query-keys'
import {
  isObserverDegraded,
  type ObserverCanonicalItem,
  type ObserverCanonicalPart,
  type ObserverTurn,
} from '../types'

const PAGE_SIZE = 25

// ============================================================================
// Filter form
// ============================================================================

type SuccessFilter = 'all' | 'true' | 'false'

interface TurnFilterDraft {
  user_id: string
  model: string
  success: SuccessFilter
}

const EMPTY_DRAFT: TurnFilterDraft = {
  user_id: '',
  model: '',
  success: 'all',
}

function buildFilterParams(draft: TurnFilterDraft): TurnQueryParams {
  const params: TurnQueryParams = {}
  const userId = Number(draft.user_id)
  if (draft.user_id !== '' && Number.isFinite(userId)) {
    params.user_id = userId
  }
  if (draft.model) params.model = draft.model
  if (draft.success === 'true') params.success = true
  if (draft.success === 'false') params.success = false
  return params
}

function FilterField(props: { children: ReactNode }) {
  return (
    <div className='min-w-0 [&_[data-slot=select-trigger]]:w-full [&_[data-slot=select-trigger]]:text-sm [&_[data-slot=select-value]]:leading-5'>
      {props.children}
    </div>
  )
}

function FilterInput(props: ComponentProps<typeof Input>) {
  return (
    <Input
      {...props}
      className={cn('h-8 min-w-0 text-sm leading-5', props.className)}
    />
  )
}

// ============================================================================
// Turn table
// ============================================================================

interface TurnTableProps {
  turns: ObserverTurn[]
  selectedTurnId: string | null
  isLoading: boolean
  onSelectTurn: (turn: ObserverTurn) => void
}

function TurnTable(props: TurnTableProps) {
  const { t } = useTranslation()
  const columns = useMemo<ColumnDef<ObserverTurn>[]>(
    () => [
      {
        accessorKey: 'occurred_at',
        header: t('Time'),
        cell: ({ row }) => (
          <span className='truncate font-mono text-xs tabular-nums'>
            {formatTimestampToDate(
              Math.floor(new Date(row.original.occurred_at).getTime() / 1000)
            )}
          </span>
        ),
      },
      {
        accessorKey: 'user_id',
        header: t('User'),
        cell: ({ row }) => (
          <span className='font-mono text-xs'>#{row.original.user_id}</span>
        ),
      },
      {
        accessorKey: 'model',
        header: t('Model'),
        cell: ({ row }) => (
          <span className='max-w-[160px] truncate font-mono text-xs'>
            {row.original.model}
          </span>
        ),
      },
      {
        accessorKey: 'client_profile',
        header: t('Client'),
        cell: ({ row }) => (
          <ClientProfileBadge
            profile={row.original.client_profile as ClientProfile}
          />
        ),
      },
      {
        accessorKey: 'success',
        header: t('Result'),
        cell: ({ row }) =>
          row.original.success ? (
            <Badge
              variant='outline'
              className='border-success/40 bg-success/10 text-success'
            >
              {t('OK')}
            </Badge>
          ) : (
            <Badge variant='warning'>{row.original.status_code}</Badge>
          ),
      },
      {
        accessorKey: 'prompt_tokens',
        header: t('Tokens'),
        cell: ({ row }) => (
          <span className='font-mono text-xs tabular-nums'>
            {row.original.prompt_tokens.toLocaleString()}
          </span>
        ),
      },
    ],
    [t]
  )

  const rowSelection = useMemo<RowSelectionState>(
    () => (props.selectedTurnId ? { [props.selectedTurnId]: true } : {}),
    [props.selectedTurnId]
  )
  const table = useReactTable({
    data: props.turns,
    columns,
    state: { rowSelection },
    getRowId: (row) => row.turn_id,
    getCoreRowModel: getCoreRowModel(),
  })

  return (
    <DataTableView
      table={table}
      isLoading={props.isLoading}
      emptyTitle={t('No Turns Found')}
      emptyDescription={t(
        'No turns have been recorded yet. Turns appear here once the observer captures traffic.'
      )}
      skeletonKeyPrefix='turns-table-skeleton'
      renderRow={(row) => (
        <DataTableRow
          key={row.id}
          row={row}
          onClick={() => props.onSelectTurn(row.original)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' || event.key === ' ') {
              event.preventDefault()
              props.onSelectTurn(row.original)
            }
          }}
          role='button'
          tabIndex={0}
          aria-selected={row.getIsSelected()}
          className='cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset'
        />
      )}
    />
  )
}

// ============================================================================
// Turn detail — canonical content reconstruction
// ============================================================================

function partText(part: ObserverCanonicalPart): ReactNode {
  switch (part.type) {
    case 'text':
      return <p className='whitespace-pre-wrap break-words'>{part.text}</p>
    case 'tool_call':
      return (
        <pre className='overflow-x-auto whitespace-pre-wrap break-words font-mono text-xs'>
          {JSON.stringify(part.call, null, 2)}
        </pre>
      )
    case 'tool_result':
      return (
        <pre className='overflow-x-auto whitespace-pre-wrap break-words font-mono text-xs'>
          {JSON.stringify(part.result, null, 2)}
        </pre>
      )
    case 'media':
      return (
        <p className='text-muted-foreground font-mono text-xs'>
          [{part.media?.kind ?? 'media'}] {part.media?.logical_bytes ?? 0} bytes
        </p>
      )
    default:
      return (
        <p className='text-muted-foreground font-mono text-xs'>
          [{part.type}] {part.logical_bytes ?? 0} bytes
        </p>
      )
  }
}

function CanonicalItemView({ item }: { item: ObserverCanonicalItem }) {
  const { t } = useTranslation()
  if (item.kind === 'gap') {
    return (
      <Badge variant='warning' className='font-mono'>
        {t('gap')} {item.logical_bytes} bytes
      </Badge>
    )
  }
  const role = item.role ? (
    <span className='text-muted-foreground mr-2 w-16 shrink-0 text-right font-mono text-xs uppercase'>
      {item.role}
    </span>
  ) : null
  return (
    <div className='flex gap-2'>
      {role}
      <div className='min-w-0 flex-1 space-y-1 text-sm'>
        {(item.content ?? []).map((part, i) => (
          <div key={`${item.hmac}-${i}`}>{partText(part)}</div>
        ))}
        {item.truncated && (
          <p className='text-muted-foreground text-xs'>
            {t('truncated')}
          </p>
        )}
      </div>
    </div>
  )
}

function TurnContextPane({
  turnId,
  sessionId,
}: {
  turnId: string
  sessionId: string | null
}) {
  const { t } = useTranslation()
  if (!sessionId) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyTitle>{t('No Content')}</EmptyTitle>
          <EmptyDescription>
            {t(
              'This turn has no session binding, so no content was persisted.'
            )}
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    )
  }
  const contextQuery = useQuery({
    queryKey: observabilityQueryKeys.context(turnId, sessionId),
    queryFn: () => getTurnContext(turnId, sessionId),
    retry: false,
  })
  if (contextQuery.isLoading) {
    return (
      <div className='space-y-3'>
        {Array.from({ length: 4 }, (_, i) => (
          <Skeleton
            key={`context-skeleton-${i}`}
            className={i % 2 === 0 ? 'h-10 w-3/4' : 'h-8 w-full'}
          />
        ))}
      </div>
    )
  }
  const ctx = contextQuery.data?.data
  if (contextQuery.isError || !ctx || isObserverDegraded(ctx)) {
    return (
      <ErrorState
        title={t('Failed to load turn content')}
        description={t('The observer could not reconstruct this turn.')}
      />
    )
  }
  if (ctx.items.length === 0) {
    return (
      <Empty>
        <EmptyTitle>{t('No Content')}</EmptyTitle>
      </Empty>
    )
  }
  return (
    <div className='flex flex-col gap-2'>
      {ctx.items.map((item) => (
        <div
          key={item.hmac}
          className='border-border/60 rounded-lg border p-2.5'
        >
          <CanonicalItemView item={item} />
        </div>
      ))}
    </div>
  )
}

// ============================================================================

export function TurnsBrowserPage() {
  const { t } = useTranslation()
  const pagination = useCursorPagination()
  const [draft, setDraft] = useState<TurnFilterDraft>(EMPTY_DRAFT)
  const [filters, setFilters] = useState<TurnQueryParams>({})
  const [selected, setSelected] = useState<ObserverTurn | null>(null)

  const query = useQuery({
    queryKey: observabilityQueryKeys.turns.all({
      ...filters,
      cursor: pagination.cursor,
    }),
    queryFn: () =>
      listAllTurns({
        ...filters,
        cursor: pagination.cursor,
        page_size: PAGE_SIZE,
      }),
    retry: false,
  })

  const data = query.data?.data
  const turns = data && !isObserverDegraded(data) ? data.items : []
  const hasMore = data && !isObserverDegraded(data) ? data.meta.has_more : false

  // The cursor stack is advanced only when the user clicks Next (pattern:
  // components/cursor-pagination.tsx) — pushing on every data arrival would
  // change the query key and auto-walk the list to the end.
  const handleNext = () => {
    if (data && !isObserverDegraded(data)) {
      pagination.push(data.meta.next_cursor)
    }
  }

  const applyFilters = () => {
    pagination.reset()
    setSelected(null)
    setFilters(buildFilterParams(draft))
  }

  return (
    <div className='grid h-full min-h-0 gap-4 xl:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]'>
      <div className='min-h-0 overflow-auto xl:max-h-full'>
        <div className='mb-3 flex flex-wrap items-end gap-2'>
          <FilterField>
            <FilterInput
              type='number'
              placeholder={t('User ID')}
              value={draft.user_id}
              onChange={(e) =>
                setDraft({ ...draft, user_id: e.target.value })
              }
            />
          </FilterField>
          <FilterField>
            <FilterInput
              placeholder={t('Model')}
              value={draft.model}
              onChange={(e) => setDraft({ ...draft, model: e.target.value })}
            />
          </FilterField>
          <FilterField>
            <Select
              value={draft.success}
              onValueChange={(v) =>
                setDraft({ ...draft, success: v as SuccessFilter })
              }
            >
              <SelectTrigger className='h-8 w-32'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectGroup>
                  <SelectItem value='all'>{t('All results')}</SelectItem>
                  <SelectItem value='true'>{t('Success')}</SelectItem>
                  <SelectItem value='false'>{t('Failed')}</SelectItem>
                </SelectGroup>
              </SelectContent>
            </Select>
          </FilterField>
          <Button type='button' size='sm' onClick={applyFilters}>
            {t('Search')}
          </Button>
        </div>
        {query.isError ? (
          <ErrorState
            title={t('Failed to load turns')}
            description={t('The observer list request failed.')}
          />
        ) : (
          <TurnTable
            turns={turns}
            selectedTurnId={selected?.turn_id ?? null}
            isLoading={query.isLoading}
            onSelectTurn={setSelected}
          />
        )}
        <div className='mt-3'>
          <CursorPagination
            pageIndex={pagination.pageIndex}
            canGoBack={pagination.canGoBack}
            hasMore={hasMore}
            loading={query.isLoading}
            onBack={pagination.back}
            onNext={handleNext}
          />
        </div>
      </div>
      <div className='min-h-0 overflow-auto xl:max-h-full'>
        {selected ? (
          <div className='space-y-3'>
            <div className='flex items-center gap-2'>
              <span className='font-mono text-xs'>{selected.event_id}</span>
              <Badge variant='outline'>{selected.relay_format}</Badge>
              <Badge variant='outline'>{selected.content_state}</Badge>
            </div>
            <TurnContextPane
              turnId={selected.turn_id}
              sessionId={selected.session_id}
            />
          </div>
        ) : (
          <Empty>
            <EmptyHeader>
              <EmptyTitle>{t('Turn Detail')}</EmptyTitle>
              <EmptyDescription>
                {t(
                  'Select a turn from the list to view its reconstructed content.'
                )}
              </EmptyDescription>
            </EmptyHeader>
          </Empty>
        )}
      </div>
    </div>
  )
}
