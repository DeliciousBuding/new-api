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
 * Sessions tab (T4.2): keyset-paginated session list from GET
 * /api/relay-observer/sessions with a filter form (native input/select ui
 * components) and the T4.1 cursor pagination pattern.
 *
 * State handling mirrors features/usage-logs/usage-logs-table.tsx: loading →
 * skeleton (DataTableView renders TableSkeleton), empty → TableEmpty, error →
 * ErrorState (components/error-state.tsx), degraded envelope → page-level
 * notice. Filter state stays in memory (this route has no search-params
 * schema; use-table-url-state does not apply here).
 *
 * T4.3 seam (PROGRESS): the tab owns a selected-session highlight and
 * exposes it as optional controlled props (`selectedSessionId` /
 * `onSelectSession`). T4.3 lifts the state in index.tsx and feeds the
 * Session Detail tab; until then the tab keeps its own selection.
 *
 * pattern: features/usage-logs/common-logs-filter-bar.tsx (filter form
 * layout + expand/collapse of advanced filters), features/usage-logs/
 * logs-filter-toolbar.tsx (LogsFilterField/LogsFilterInput sizing),
 * components/cursor-pagination.tsx (keyset footer)
 */
import { useQuery } from '@tanstack/react-query'
import {
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
  type RowSelectionState,
} from '@tanstack/react-table'
import { ChevronDown } from 'lucide-react'
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
import { formatDateTimeStr } from '@/lib/format'
import { cn } from '@/lib/utils'

import { listSessions, type SessionQueryParams } from '../api'
import { CursorPagination } from '../components/cursor-pagination'
import { useCursorPagination } from '../components/use-cursor-pagination'
import { observabilityQueryKeys } from '../query-keys'
import { isObserverDegraded, type ObserverSession } from '../types'

const PAGE_SIZE = 25

// ============================================================================
// Filter form state
// ============================================================================

// 'all' is a Select option value (pattern: usage-logs LOG_TYPE_ALL_VALUE),
// so the select value always matches one of its items.
type SuccessFilter = 'all' | 'true' | 'false'
type IpTrustFilter = 'all' | 'direct' | 'proxy' | 'none'

/** Editable form state — stringly typed, converted on apply. */
interface SessionFilterDraft {
  node_scope: string
  client_family: string
  model: string
  country: string
  ip: string
  user_id: string
  asn: string
  success: SuccessFilter
  ip_trust: IpTrustFilter
  from: string // RFC3339
  to: string // RFC3339
}

const EMPTY_DRAFT: SessionFilterDraft = {
  node_scope: '',
  client_family: '',
  model: '',
  country: '',
  ip: '',
  user_id: '',
  asn: '',
  success: 'all',
  ip_trust: 'all',
  from: '',
  to: '',
}

/** Convert the draft to API query params, dropping empty and non-numeric
 * fields (mirrors the api.ts buildQueryString convention). */
function buildFilterParams(draft: SessionFilterDraft): SessionQueryParams {
  const params: SessionQueryParams = {}
  if (draft.node_scope) params.node_scope = draft.node_scope
  if (draft.client_family) params.client_family = draft.client_family
  if (draft.model) params.model = draft.model
  if (draft.country) params.country = draft.country
  if (draft.ip) params.ip = draft.ip
  if (draft.success === 'true') params.success = true
  if (draft.success === 'false') params.success = false
  if (draft.ip_trust !== 'all') params.ip_trust = draft.ip_trust
  if (draft.from) params.from = draft.from
  if (draft.to) params.to = draft.to
  const userId = Number(draft.user_id)
  if (draft.user_id !== '' && Number.isFinite(userId)) {
    params.user_id = userId
  }
  const asn = Number(draft.asn)
  if (draft.asn !== '' && Number.isFinite(asn)) {
    params.asn = asn
  }
  return params
}

// ============================================================================
// Filter form building blocks (pattern: logs-filter-toolbar LogsFilterField/
// LogsFilterInput)
// ============================================================================

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
// Session table
// ============================================================================

