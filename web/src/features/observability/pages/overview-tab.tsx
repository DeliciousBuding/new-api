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
 * Overview tab (T4.2): live observer snapshot from GET /status plus the
 * aggregate windows / totals from GET /overview.
 *
 * Layout follows the native dashboard/usage-logs practice: a ui/card status
 * snapshot with badges, a bordered table for the window aggregates, and a
 * totals row. State handling mirrors features/usage-logs/usage-logs-table:
 * loading → skeleton, empty → TableEmpty, error → ErrorState
 * (components/error-state.tsx), degraded envelope → page-level empty notice
 * (it is an HTTP 200 answer, not an error).
 *
 * pattern: features/usage-logs/usage-logs-table.tsx (isLoadingData / empty /
 * skeleton), features/dashboard/components/cache/cache-efficiency-chart.tsx
 * (card + skeleton section layout)
 */
import { useQuery } from '@tanstack/react-query'
import {
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
} from '@tanstack/react-table'
import { useMemo, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTableView } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { Badge } from '@/components/ui/badge'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from '@/components/ui/empty'
import { Skeleton } from '@/components/ui/skeleton'
import { formatDateTimeStr } from '@/lib/format'

import { getOverview, getStatus, type OverviewQueryParams } from '../api'
import { observabilityQueryKeys } from '../query-keys'
import {
  isObserverDegraded,
  type ObserverOverview,
  type ObserverOverviewWindow,
  type ObserverStatus,
} from '../types'

/** Default aggregate window: 5-minute buckets over the last hour. */
const DEFAULT_OVERVIEW_PARAMS: OverviewQueryParams = {
  window_seconds: 300,
  windows: 12,
}

function StatusField(props: { label: string; value: ReactNode }) {
  return (
    <div className='flex items-baseline justify-between gap-2'>
      <span className='text-muted-foreground shrink-0 text-xs'>
        {props.label}
      </span>
      <span className='text-foreground/90 text-right text-sm font-medium tabular-nums'>
        {props.value}
      </span>
    </div>
  )
}

function StatusSection(props: { title: string; children: ReactNode }) {
  return (
    <div className='space-y-2'>
      <h4 className='text-muted-foreground text-xs font-semibold tracking-wide uppercase'>
        {props.title}
      </h4>
      <div className='grid gap-x-6 gap-y-2 sm:grid-cols-2 lg:grid-cols-3'>
        {props.children}
      </div>
    </div>
  )
}

function StatusCard(props: { status: ObserverStatus }) {
  const { t } = useTranslation()
  const status = props.status
  const enabled = status.Enabled
  const circuitOpen = status.CircuitOpen

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('Observer Status')}</CardTitle>
        <CardDescription>
          {t('Live snapshot of the relay observer pipeline.')}
        </CardDescription>
      </CardHeader>
      <CardContent className='space-y-4'>
        <StatusSection title={t('Status')}>
          <StatusField
            label={t('Enabled')}
            value={
              <Badge variant={enabled ? 'default' : 'destructive'}>
                {enabled ? t('Enabled') : t('Disabled')}
              </Badge>
            }
          />
          <StatusField label={t('Reason Code')} value={status.ReasonCode} />
          <StatusField label={t('IP Trust')} value={status.IPTrust} />
        </StatusSection>

        <StatusSection title={t('Queue')}>
          <StatusField
            label={t('Queue Count')}
            value={status.QueueCount.toLocaleString()}
          />
          <StatusField
            label={t('Queue Bytes')}
            value={status.QueueBytes.toLocaleString()}
          />
          <StatusField
            label={t('PG Latency')}
            value={`${status.PGLatencyMS} ms`}
          />
        </StatusSection>

        <StatusSection title={t('Counters')}>
          <StatusField
            label={t('Accepted Total')}
            value={status.AcceptedTotal.toLocaleString()}
          />
          <StatusField
            label={t('Written Total')}
            value={status.WrittenTotal.toLocaleString()}
          />
          <StatusField
            label={t('Dropped Total')}
            value={status.DroppedTotal.toLocaleString()}
          />
          <StatusField
            label={t('Content Gaps')}
            value={status.ContentGapsTotal.toLocaleString()}
          />
          <StatusField
            label={t('Recent Volume')}
            value={status.RecentVolume.toLocaleString()}
          />
        </StatusSection>

        <StatusSection title={t('Circuit')}>
          <StatusField
            label={t('Circuit Open')}
            value={
              <Badge variant={circuitOpen ? 'destructive' : 'default'}>
                {circuitOpen ? t('Open') : t('Closed')}
              </Badge>
            }
          />
          <StatusField
            label={t('Circuit Cooldown')}
            // The wire value is nanoseconds: Status serializes with Go's
            // default field names (no JSON tags) and CircuitCooldown is a
            // time.Duration. Render it as seconds.
            value={`${Math.round(status.CircuitCooldown / 1e9)} s`}
          />
        </StatusSection>

        <StatusSection title={t('Retention')}>
          <StatusField
            label={t('Last Retention Pass')}
            value={formatDateTimeStr(new Date(status.LastRetentionPass))}
          />
          <StatusField
            label={t('Retention Turns Deleted')}
            value={status.RetentionTurnsDeleted.toLocaleString()}
          />
          <StatusField
            label={t('Retention Sessions Deleted')}
            value={status.RetentionSessionsDeleted.toLocaleString()}
          />
          <StatusField
            label={t('Retention Objects Deleted')}
            value={status.RetentionObjectsDeleted.toLocaleString()}
          />
          <StatusField
            label={t('Retention Failures')}
            value={status.RetentionFailures.toLocaleString()}
          />
        </StatusSection>
      </CardContent>
    </Card>
  )
}

