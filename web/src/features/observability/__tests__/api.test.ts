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
import { after, afterEach, describe, test } from 'node:test'

// Import the shared axios instance first (same module instance the feature
// api.ts binds to), then swap api.get for a recording stub. The original is
// restored at suite teardown so sibling test files never see the stub.
const { api } = await import('../../../lib/http-client')
const {
  getOverview,
  getSession,
  getStatus,
  getTurnContext,
  listSessions,
  listTurns,
} = await import('../api')

const calls: string[] = []
const originalGet = api.get
api.get = (async (url: unknown) => {
  calls.push(String(url))
  return { data: { success: true, message: '', data: {} } }
}) as typeof api.get

after(() => {
  api.get = originalGet
})

afterEach(() => {
  calls.length = 0
})

describe('observability api endpoints', () => {
  test('getStatus hits the status route without query params', async () => {
    await getStatus()
    assert.deepEqual(calls, ['/api/relay-observer/status'])
  })

  test('getOverview serializes window params', async () => {
    await getOverview({ window_seconds: 300, windows: 12 })
    assert.deepEqual(calls, [
      '/api/relay-observer/overview?window_seconds=300&windows=12',
    ])
  })

  test('getOverview omits the query string when no params are set', async () => {
    await getOverview()
    assert.deepEqual(calls, ['/api/relay-observer/overview'])
  })

  test('listSessions serializes all filters in snake_case', async () => {
    await listSessions({
      page_size: 50,
      cursor: 'opaque-cursor',
      node_scope: 'scope-a',
      user_id: 7,
      model: 'gpt-4o',
      ip_trust: 'proxy',
      from: '2026-08-01T00:00:00Z',
    })
    assert.deepEqual(calls, [
      '/api/relay-observer/sessions?page_size=50&cursor=opaque-cursor&node_scope=scope-a&user_id=7&model=gpt-4o&ip_trust=proxy&from=2026-08-01T00%3A00%3A00Z',
    ])
  })

  test('listSessions drops undefined, null and empty-string values', async () => {
    await listSessions({
      page_size: undefined,
      cursor: null as unknown as string,
      model: '',
      country: 'US',
    })
    assert.deepEqual(calls, ['/api/relay-observer/sessions?country=US'])
  })

  test('listSessions keeps explicit false (a valid filter, not an empty value)', async () => {
    await listSessions({ success: false })
    assert.deepEqual(calls, ['/api/relay-observer/sessions?success=false'])
  })

  test('getSession builds the id path', async () => {
    await getSession('session-abc')
    assert.deepEqual(calls, ['/api/relay-observer/sessions/session-abc'])
  })

  test('listTurns builds the session path and serializes turn filters', async () => {
    await listTurns('session-abc', {
      page_size: 25,
      cursor: 'c2',
      error_type: 'upstream',
    })
    assert.deepEqual(calls, [
      '/api/relay-observer/sessions/session-abc/turns?page_size=25&cursor=c2&error_type=upstream',
    ])
  })

  test('listTurns without params keeps a bare turns path', async () => {
    await listTurns('session-abc')
    assert.deepEqual(calls, ['/api/relay-observer/sessions/session-abc/turns'])
  })

  test('getTurnContext always sends session_id', async () => {
    await getTurnContext('turn-1', 'session-abc')
    assert.deepEqual(calls, [
      '/api/relay-observer/turns/turn-1/context?session_id=session-abc',
    ])
  })
})
