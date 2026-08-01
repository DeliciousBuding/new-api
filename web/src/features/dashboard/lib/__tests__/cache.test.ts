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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import {
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
        cache_read_tokens: 9000,
        cache_creation_tokens: 500,
        cache_rate: 100,
      },
      {
        day: 3,
        prompt_tokens: 500,
        cache_read_tokens: 1500,
        cache_creation_tokens: 100,
        cache_rate: 100,
      },
    ])
    assert.equal(summary.promptTokens, 1500)
    assert.equal(summary.cacheReadTokens, 10500)
    assert.equal(summary.cacheCreationTokens, 600)
    // OpenAI-compatible semantics: prompt already includes cached reads,
    // so the rate is read/prompt (capped at 100).
    assert.equal(summary.rate, CACHE_RATE_AXIS_MAX)
  })

  test('no double counting keeps a real ~100% hit rate at 100%', () => {
    const summary = buildCacheSummary([
      {
        day: 2,
        prompt_tokens: 33409,
        cache_read_tokens: 33408,
        cache_creation_tokens: 0,
        cache_rate: 100,
      },
    ])
    assert.equal(summary.rate, 100)
  })

  test('partial cache hit rates below the cap', () => {
    const summary = buildCacheSummary([
      {
        day: 2,
        prompt_tokens: 900,
        cache_read_tokens: 100,
        cache_creation_tokens: 0,
        cache_rate: 11.1,
      },
    ])
    assert.equal(summary.rate, 11.1)
  })

  test('all cached with no fresh prompt counts as 100%', () => {
    const summary = buildCacheSummary([
      {
        day: 2,
        prompt_tokens: 0,
        cache_read_tokens: 500,
        cache_creation_tokens: 0,
        cache_rate: 100,
      },
    ])
    assert.equal(summary.rate, CACHE_RATE_AXIS_MAX)
  })

  test('empty rows give a zero rate', () => {
    const summary = buildCacheSummary([])
    assert.equal(summary.rate, 0)
    assert.equal(summary.promptTokens, 0)
  })
})

describe('formatDayLabel', () => {
  test('renders the UTC day bucket in the local timezone', () => {
    // Day bucket 2 corresponds to 1970-01-03T00:00:00Z.
    const label = formatDayLabel(2)
    assert.match(label, /^\d{2}-\d{2}$/)
  })
})
