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
import { useQuery } from '@tanstack/react-query'
import { VChart } from '@visactor/react-vchart'
import { CircleAlert, Zap } from 'lucide-react'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
import { IconBadge } from '@/components/ui/icon-badge'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { getCacheDailyStats } from '@/features/dashboard/api'
import { getDefaultDays } from '@/features/dashboard/lib'
import {
  buildCacheRateLabels,
  buildCacheRateSpec,
  buildCacheSummary,
} from '@/features/dashboard/lib/cache'
import type { DashboardFilters } from '@/features/dashboard/types'
import { computeTimeRange } from '@/lib/time'
import { useChartTheme } from '@/lib/use-chart-theme'
import { VCHART_OPTION } from '@/lib/vchart'

interface CacheEfficiencyChartProps {
  filters?: DashboardFilters
}

export function CacheEfficiencyChart(props: CacheEfficiencyChartProps) {
  const { t } = useTranslation()
  const { resolvedTheme, themeReady } = useChartTheme()

  const timeRange = useMemo(
    () =>
      computeTimeRange(
        getDefaultDays(props.filters?.time_granularity),
        props.filters?.start_timestamp,
        props.filters?.end_timestamp
      ),
    [
      props.filters?.end_timestamp,
      props.filters?.start_timestamp,
      props.filters?.time_granularity,
    ]
  )

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['dashboard', 'cache-efficiency', timeRange],
    queryFn: async () => {
      const res = await getCacheDailyStats({
        start_timestamp: timeRange.start_timestamp,
        end_timestamp: timeRange.end_timestamp,
      })
      return res.data?.items ?? []
    },
    staleTime: 60_000,
  })

  const rows = useMemo(() => data ?? [], [data])
  const summary = useMemo(() => buildCacheSummary(rows), [rows])
  const spec = useMemo(
    () => buildCacheRateSpec(rows, buildCacheRateLabels(t)),
    [rows, t]
  )

  const theme = resolvedTheme === 'dark' ? 'dark' : 'light'

  let chartContent: React.ReactNode
  if (isLoading) {
    chartContent = <Skeleton className='h-full w-full' />
  } else if (isError) {
    chartContent = (
      <div className='flex h-full items-center justify-center p-4'>
        <Alert variant='destructive' className='max-w-md'>
          <CircleAlert />
          <AlertTitle>{t('Failed to load')}</AlertTitle>
          <AlertDescription>
            {error instanceof Error
              ? error.message
              : t('Please try again later.')}
          </AlertDescription>
        </Alert>
      </div>
    )
  } else if (rows.length === 0) {
    chartContent = (
      <Empty className='h-full border-0 py-12'>
        <EmptyHeader>
          <EmptyMedia variant='icon'>
            <Zap />
          </EmptyMedia>
          <EmptyTitle>{t('No cache data available')}</EmptyTitle>
          <EmptyDescription>{t('No data available')}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    )
  } else {
    chartContent = (
      <VChart
        key={`cache-rate-${theme}-${rows.length}`}
        spec={{
          ...spec,
          theme,
          background: 'transparent',
        }}
        option={VCHART_OPTION}
      />
    )
  }

  return (
    <div className='overflow-hidden rounded-lg border'>
      <div className='flex flex-wrap items-center gap-x-5 gap-y-2.5 border-b px-4 py-2.5 sm:px-5 sm:py-3'>
        <div className='flex items-center gap-1.5'>
          <IconBadge tone='info' size='xs'>
            <Zap />
          </IconBadge>
          <span className='text-xs font-semibold whitespace-nowrap'>
            {t('Cache Efficiency')}
          </span>
        </div>

        <div className='bg-border hidden h-4 w-px sm:block' />

        {isLoading ? (
          <div className='flex flex-wrap items-center gap-x-5 gap-y-2'>
            {['rate', 'read', 'write'].map((key) => (
              <div key={key} className='flex items-center gap-1.5'>
                <Skeleton className='h-3 w-14' />
                <Skeleton className='h-4 w-16' />
              </div>
            ))}
          </div>
        ) : (
          <div className='flex flex-wrap items-center gap-x-5 gap-y-2'>
            <span className='flex items-center gap-1.5'>
              <span className='text-muted-foreground text-xs'>
                {t('Cache Rate')}
              </span>
              <span className='font-mono text-xs font-semibold tabular-nums'>
                {summary.rate.toFixed(1)}%
              </span>
            </span>
            <span className='flex items-center gap-1.5'>
              <span className='text-muted-foreground text-xs'>
                {t('Cache Read')}
              </span>
              <span className='font-mono text-xs font-semibold tabular-nums'>
                {summary.cacheReadTokens.toLocaleString()}
              </span>
            </span>
            <span className='flex items-center gap-1.5'>
              <span className='text-muted-foreground text-xs'>
                {t('Cache Write')}
              </span>
              <span className='font-mono text-xs font-semibold tabular-nums'>
                {summary.cacheCreationTokens.toLocaleString()}
              </span>
            </span>
            <TooltipProvider>
              <Tooltip>
                <TooltipTrigger
                  render={<span className='text-muted-foreground/60 inline-flex' />}
                >
                  <CircleAlert className='text-muted-foreground/60 size-3.5' />
                </TooltipTrigger>
                <TooltipContent className='max-w-[16rem]'>
                  <span className='text-xs'>
                    {t(
                      'Cache rate measures cached input tokens over the selected period'
                    )}
                  </span>
                </TooltipContent>
              </Tooltip>
            </TooltipProvider>
          </div>
        )}
      </div>

      <div className='h-[300px] p-1.5 sm:h-96 sm:p-2'>
        {themeReady ? chartContent : <Skeleton className='h-full w-full' />}
      </div>
    </div>
  )
}
