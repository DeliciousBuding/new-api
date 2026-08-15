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

import { observabilityQueryKeys } from '../query-keys'

describe('observabilityQueryKeys', () => {
  test('status and overview keys are stable', () => {
    expect(observabilityQueryKeys.status()).toEqual(['observability', 'status'])
    expect(observabilityQueryKeys.overview()).toEqual([
      'observability',
      'overview',
      undefined,
    ])
    expect(
      observabilityQueryKeys.overview({ window_seconds: 300, windows: 12 })
    ).toEqual([
      'observability',
      'overview',
      { window_seconds: 300, windows: 12 },
    ])
  })

  test('session list keys nest under sessions/list with filters', () => {
    expect(observabilityQueryKeys.sessions.lists()).toEqual([
      'observability',
      'sessions',
      'list',
    ])
    expect(observabilityQueryKeys.sessions.list({ page_size: 50 })).toEqual([
      'observability',
      'sessions',
      'list',
      { page_size: 50 },
    ])
  })

  test('session detail key carries the session id', () => {
    expect(observabilityQueryKeys.sessions.detail('session-abc')).toEqual([
      'observability',
      'sessions',
      'detail',
      'session-abc',
    ])
  })

  test('turn list keys are scoped per session', () => {
    expect(observabilityQueryKeys.turns.list('session-abc', {})).toEqual([
      'observability',
      'turns',
      'list',
      'session-abc',
      {},
    ])
    expect(observabilityQueryKeys.turns.list('session-abc', {})).not.toEqual(
      observabilityQueryKeys.turns.list('session-xyz', {})
    )
  })

  test('context key carries both turn and session ids', () => {
    expect(observabilityQueryKeys.context('turn-1', 'session-abc')).toEqual([
      'observability',
      'context',
      'turn-1',
      'session-abc',
    ])
  })

  test('transcript list key is scoped per session', () => {
    expect(observabilityQueryKeys.transcript.list('session-abc')).toEqual([
      'observability',
      'transcript',
      'list',
      'session-abc',
    ])
    expect(observabilityQueryKeys.transcript.list('session-abc')).not.toEqual(
      observabilityQueryKeys.transcript.list('session-xyz')
    )
    expect(
      observabilityQueryKeys.transcript
        .lists()
        .slice(0, observabilityQueryKeys.transcript.lists().length)
    ).toEqual(
      observabilityQueryKeys.transcript
        .list('session-abc')
        .slice(0, observabilityQueryKeys.transcript.lists().length)
    )
  })

  test('each page cursor is its own cache entry (keyset isolation)', () => {
    const first = observabilityQueryKeys.sessions.list({ cursor: undefined })
    const second = observabilityQueryKeys.sessions.list({ cursor: 'c1' })
    const third = observabilityQueryKeys.sessions.list({ cursor: 'c2' })
    expect(first).not.toEqual(second)
    expect(second).not.toEqual(third)
  })

  test('invalidating lists() also matches every page key (prefix semantics)', () => {
    const prefix = observabilityQueryKeys.sessions.lists()
    const page = observabilityQueryKeys.sessions.list({ cursor: 'c1' })
    expect(page.slice(0, prefix.length)).toEqual(prefix)
  })
})
