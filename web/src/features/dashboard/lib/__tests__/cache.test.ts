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
import { describe, expect, test } from 'vitest'

import {
  buildCacheRateSpec,
  buildCacheSummary,
  formatDayLabel,
  CACHE_RATE_AXIS_MAX,
} from '@/features/dashboard/lib/cache'

describe('buildCacheSummary', () => {
  test('aggregates window totals and rate', () => {
    const summary = buildCacheSummary([
      {
        day: 2,
        prompt_tokens: 1000,
        input_tokens: 10500,
        cache_read_tokens: 9000,
        cache_creation_tokens: 500,
        cache_rate: 100,
      },
      {
        day: 3,
        prompt_tokens: 500,
        input_tokens: 2000,
        cache_read_tokens: 1500,
        cache_creation_tokens: 100,
        cache_rate: 100,
      },
    ])
    expect(summary.inputTokens).toBe(12500)
    expect(summary.cacheReadTokens).toBe(10500)
    expect(summary.cacheCreationTokens).toBe(600)
    expect(summary.rate).toBe(84)
  })

  test('no double counting keeps a real ~100% hit rate at 100%', () => {
    const summary = buildCacheSummary([
      {
        day: 2,
        prompt_tokens: 33409,
        input_tokens: 33409,
        cache_read_tokens: 33408,
        cache_creation_tokens: 0,
        cache_rate: 100,
      },
    ])
    expect(summary.rate).toBe(100)
  })

  test('partial cache hit rates below the cap', () => {
    const summary = buildCacheSummary([
      {
        day: 2,
        prompt_tokens: 900,
        input_tokens: 900,
        cache_read_tokens: 100,
        cache_creation_tokens: 0,
        cache_rate: 11.1,
      },
    ])
    expect(summary.rate).toBe(11.1)
  })

  test('all cached with no fresh prompt counts as 100%', () => {
    const summary = buildCacheSummary([
      {
        day: 2,
        prompt_tokens: 0,
        input_tokens: 500,
        cache_read_tokens: 500,
        cache_creation_tokens: 0,
        cache_rate: 100,
      },
    ])
    expect(summary.rate).toBe(CACHE_RATE_AXIS_MAX)
  })

  test('empty rows give a zero rate', () => {
    const summary = buildCacheSummary([])
    expect(summary.rate).toBe(0)
    expect(summary.inputTokens).toBe(0)
  })
})

describe('buildCacheRateSpec', () => {
  test('sorts day buckets before rendering and uses normalized input tokens', () => {
    const spec = buildCacheRateSpec(
      [
        {
          day: 3,
          prompt_tokens: 10,
          input_tokens: 15,
          cache_read_tokens: 5,
          cache_creation_tokens: 0,
          cache_rate: 33.3,
        },
        {
          day: 2,
          prompt_tokens: 20,
          input_tokens: 25,
          cache_read_tokens: 5,
          cache_creation_tokens: 0,
          cache_rate: 20,
        },
      ],
      {
        cacheRate: 'rate',
        cacheRead: 'read',
        cacheWrite: 'write',
        inputTokens: 'input',
      }
    )

    expect(spec.data[0].values.map((row) => row.day)).toEqual([2, 3])
    const inputLine = spec.tooltip.mark.content[3]
    expect(inputLine.value(spec.data[0].values[0])).toBe('25')
  })
})

describe('formatDayLabel', () => {
  test('renders the UTC day bucket in the local timezone', () => {
    // Day bucket 2 corresponds to 1970-01-03T00:00:00Z.
    const label = formatDayLabel(2)
    expect(label).toMatch(/^\d{2}-\d{2}$/)
  })
})