function WindowTable(props: { windows: ObserverOverviewWindow[] }) {
  const { t } = useTranslation()
  const columns = useMemo<ColumnDef<ObserverOverviewWindow>[]>(
    () => [
      {
        accessorKey: 'start',
        header: t('Window Start'),
        cell: ({ row }) => formatDateTimeStr(new Date(row.original.start)),
      },
      {
        accessorKey: 'turns',
        header: t('Turns'),
        cell: ({ row }) => row.original.turns.toLocaleString(),
      },
      {
        accessorKey: 'success',
        header: t('Success'),
        cell: ({ row }) => row.original.success.toLocaleString(),
      },
    ],
    [t]
  )

  const table = useReactTable({
    data: props.windows,
    columns,
    manualPagination: true,
    getRowId: (row) => row.start,
    getCoreRowModel: getCoreRowModel(),
  })

  return (
    <DataTableView
      table={table}
      emptyTitle={t('No window aggregates available yet.')}
      emptyDescription={t('No data available')}
      skeletonKeyPrefix='overview-window-skeleton'
    />
  )
}

export function OverviewTab() {
  const { t } = useTranslation()

  const statusQuery = useQuery({
    queryKey: observabilityQueryKeys.status(),
    queryFn: getStatus,
    // pattern: observability/query-keys.ts — the degraded envelope is a
    // deliberate HTTP 200 answer; automatic retries add nothing.
    retry: false,
  })
  const overviewQuery = useQuery({
    queryKey: observabilityQueryKeys.overview(DEFAULT_OVERVIEW_PARAMS),
    queryFn: () => getOverview(DEFAULT_OVERVIEW_PARAMS),
    retry: false,
  })

  const statusData = statusQuery.data?.data
  const overviewData = overviewQuery.data?.data
  const statusDegraded = statusData ? isObserverDegraded(statusData) : false
  const overviewDegraded = overviewData
    ? isObserverDegraded(overviewData)
    : false
  const isLoading = statusQuery.isLoading || overviewQuery.isLoading
  const isDegraded = statusDegraded || overviewDegraded

  const handleRetry = () => {
    void statusQuery.refetch()
    void overviewQuery.refetch()
  }

  if (statusQuery.isError || overviewQuery.isError) {
    return (
      <ErrorState
        title={t('Failed to load overview')}
        onRetry={handleRetry}
      />
    )
  }

  // The degraded envelope is a healthy-but-empty answer, so it is rendered
  // as a notice, never as an error. pattern: observability/types.ts
  // (isObserverDegraded), features/usage-logs/usage-logs-table.tsx
  // (DEFAULT_LOGS_DATA fallback keeps the UI alive on missing data).
  if (isDegraded) {
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

  if (isLoading) {
    return (
      <div className='space-y-4'>
        <Card>
          <CardHeader>
            <Skeleton className='h-5 w-40' />
          </CardHeader>
          <CardContent>
            <Skeleton className='h-24 w-full' />
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <Skeleton className='h-5 w-40' />
          </CardHeader>
          <CardContent>
            <Skeleton className='h-24 w-full' />
          </CardContent>
        </Card>
      </div>
    )
  }

  const status = statusData as ObserverStatus
  const overview = overviewData as ObserverOverview

  return (
    <div className='space-y-4'>
      <StatusCard status={status} />

      <Card>
        <CardHeader>
          <CardTitle>{t('Window Aggregation')}</CardTitle>
          <CardDescription>
            {t(
              'Turn volume per {{window_seconds}}s window over the last {{windows}} windows.',
              {
                window_seconds: overview.window_seconds,
                windows: overview.windows.length,
              }
            )}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <WindowTable windows={overview.windows} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t('Totals')}</CardTitle>
        </CardHeader>
        <CardContent>
          <div className='grid gap-4 sm:grid-cols-3'>
            <StatusField
              label={t('Total Sessions')}
              value={overview.session_count.toLocaleString()}
            />
            <StatusField
              label={t('Total Turns')}
              value={overview.turn_count.toLocaleString()}
            />
            <StatusField
              label={t('Total Gaps')}
              value={overview.gap_count.toLocaleString()}
            />
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