interface SessionTableProps {
  sessions: ObserverSession[]
  selectedSessionId: string | null
  isLoading: boolean
  onSelectSession: (sessionId: string) => void
}

function SessionTable(props: SessionTableProps) {
  const { t } = useTranslation()
  const columns = useMemo<ColumnDef<ObserverSession>[]>(
    () => [
      {
        accessorKey: 'session_id',
        header: t('Session ID'),
        cell: ({ row }) => (
          <span className='font-mono text-xs'>{row.original.session_id}</span>
        ),
      },
      { accessorKey: 'node_scope', header: t('Node Scope') },
      { accessorKey: 'user_id', header: t('User ID') },
      { accessorKey: 'client_family', header: t('Client Family') },
      {
        accessorKey: 'first_seen',
        header: t('First Seen'),
        cell: ({ row }) =>
          formatDateTimeStr(new Date(row.original.first_seen)),
      },
      {
        accessorKey: 'last_seen',
        header: t('Last Seen'),
        cell: ({ row }) => formatDateTimeStr(new Date(row.original.last_seen)),
      },
      {
        accessorKey: 'turn_count',
        header: t('Turns'),
        cell: ({ row }) => row.original.turn_count.toLocaleString(),
      },
      {
        accessorKey: 'gap_count',
        header: t('Gaps'),
        cell: ({ row }) => row.original.gap_count.toLocaleString(),
      },
    ],
    [t]
  )

  // The selected row is driven by the external selection state (own state or
  // the T4.3 controlled prop), so TanStack selection is derived, not owned:
  // clicking a row calls onSelectSession and the highlight follows.
  const rowSelection = useMemo<RowSelectionState>(
    () => (props.selectedSessionId ? { [props.selectedSessionId]: true } : {}),
    [props.selectedSessionId]
  )
  const table = useReactTable({
    data: props.sessions,
    columns,
    state: { rowSelection },
    getRowId: (row) => row.session_id,
    getCoreRowModel: getCoreRowModel(),
  })

  return (
    <DataTableView
      table={table}
      isLoading={props.isLoading}
      emptyTitle={t('No Sessions Found')}
      emptyDescription={t(
        'No sessions have been recorded yet. Sessions will appear here once the observer captures traffic.'
      )}
      skeletonKeyPrefix='sessions-table-skeleton'
      renderRow={(row) => (
        <DataTableRow
          key={row.id}
          row={row}
          onClick={() => props.onSelectSession(row.original.session_id)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' || event.key === ' ') {
              event.preventDefault()
              props.onSelectSession(row.original.session_id)
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

export interface SessionsTabProps {
  /**
   * T4.3 seam — the currently selected session id. Optional: when the parent
   * passes it (with onSelectSession) the tab is controlled and the parent
   * owns the value; otherwise the tab keeps its own selection state.
   */
  selectedSessionId?: string | null
  /** T4.3 seam — selection change callback (id or null when deselected). */
  onSelectSession?: (sessionId: string | null) => void
}

export function SessionsTab(props: SessionsTabProps) {
  const { t } = useTranslation()
  const pagination = useCursorPagination()
  const [draft, setDraft] = useState<SessionFilterDraft>(EMPTY_DRAFT)
  const [filters, setFilters] = useState<SessionQueryParams>({})
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const [internalSelectedId, setInternalSelectedId] = useState<string | null>(
    null
  )

  const selectedSessionId = props.selectedSessionId ?? internalSelectedId

  const query = useQuery({
    queryKey: observabilityQueryKeys.sessions.list({
      ...filters,
      cursor: pagination.cursor,
    }),
    queryFn: () =>
      listSessions({
        ...filters,
        cursor: pagination.cursor,
        page_size: PAGE_SIZE,
      }),
    // pattern: observability/query-keys.ts — the degraded envelope is a
    // deliberate HTTP 200 answer; automatic retries add nothing.
    retry: false,
  })

  const data = query.data?.data
  const degraded = data != null ? isObserverDegraded(data) : false

  // Explicit navigation (pattern: components/cursor-pagination.tsx). The
  // cursor stack is advanced only when the user clicks Next — pushing on
  // every data arrival would change the query key and refetch the next page
  // automatically, walking the list to the end.
  const handleNext = () => {
    if (data && !isObserverDegraded(data)) {
      pagination.push(data.meta.next_cursor)
    }
  }

  const hasMore =
    data != null && !isObserverDegraded(data) && data.meta.has_more
  const sessions =
    data != null && !isObserverDegraded(data) ? data.items : []
  const hasActiveFilters = Object.keys(filters).length > 0

  const advancedActiveCount = [
    draft.user_id,
    draft.country,
    draft.asn,
    draft.ip,
    draft.from,
    draft.to,
  ].filter(Boolean).length

  const handleDraftChange = (field: keyof SessionFilterDraft, value: string) => {
    setDraft((current) => ({ ...current, [field]: value }))
  }

  const handleApply = () => {
    setFilters(buildFilterParams(draft))
    pagination.reset()
  }

  const handleReset = () => {
    setDraft(EMPTY_DRAFT)
    setFilters({})
    pagination.reset()
  }

  const handleSelectSession = (sessionId: string) => {
    const next = selectedSessionId === sessionId ? null : sessionId
    if (props.onSelectSession) {
      props.onSelectSession(next)
    } else {
      setInternalSelectedId(next)
    }
  }

  const successOptions = useMemo(
    () => [
      { value: 'all', label: t('All') },
      { value: 'true', label: t('Successful') },
      { value: 'false', label: t('Failed') },
    ],
    [t]
  )
  const ipTrustOptions = useMemo(
    () => [
      { value: 'all', label: t('All') },
      { value: 'direct', label: t('Direct') },
      { value: 'proxy', label: t('Proxy') },
      { value: 'none', label: t('None') },
    ],
    [t]
  )
  const successLabel =
    successOptions.find((option) => option.value === draft.success)?.label ??
    t('All')
  const ipTrustLabel =
    ipTrustOptions.find((option) => option.value === draft.ip_trust)?.label ??
    t('All')

  const advancedFilters = (
    <>
      <FilterField>
        <FilterInput
          placeholder={t('User ID')}
          value={draft.user_id}
          onChange={(e) => handleDraftChange('user_id', e.target.value)}
        />
      </FilterField>
      <FilterField>
        <FilterInput
          placeholder={t('Country')}
          value={draft.country}
          onChange={(e) => handleDraftChange('country', e.target.value)}
        />
      </FilterField>
      <FilterField>
        <FilterInput
          placeholder={t('ASN')}
          value={draft.asn}
          onChange={(e) => handleDraftChange('asn', e.target.value)}
        />
      </FilterField>
      <FilterField>
        <FilterInput
          placeholder={t('IP Address')}
          value={draft.ip}
          onChange={(e) => handleDraftChange('ip', e.target.value)}
        />
      </FilterField>
      <FilterField>
        <FilterInput
          placeholder={t('From (RFC3339)')}
          value={draft.from}
          onChange={(e) => handleDraftChange('from', e.target.value)}
        />
      </FilterField>
      <FilterField>
        <FilterInput
          placeholder={t('To (RFC3339)')}
          value={draft.to}
          onChange={(e) => handleDraftChange('to', e.target.value)}
        />
      </FilterField>
    </>
  )

  return (
    <div className='space-y-4'>
      <div className='bg-card/50 rounded-lg border p-2.5 sm:p-3'>
        <div className='flex flex-wrap items-start gap-2'>
          <div className='grid min-w-0 flex-1 grid-cols-1 gap-2 sm:grid-cols-[repeat(auto-fit,minmax(10rem,1fr))]'>
            <FilterField>
              <FilterInput
                placeholder={t('Node Scope')}
                value={draft.node_scope}
                onChange={(e) =>
                  handleDraftChange('node_scope', e.target.value)
                }
              />
            </FilterField>
            <FilterField>
              <FilterInput
                placeholder={t('Client Family')}
                value={draft.client_family}
                onChange={(e) =>
                  handleDraftChange('client_family', e.target.value)
                }
              />
            </FilterField>
            <FilterField>
              <FilterInput
                placeholder={t('Model')}
                value={draft.model}
                onChange={(e) => handleDraftChange('model', e.target.value)}
              />
            </FilterField>
            <FilterField>
              <Select
                items={successOptions}
                value={draft.success}
                onValueChange={(value) =>
                  handleDraftChange(
                    'success',
                    value === null ? 'all' : String(value)
                  )
                }
              >
                <SelectTrigger>
                  <SelectValue>{successLabel}</SelectValue>
                </SelectTrigger>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    {successOptions.map((option) => (
                      <SelectItem key={option.value} value={option.value}>
                        {option.label}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
            </FilterField>
            <FilterField>
              <Select
                items={ipTrustOptions}
                value={draft.ip_trust}
                onValueChange={(value) =>
                  handleDraftChange(
                    'ip_trust',
                    value === null ? 'all' : String(value)
                  )
                }
              >
                <SelectTrigger>
                  <SelectValue>{ipTrustLabel}</SelectValue>
                </SelectTrigger>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    {ipTrustOptions.map((option) => (
                      <SelectItem key={option.value} value={option.value}>
                        {option.label}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
            </FilterField>
          </div>
          <div className='flex shrink-0 items-center justify-end'>
            <Button
              type='button'
              variant='ghost'
              onClick={() => setAdvancedOpen((open) => !open)}
              aria-expanded={advancedOpen}
              className={cn(
                'text-muted-foreground hover:text-foreground gap-1 px-2',
                advancedActiveCount > 0 &&
                  !advancedOpen &&
                  'text-primary hover:text-primary'
              )}
            >
              {advancedOpen ? t('Collapse') : t('Expand')}
              {advancedActiveCount > 0 && (
                <Badge className='ml-0.5 size-5 justify-center p-0 text-[10px]'>
                  {advancedActiveCount}
                </Badge>
              )}
              <ChevronDown
                className={cn(
                  'size-3.5 transition-transform duration-200',
                  advancedOpen && 'rotate-180'
                )}
              />
            </Button>
          </div>
        </div>

        {advancedOpen && (
          <div className='mt-2 grid grid-cols-1 gap-2 sm:grid-cols-[repeat(auto-fit,minmax(10rem,1fr))]'>
            {advancedFilters}
          </div>
        )}

        <div className='mt-2 flex flex-wrap items-center justify-end gap-1.5 sm:gap-2'>
          <Button
            type='button'
            variant='outline'
            onClick={handleReset}
            disabled={!hasActiveFilters}
          >
            {t('Reset')}
          </Button>
          <Button type='button' onClick={handleApply}>
            {t('Search')}
          </Button>
        </div>
      </div>

      {selectedSessionId != null && (
        <div className='text-muted-foreground text-sm'>
          {t('Selected session: {{sessionId}}', {
            sessionId: selectedSessionId,
          })}
        </div>
      )}

      {renderContent()}
    </div>
  )

  function renderContent(): ReactNode {
    if (query.isError) {
      return (
        <ErrorState
          title={t('Failed to load sessions')}
          onRetry={() => void query.refetch()}
        />
      )
    }
    if (degraded) {
      return (
        <Empty className='min-h-[300px]'>
          <EmptyHeader>
            <EmptyTitle>
              {t('Observer data is temporarily unavailable')}
            </EmptyTitle>
            <EmptyDescription>
              {t('The observer store is degraded. Please try again later.')}
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      )
    }
    return (
      <>
        <SessionTable
          sessions={sessions}
          selectedSessionId={selectedSessionId}
          isLoading={query.isLoading}
          onSelectSession={handleSelectSession}
        />
        <CursorPagination
          pageIndex={pagination.pageIndex}
          canGoBack={pagination.canGoBack}
          hasMore={hasMore}
          loading={query.isLoading}
          onBack={pagination.back}
          onNext={handleNext}
        />
      </>
    )
  }
}
