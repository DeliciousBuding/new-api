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
 * Overview tab (redesigned): live observer snapshot as a compact health bar +
 * StatCard grid (Accepted / Written / Content Gaps / Dropped) with sparklines,
 * and the window aggregates as a VChart stacked bar instead of a static table.
 *
 * The redesign follows the native dashboard practice:
 *   - StatCard (features/dashboard/components/ui/stat-card.tsx) for the four
 *     counters, each with an IconBadge, a sparkline, and a tone-coded detail
 *     badge. The sparkline reuses window turn counts (the only server-side
 *     time series the overview API exposes).
 *   - VChart spec built by a pure function (lib/chart.ts buildTurnVolumeSpec),
 *     memoized, and rendered with the shared useChartTheme + VCHART_OPTION,
 *     exactly like features/dashboard/components/cache/cache-efficiency-chart.
 *   - Status + Circuit collapse into a single header health bar so the page
 *     opens on signal, not on a five-section label/value list.
 *
 * State handling is unchanged from the prior Overview tab: loading → skeleton,
 * empty/degraded → page-level Empty notice, error → ErrorState(retry). The
 * degraded envelope is a deliberate HTTP 200 answer, so retry:false still
 * holds (pattern: observability/query-keys.ts).
 */
import { useQuery } from '@tanstack/react-query'
import { VChart } from '@visactor/react-vchart'
import {
  Activity,
  CheckCircle2,
  AlertTriangle,
  CircleSlash,
  Zap,
  Database,
} from 'lucide-react'
import { useMemo, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
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
import { IconBadge } from '@/components/ui/icon-badge'
import { Skeleton } from '@/components/ui/skeleton'
import { StatCard } from '@/features/dashboard/components/ui/stat-card'
import { useChartTheme } from '@/lib/use-chart-theme'
import { VCHART_OPTION } from '@/lib/vchart'

import { getOverview, getStatus, type OverviewQueryParams } from '../api'
import { buildTurnVolumeSpec, windowSparkline } from '../lib/chart'
import { observabilityQueryKeys } from '../query-keys'
import {
  isObserverDegraded,
  type ObserverOverview,
  type ObserverStatus,
} from '../types'

/** Default aggregate window: 5-minute buckets over the last hour. */
const DEFAULT_OVERVIEW_PARAMS: OverviewQueryParams = {
  window_seconds: 300,
  windows: 12,
}

// ============================================================================
// Compact health bar (replaces the five stacked Status sections)
// ============================================================================

function HealthBar(props: { status: ObserverStatus }) {
  const { t } = useTranslation()
  const s = props.status
  const healthy = s.Enabled && !s.CircuitOpen

  return (
    <Card>
      <CardContent className='flex flex-wrap items-center gap-x-6 gap-y-3 py-4'>
        <div className='flex items-center gap-2'>
          <IconBadge tone={healthy ? 'success' : 'destructive'} size='sm'>
            {healthy ? <CheckCircle2 /> : <AlertTriangle />}
          </IconBadge>
          <div className='flex flex-col'>
            <span className='text-sm font-semibold'>
              {healthy ? t('Healthy') : t('Degraded')}
            </span>
            <span className='text-muted-foreground text-xs'>
              {s.Enabled
                ? t('Observer enabled')
                : t('Observer disabled — {{reason}}', { reason: s.ReasonCode })}
            </span>
          </div>
        </div>

        <div className='flex items-center gap-2'>
          <IconBadge tone='info' size='sm'>
            <Zap />
          </IconBadge>
          <div className='flex flex-col'>
            <span className='text-muted-foreground text-xs'>{t('Queue')}</span>
            <span className='text-sm font-semibold tabular-nums'>
              {s.QueueCount.toLocaleString()} · {s.PGLatencyMS} ms
            </span>
          </div>
        </div>

        <div className='flex items-center gap-2'>
          <IconBadge tone={s.CircuitOpen ? 'destructive' : 'neutral'} size='sm'>
            <Activity />
          </IconBadge>
          <div className='flex flex-col'>
            <span className='text-muted-foreground text-xs'>
              {t('Circuit')}
            </span>
            <span className='text-sm font-semibold'>
              {s.CircuitOpen ? t('Open') : t('Closed')}
            </span>
          </div>
        </div>

        <div className='flex items-center gap-2'>
          <IconBadge tone='neutral' size='sm'>
            <Database />
          </IconBadge>
          <div className='flex flex-col'>
            <span className='text-muted-foreground text-xs'>
              {t('IP Trust')}
            </span>
            <span className='text-sm font-semibold'>{s.IPTrust}</span>
          </div>
        </div>

        <div className='ml-auto flex items-center gap-2'>
          <IconBadge tone='neutral' size='sm'>
            <CircleSlash />
          </IconBadge>
          <div className='flex flex-col'>
            <span className='text-muted-foreground text-xs'>
              {t('Retention')}
            </span>
            <span className='text-muted-foreground text-xs tabular-nums'>
              {new Date(s.LastRetentionPass).getFullYear() < 1000
                ? t('Never')
                : new Date(s.LastRetentionPass).toLocaleString()}
            </span>
          </div>
        </div>
      </CardContent>
    </Card>
  )
}

// ============================================================================
// StatCard grid (Accepted / Written / Content Gaps / Dropped)
// ============================================================================

function StatCardGrid(props: {
  status: ObserverStatus
  sparkline: number[]
  gapRatio: number
}) {
  const { t } = useTranslation()
  const { status, sparkline, gapRatio } = props

  return (
    <div className='grid gap-4 sm:grid-cols-2 lg:grid-cols-4'>
      <StatCard
        title={t('Accepted')}
        value={status.AcceptedTotal.toLocaleString()}
        description={t('Since process start')}
        icon={Activity}
        tone='accent-1'
        sparkline={sparkline}
        sparklineVariant='line'
        details={[
          {
            label: t('Recent'),
            value: `${status.RecentVolume}/s`,
            tone: 'default',
          },
          {
            label: t('Queue'),
            value: status.QueueCount.toLocaleString(),
            tone: status.QueueCount > 0 ? 'warning' : 'muted',
          },
        ]}
      />
      <StatCard
        title={t('Written')}
        value={status.WrittenTotal.toLocaleString()}
        description={
          status.DroppedTotal === 0
            ? t('No drops — write path healthy')
            : t('{{n}} drops', { n: status.DroppedTotal.toLocaleString() })
        }
        icon={CheckCircle2}
        tone='accent-2'
        sparkline={sparkline}
        sparklineVariant='line'
        details={[
          {
            label: t('PG latency'),
            value: `${status.PGLatencyMS} ms`,
            tone: status.PGLatencyMS > 200 ? 'warning' : 'success',
          },
          {
            label: t('Circuit'),
            value: status.CircuitOpen ? t('Open') : t('Closed'),
            tone: status.CircuitOpen ? 'destructive' : 'success',
          },
        ]}
      />
      <StatCard
        title={t('Content Gaps')}
        value={status.ContentGapsTotal.toLocaleString()}
        description={t('Reconstruction gaps over total turns')}
        icon={AlertTriangle}
        tone='accent-3'
        sparkline={sparkline}
        sparklineVariant='bars'
        details={[
          {
            label: t('Ratio'),
            value: `${gapRatio.toFixed(2)}%`,
            tone: gapRatio > 1 ? 'warning' : 'muted',
          },
          {
            label: t('Objects deleted'),
            value: status.RetentionObjectsDeleted.toLocaleString(),
            tone: 'muted',
          },
        ]}
      />
      <StatCard
        title={t('Dropped')}
        value={status.DroppedTotal.toLocaleString()}
        description={
          status.DroppedTotal === 0
            ? t('Circuit closed — no drops')
            : t('Inspect circuit and queue pressure')
        }
        icon={CircleSlash}
        tone='accent-1'
        details={[
          {
            label: t('Failures'),
            value: status.RetentionFailures.toLocaleString(),
            tone: status.RetentionFailures > 0 ? 'warning' : 'muted',
          },
          {
            label: t('Sessions deleted'),
            value: status.RetentionSessionsDeleted.toLocaleString(),
            tone: 'muted',
          },
        ]}
      />
    </div>
  )
}

// ============================================================================
// Window volume chart (VChart stacked bar — replaces the static table)
// ============================================================================

function WindowVolumeChart(props: { overview: ObserverOverview }) {
  const { t } = useTranslation()
  const { resolvedTheme, themeReady } = useChartTheme()
  const theme = resolvedTheme === 'dark' ? 'dark' : 'light'

  const labels = useMemo(
    () => ({
      turns: t('Turns'),
      success: t('Success'),
      failed: t('Failed'),
      windowStart: t('Window Start'),
    }),
    [t]
  )

  const spec = useMemo(
    () => buildTurnVolumeSpec(props.overview.windows, labels),
    [props.overview.windows, labels]
  )

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('Turn volume')}</CardTitle>
        <CardDescription>
          {t('{{n}} windows · {{seconds}}s each · peak {{peak}} turns', {
            n: props.overview.windows.length,
            seconds: props.overview.window_seconds,
            peak: Math.max(...props.overview.windows.map((w) => w.turns), 0),
          })}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <div className='h-[240px] p-1.5 sm:p-2'>
          {themeReady ? (
            <VChart
              key={`turn-volume-${theme}-${props.overview.windows.length}`}
              spec={{ ...spec, theme, background: 'transparent' }}
              option={VCHART_OPTION}
            />
          ) : (
            <Skeleton className='h-full w-full' />
          )}
        </div>
      </CardContent>
    </Card>
  )
}

