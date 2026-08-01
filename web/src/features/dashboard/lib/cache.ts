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
import type { TFunction } from 'i18next'

import type { CacheDailyStat } from '@/features/dashboard/api'
import dayjs from '@/lib/dayjs'

// Cache rate is a percentage; pin the axis so short windows are readable.
export const CACHE_RATE_AXIS_MAX = 100

// UTC day bucket (created_at / 86400) rendered in the viewer's local timezone.
export function formatDayLabel(day: number): string {
  return dayjs.unix(day * 86400).format('MM-DD')
}

export interface CacheSummary {
  promptTokens: number
  cacheReadTokens: number
  cacheCreationTokens: number
  rate: number
}

// Aggregate the window totals. The rate uses prompt as the denominator:
// OpenAI-compatible responses already include cached reads in prompt_tokens,
// so read/(read+prompt) would double-count and show ~50% for a ~100% hit rate.
export function buildCacheSummary(rows: CacheDailyStat[]): CacheSummary {
  let prompt = 0
  let read = 0
  let write = 0
  for (const row of rows) {
    prompt += row.prompt_tokens
    read += row.cache_read_tokens
    write += row.cache_creation_tokens
  }
  let rate: number
  if (prompt > 0) {
    rate = Math.min((read / prompt) * 100, CACHE_RATE_AXIS_MAX)
  } else if (read > 0) {
    rate = CACHE_RATE_AXIS_MAX
  } else {
    rate = 0
  }
  // One decimal place, matching the backend cache_rate rounding.
  return {
    promptTokens: prompt,
    cacheReadTokens: read,
    cacheCreationTokens: write,
    rate: Math.round(rate * 10) / 10,
  }
}

export interface CacheRateLabels {
  cacheRate: string
  cacheRead: string
  cacheWrite: string
  inputTokens: string
}

// VChart tooltip lines render functions via `value(datum)` (the
// `valueFormatter` string option only applies to globally registered
// formatters). See flow.ts tooltipMetricLines for the same pattern.
export function buildCacheRateSpec(
  rows: CacheDailyStat[],
  labels: CacheRateLabels
) {
  const chartData = rows.map((row) => ({
    ...row,
    label: formatDayLabel(row.day),
  }))
  const numberValue = (datum: Record<string, unknown>, key: string) =>
    Number(datum[key]) || 0
  const formattedNumber = (datum: Record<string, unknown>, key: string) =>
    numberValue(datum, key).toLocaleString()

  return {
    type: 'line' as const,
    data: [{ id: 'cacheRateData', values: chartData }],
    xField: 'label',
    yField: 'cache_rate',
    point: { visible: true },
    line: { style: { lineWidth: 2 } },
    yAxis: { max: CACHE_RATE_AXIS_MAX },
    label: { visible: false },
    tooltip: {
      mark: {
        content: [
          {
            key: labels.cacheRate,
            value: (datum: Record<string, unknown>) =>
              `${numberValue(datum, 'cache_rate').toFixed(1)}%`,
          },
          {
            key: labels.cacheRead,
            value: (datum: Record<string, unknown>) =>
              formattedNumber(datum, 'cache_read_tokens'),
          },
          {
            key: labels.cacheWrite,
            value: (datum: Record<string, unknown>) =>
              formattedNumber(datum, 'cache_creation_tokens'),
          },
          {
            key: labels.inputTokens,
            value: (datum: Record<string, unknown>) =>
              formattedNumber(datum, 'prompt_tokens'),
          },
        ],
      },
    },
  }
}

export function buildCacheRateLabels(t: TFunction): CacheRateLabels {
  return {
    cacheRate: t('Cache Rate'),
    cacheRead: t('Cache Read'),
    cacheWrite: t('Cache Write'),
    inputTokens: t('Input Tokens'),
  }
}
