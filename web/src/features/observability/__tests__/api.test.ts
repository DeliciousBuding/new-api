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
import { afterAll, afterEach, describe, expect, test } from 'vitest'

// Import the shared axios instance first (same module instance the feature
// api.ts binds to), then swap api.get for a recording stub. The original is
// restored at suite teardown so sibling test files never see the stub.
import { api } from '../../../lib/http-client'
import {
  getOverview,
  getSession,
  getStatus,
  getTurnContext,
  listSessions,
  listTurns,
} from '../api'

const calls: string[] = []
const originalGet = api.get
const degradedResponse = {
  success: true,
  message: '',
  data: {
    degraded: true,
    reason: 'unavailable',
    message: 'test fixture',
  },
}
let responseData: unknown = degradedResponse
api.get = (async (url: unknown) => {
  calls.push(String(url))
  return { data: responseData }
}) as typeof api.get

afterAll(() => {
  api.get = originalGet
})

afterEach(() => {
  calls.length = 0
  responseData = degradedResponse
})

describe('observability api endpoints', () => {
  test('getStatus hits the status route without query params', async () => {
    await getStatus()
    expect(calls).toEqual(['/api/relay-observer/status'])
  })

  test('getStatus validates a healthy status payload', async () => {
    responseData = {
      success: true,
      message: '',
      data: {
        Enabled: true,
        ReasonCode: '',
        IPTrust: 'none',
        QueueCount: 0,
        QueueBytes: 0,
        AcceptedTotal: 1,
        WrittenTotal: 1,
        DroppedTotal: 0,
        CircuitOpen: false,
        CircuitCooldown: 0,
        PGLatencyMS: 1,
        ContentGapsTotal: 0,
        RecentVolume: 1,
        LastRetentionPass: '0001-01-01T00:00:00Z',
        RetentionTurnsDeleted: 0,
        RetentionSessionsDeleted: 0,
        RetentionObjectsDeleted: 0,
        RetentionFailures: 0,
      },
    }
    const result = await getStatus()
    expect(result.data && 'Enabled' in result.data).toBeTruthy()
    if (!result.data || !('Enabled' in result.data)) {
      throw new Error('status payload must carry Enabled')
    }
    expect(result.data.Enabled).toBe(true)
  })

  test('getStatus rejects malformed backend contract data', async () => {
    responseData = {
      success: true,
      data: { Enabled: 'yes' },
    }
    await expect(getStatus()).rejects.toThrow()
  })

  test('getOverview serializes window params', async () => {
    await getOverview({ window_seconds: 300, windows: 12 })
    expect(calls).toEqual([
      '/api/relay-observer/overview?window_seconds=300&windows=12',
    ])
  })

  test('getOverview omits the query string when no params are set', async () => {
    await getOverview()
    expect(calls).toEqual(['/api/relay-observer/overview'])
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
    expect(calls).toEqual([
      '/api/relay-observer/sessions?page_size=50&cursor=opaque-cursor&node_scope=scope-a&user_id=7&model=gpt-4o&ip_trust=proxy&from=2026-08-01T00%3A00%3A00Z',
    ])
  })

  test('listSessions drops undefined, null and empty-string values', async () => {
    await listSessions({
      page_size: undefined,
      cursor: null as unknown as string,
      model: '',
    })
    expect(calls).toEqual(['/api/relay-observer/sessions'])
  })

  test('listSessions keeps explicit false (a valid filter, not an empty value)', async () => {
    await listSessions({ success: false })
    expect(calls).toEqual(['/api/relay-observer/sessions?success=false'])
  })

  test('getSession builds the id path', async () => {
    await getSession('session-abc')
    expect(calls).toEqual(['/api/relay-observer/sessions/session-abc'])
  })

  test('listTurns builds the session path and serializes turn filters', async () => {
    await listTurns('session-abc', {
      page_size: 25,
      cursor: 'c2',
      error_type: 'upstream',
    })
    expect(calls).toEqual([
      '/api/relay-observer/sessions/session-abc/turns?page_size=25&cursor=c2&error_type=upstream',
    ])
  })

  test('listTurns validates and returns the per-turn client profile', async () => {
    responseData = {
      success: true,
      message: '',
      data: {
        page_size: 25,
        items: [
          {
            turn_id: 'turn-1',
            event_id: 'request-1',
            session_id: 'session-abc',
            occurred_at: '2026-08-01T00:00:00Z',
            node_scope: 'scope-a',
            user_id: 7,
            model: 'gpt-5',
            upstream_model: 'gpt-5',
            relay_format: 'responses',
            success: true,
            status_code: 200,
            error_type: '',
            error_code: '',
            latency_ms: 120,
            stream: true,
            prompt_tokens: 10,
            completion_tokens: 5,
            cached_tokens: 2,
            quota: 15,
            attempts: [],
            content_state: 'full',
            client_profile: 'claude_vscode',
          },
        ],
        meta: { next_cursor: '', has_more: false },
      },
    }

    const result = await listTurns('session-abc')

    expect(result.data && 'items' in result.data).toBeTruthy()
    if (!result.data || !('items' in result.data)) {
      throw new Error('turn page payload must carry items')
    }
    expect(result.data.items[0]?.client_profile).toBe('claude_vscode')
  })

  test('listTurns without params keeps a bare turns path', async () => {
    await listTurns('session-abc')
    expect(calls).toEqual(['/api/relay-observer/sessions/session-abc/turns'])
  })

  test('getTurnContext always sends session_id', async () => {
    await getTurnContext('turn-1', 'session-abc')
    expect(calls).toEqual([
      '/api/relay-observer/turns/turn-1/context?session_id=session-abc',
    ])
  })
})