// ============================================================================
// Totals row
// ============================================================================

function TotalsCard(props: { overview: ObserverOverview }) {
  const { t } = useTranslation()
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('Totals')}</CardTitle>
      </CardHeader>
      <CardContent className='grid gap-4 sm:grid-cols-3'>
        <StatField
          label={t('Total Sessions')}
          value={props.overview.session_count.toLocaleString()}
        />
        <StatField
          label={t('Total Turns')}
          value={props.overview.turn_count.toLocaleString()}
        />
        <StatField
          label={t('Total Gaps')}
          value={props.overview.gap_count.toLocaleString()}
        />
      </CardContent>
    </Card>
  )
}

function StatField(props: { label: string; value: ReactNode }) {
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

// ============================================================================
// Overview tab
// ============================================================================

export function OverviewTab() {
  const { t } = useTranslation()

  const statusQuery = useQuery({
    queryKey: observabilityQueryKeys.status(),
    queryFn: getStatus,
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
      <ErrorState title={t('Failed to load overview')} onRetry={handleRetry} />
    )
  }

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
        <Skeleton className='h-16 w-full' />
        <div className='grid gap-4 sm:grid-cols-2 lg:grid-cols-4'>
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={`stat-${i}`} className='h-32 w-full' />
          ))}
        </div>
        <Skeleton className='h-64 w-full' />
      </div>
    )
  }

  const status = statusData as ObserverStatus
  const overview = overviewData as ObserverOverview
  const sparkline = windowSparkline(overview.windows)
  const gapRatio =
    overview.turn_count > 0
      ? (status.ContentGapsTotal / overview.turn_count) * 100
      : 0

  return (
    <div className='space-y-4'>
      <HealthBar status={status} />
      <StatCardGrid status={status} sparkline={sparkline} gapRatio={gapRatio} />
      <WindowVolumeChart overview={overview} />
      <TotalsCard overview={overview} />
    </div>
  )
}
