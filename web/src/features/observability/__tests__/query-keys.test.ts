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

const { observabilityQueryKeys } = await import('../query-keys')

describe('observabilityQueryKeys', () => {
  test('status and overview keys are stable', () => {
    assert.deepEqual(observabilityQueryKeys.status(), [
      'observability',
      'status',
    ])
    assert.deepEqual(observabilityQueryKeys.overview(), [
      'observability',
      'overview',
      undefined,
    ])
    assert.deepEqual(
      observabilityQueryKeys.overview({ window_seconds: 300, windows: 12 }),
      ['observability', 'overview', { window_seconds: 300, windows: 12 }]
    )
  })

  test('session list keys nest under sessions/list with filters', () => {
    assert.deepEqual(observabilityQueryKeys.sessions.lists(), [
      'observability',
      'sessions',
      'list',
    ])
    assert.deepEqual(
      observabilityQueryKeys.sessions.list({ page_size: 50 }),
      ['observability', 'sessions', 'list', { page_size: 50 }]
    )
  })

  test('session detail key carries the session id', () => {
    assert.deepEqual(observabilityQueryKeys.sessions.detail('session-abc'), [
      'observability',
      'sessions',
      'detail',
      'session-abc',
    ])
  })

  test('turn list keys are scoped per session', () => {
    assert.deepEqual(observabilityQueryKeys.turns.list('session-abc', {}), [
      'observability',
      'turns',
      'list',
      'session-abc',
      {},
    ])
    assert.notDeepEqual(
      observabilityQueryKeys.turns.list('session-abc', {}),
      observabilityQueryKeys.turns.list('session-xyz', {})
    )
  })

  test('context key carries both turn and session ids', () => {
    assert.deepEqual(observabilityQueryKeys.context('turn-1', 'session-abc'), [
      'observability',
      'context',
      'turn-1',
      'session-abc',
    ])
  })

  test('each page cursor is its own cache entry (keyset isolation)', () => {
    const first = observabilityQueryKeys.sessions.list({ cursor: undefined })
    const second = observabilityQueryKeys.sessions.list({ cursor: 'c1' })
    const third = observabilityQueryKeys.sessions.list({ cursor: 'c2' })
    assert.notDeepEqual(first, second)
    assert.notDeepEqual(second, third)
  })

  test('invalidating lists() also matches every page key (prefix semantics)', () => {
    const prefix = observabilityQueryKeys.sessions.lists()
    const page = observabilityQueryKeys.sessions.list({ cursor: 'c1' })
    assert.deepEqual(page.slice(0, prefix.length), prefix)
  })
})
