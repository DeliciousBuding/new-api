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
 * SessionDetailTab behavior tests: summary rendering, keyset turns
 * pagination, on-demand context triggering (enabled assertion), context
 * content rendering including media summaries, and the degraded / empty /
 * error states. All data is fake; no real session ids or IPs.
 *
 * pattern: web/src/features/observability/components/__tests__/cursor-pagination.test.tsx
 * (node:test + happy-dom + manual i18n instance) and
 * web/src/features/observability/__tests__/api.test.ts (stubbing the shared
 * http-client `api.get` singleton instead of mocking modules).
 */
import assert from 'node:assert/strict'
import { after, afterEach, beforeEach, describe, test } from 'node:test'

import { Window } from 'happy-dom'

const domWindow = new Window()
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'CustomEvent',
  'MutationObserver',
  'requestAnimationFrame',
  'cancelAnimationFrame',
  'getComputedStyle',
  'matchMedia',
  'customElements',
] as const

for (const key of domGlobals) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

// Shared axios instance first (same module the feature api.ts binds to),
// then swap api.get for a URL-dispatching stub restored at suite teardown.
const { api } = await import('@/lib/http-client')
const { QueryClient, QueryClientProvider } =
  await import('@tanstack/react-query')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { act } = await import('react')
const { createRoot } = await import('react-dom/client')

const { SessionDetailTab } = await import('../session-detail-tab')

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

// ============================================================================
// i18n (english values — t() renders the key text we assert on)
// ============================================================================

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        'Session Detail': 'Session Detail',
        'Select a session from the Sessions tab to view its details.':
          'Select a session from the Sessions tab to view its details.',
        'Session Summary': 'Session Summary',
        'Session ID': 'Session ID',
        'Node Scope': 'Node Scope',
        'User ID': 'User ID',
        'Client Family': 'Client Family',
        'First Seen': 'First Seen',
        'Last Seen': 'Last Seen',
        'Turn Count': 'Turn Count',
        'Gap Count': 'Gap Count',
        'Failed to load session details': 'Failed to load session details',
        Turns: 'Turns',
        'Failed to load turns': 'Failed to load turns',
        'No turns recorded for this session yet.':
          'No turns recorded for this session yet.',
        Time: 'Time',
        Model: 'Model',
        Status: 'Status',
        Code: 'Code',
        Latency: 'Latency',
        Tokens: 'Tokens',
        Attempts: 'Attempts',
        Success: 'Success',
        Failed: 'Failed',
        Previous: 'Previous',
        Next: 'Next',
        'Page {{current}}': 'Page {{current}}',
        'Turn Context': 'Turn Context',
        'Failed to load turn context': 'Failed to load turn context',
        'No content captured for this turn.':
          'No content captured for this turn.',
        Media: 'Media',
        'Tool call': 'Tool call',
        'Tool result': 'Tool result',
        Truncated: 'Truncated',
        Degraded: 'Degraded',
        'The store timed out': 'The store timed out',
        'The store is temporarily unavailable':
          'The store is temporarily unavailable',
        bytes: 'bytes',
      },
    },
  },
})

// ============================================================================
// api.get stub — exact URL wins, then longest registered prefix
// ============================================================================

type Handler = () => unknown
const handlers = new Map<string, Handler>()
const calls: string[] = []
const originalGet = api.get

api.get = (async (url: unknown) => {
  const u = String(url)
  calls.push(u)
  const exact = handlers.get(u)
  const key =
    exact !== undefined ? u : [...handlers.keys()].find((k) => u.startsWith(k))
  if (key === undefined) {
    throw new Error(`no handler registered for ${u}`)
  }
  const handler = handlers.get(key)
  if (handler === undefined) {
    throw new Error(`no handler registered for ${u}`)
  }
  const value = handler()
  // A handler may return a pending promise to hold the request open (loading
  // state tests); resolve it into the axios response envelope.
  return value instanceof Promise
    ? value.then((data) => ({ data }))
    : { data: value }
}) as typeof api.get

after(() => {
  api.get = originalGet
})

afterEach(() => {
  handlers.clear()
  calls.length = 0
})

// ============================================================================
// Fake payloads (all invented; no real session ids or addresses)
// ============================================================================

const SESSION_PAYLOAD = {
  success: true,
  message: '',
  data: {
    session_id: 'session-abc',
    node_scope: 'edge-1',
    user_id: 42,
    client_family: 'codex_cli',
    first_seen: '2026-08-01T01:02:03Z',
    last_seen: '2026-08-01T01:02:45Z',
    turn_count: 3,
    gap_count: 1,
  },
}

function makeTurn(
  id: string,
  overrides: Record<string, unknown> = {}
): Record<string, unknown> {
  return {
    turn_id: id,
    event_id: `event-${id}`,
    session_id: 'session-abc',
    occurred_at: '2026-08-01T01:02:10Z',
    node_scope: 'edge-1',
    user_id: 42,
    model: 'gpt-4o',
    upstream_model: 'gpt-4o',
    relay_format: 'openai',
    success: true,
    status_code: 200,
    error_type: '',
    error_code: '',
    latency_ms: 1234,
    stream: false,
    prompt_tokens: 100,
    completion_tokens: 50,
    cached_tokens: 10,
    quota: 150,
    attempts: [
      {
        channel_id: 1,
        group: 'default',
        status_code: 200,
        error_code: '',
        elapsed_ms: 1234,
      },
    ],
    content_state: 'complete',
    ...overrides,
  }
}

const TURN_PAGE_1 = {
  success: true,
  message: '',
  data: {
    page_size: 50,
    items: [
      makeTurn('turn-1'),
      makeTurn('turn-2', {
        model: 'claude-3-5-sonnet',
        success: false,
        status_code: 529,
        error_type: 'upstream',
        error_code: 'overloaded',
        latency_ms: 88,
        prompt_tokens: 5,
        completion_tokens: 0,
        cached_tokens: 0,
        attempts: [
          {
            channel_id: 1,
            group: 'default',
            status_code: 529,
            error_code: 'overloaded',
            elapsed_ms: 44,
          },
          {
            channel_id: 2,
            group: 'default',
            status_code: 529,
            error_code: 'overloaded',
            elapsed_ms: 44,
          },
        ],
      }),
    ],
    meta: { next_cursor: 'cursor-1', has_more: true },
  },
}

const TURN_PAGE_2 = {
  success: true,
  message: '',
  data: {
    page_size: 50,
    items: [makeTurn('turn-3', { model: 'gemini-2.0-flash' })],
    meta: { next_cursor: '', has_more: false },
  },
}

const CONTEXT_PAYLOAD = {
  success: true,
  message: '',
  data: {
    turn_id: 'turn-1',
    ordinal: 1,
    items: [
      {
        kind: 'message',
        role: 'user',
        content: [
          {
            type: 'text',
            text: 'Hello observer',
            logical_bytes: 16,
            hmac: 'a'.repeat(64),
          },
          {
            type: 'media',
            media: {
              kind: 'image',
              media_type: 'image/png',
              logical_bytes: 2048,
              hmac: 'b'.repeat(64),
            },
          },
        ],
        logical_bytes: 2064,
        hmac: 'c'.repeat(64),
      },
      {
        kind: 'tool_call',
        role: 'assistant',
        content: [
          {
            // Contract value from pkg/relay_observer/normalizer.go:
            // partTypeToolCall = "tool_call" (NOT "call").
            type: 'tool_call',
            call: { id: 'call-1', name: 'search', arguments: '{"q":"x"}' },
          },
        ],
        logical_bytes: 32,
        hmac: 'd'.repeat(64),
      },
      {
        kind: 'tool_result',
        role: 'tool',
        content: [
          {
            // Contract value: partTypeToolResult = "tool_result".
            type: 'tool_result',
            result: {
              tool_call_id: 'call-1',
              output: '{"hits": 3}',
            },
          },
        ],
        logical_bytes: 24,
        hmac: 'g'.repeat(64),
      },
      {
        kind: 'message',
        role: 'assistant',
        truncated: true,
        content: [
          {
            type: 'text',
            text: 'Streamed tail cut off',
            logical_bytes: 20,
            hmac: 'e'.repeat(64),
          },
        ],
        logical_bytes: 20,
        hmac: 'f'.repeat(64),
      },
    ],
  },
}

const EMPTY_CONTEXT_PAYLOAD = {
  success: true,
  message: '',
  data: { turn_id: 'turn-1', ordinal: 1, items: [] },
}

const DEGRADED_PAYLOAD = {
  success: true,
  message: '',
  data: { degraded: true, reason: 'timeout', message: 'store timed out' },
}

// ============================================================================
// Render helpers
// ============================================================================

let queryClient: InstanceType<typeof QueryClient>

function renderTab(sessionId: string | null = null) {
  const host = document.createElement('div')
  document.body.appendChild(host)
  const root = createRoot(host)
  act(() => {
    root.render(
      <QueryClientProvider client={queryClient}>
        <I18nextProvider i18n={i18n}>
          <SessionDetailTab sessionId={sessionId} />
        </I18nextProvider>
      </QueryClientProvider>
    )
  })
  return { host, root }
}

function textOf(host: HTMLElement): string {
  return host.textContent ?? ''
}

/** Flush React Query promise chains and timers until the predicate holds. */
async function waitFor(
  host: HTMLElement,
  predicate: () => boolean
): Promise<void> {
  const deadline = Date.now() + 3000
  for (;;) {
    if (predicate()) return
    if (Date.now() > deadline) {
      throw new Error(
        `waitFor timeout; text so far: ${textOf(host).slice(0, 300)}`
      )
    }
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10))
    })
  }
}

function clickRow(host: HTMLElement, rowIndex: number): void {
  const row = host.querySelectorAll('tbody tr')[rowIndex]
  assert.ok(row, 'expected a turn row to click')
  act(() => {
    row.dispatchEvent(
      new domWindow.Event('click', { bubbles: true }) as unknown as Event
    )
  })
}

function pressRow(
  host: HTMLElement,
  rowIndex: number,
  key: 'Enter' | ' '
): void {
  const row = host.querySelectorAll('tbody tr')[rowIndex]
  assert.ok(row, 'expected a turn row to activate')
  act(() => {
    row.dispatchEvent(
      new domWindow.KeyboardEvent('keydown', {
        bubbles: true,
        cancelable: true,
        key,
      }) as unknown as KeyboardEvent
    )
  })
}

function clickButton(host: HTMLElement, index: number): void {
  const button = host.querySelectorAll('button')[index]
  assert.ok(button, 'expected a button to click')
  act(() => {
    button.dispatchEvent(
      new domWindow.Event('click', { bubbles: true }) as unknown as Event
    )
  })
}

beforeEach(() => {
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  document.body.innerHTML = ''
})

after(() => {
  document.body.innerHTML = ''
})

// ============================================================================
// Tests
// ============================================================================

describe('SessionDetailTab — default empty state', () => {
  test('renders the selection hint and fires no requests at all', async () => {
    handlers.set('/api/relay-observer/sessions', () => SESSION_PAYLOAD)
    const { host } = renderTab(null)
    assert.match(
      textOf(host),
      /Select a session from the Sessions tab to view its details\./
    )
    // Give any (mis-)configured query a chance to fire before asserting.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 50))
    })
    assert.deepEqual(calls, [], 'no requests may fire without a sessionId')
  })
})

describe('SessionDetailTab — session summary', () => {
  test('renders every summary field with formatted timestamps', async () => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc',
      () => SESSION_PAYLOAD
    )
    handlers.set('/api/relay-observer/sessions/session-abc/turns', () => ({
      success: true,
      message: '',
      data: {
        page_size: 50,
        items: [],
        meta: { next_cursor: '', has_more: false },
      },
    }))
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('codex_cli'))

    const text = textOf(host)
    assert.ok(text.includes('session-abc'), 'session id shown')
    assert.ok(text.includes('edge-1'), 'node scope shown')
    assert.ok(text.includes('42'), 'user id shown')
    assert.ok(text.includes('codex_cli'), 'client family shown')
    assert.ok(text.includes('2026-08-01 01:02:03'), 'first_seen formatted')
    assert.ok(text.includes('2026-08-01 01:02:45'), 'last_seen formatted')
    assert.match(text, /Turn Count\s*3/, 'turn count rendered')
    assert.match(text, /Gap Count\s*1/, 'gap count rendered')
    assert.equal(calls.length, 2, 'exactly the summary and turns queries fire')
    assert.ok(
      calls.includes('/api/relay-observer/sessions/session-abc'),
      'summary endpoint hit'
    )
    assert.ok(
      calls.includes('/api/relay-observer/sessions/session-abc/turns'),
      'turns endpoint hit'
    )
  })

  test('shows skeletons while loading', async () => {
    let release!: (value: unknown) => void
    const pending = new Promise((resolve) => {
      release = resolve
    })
    handlers.set('/api/relay-observer/sessions/session-abc', () => pending)
    handlers.set('/api/relay-observer/sessions/session-abc/turns', () => ({
      success: true,
      message: '',
      data: {
        page_size: 50,
        items: [],
        meta: { next_cursor: '', has_more: false },
      },
    }))
    const { host } = renderTab('session-abc')
    assert.ok(
      host.querySelector('[data-slot="skeleton"]'),
      'summary skeletons render while the query is pending'
    )
    await act(async () => {
      release(SESSION_PAYLOAD)
    })
    await waitFor(host, () => textOf(host).includes('codex_cli'))
    assert.ok(
      !host.querySelector('[data-slot="skeleton"]'),
      'skeletons are gone once the summary resolved'
    )
  })

  test('shows the degraded alert instead of fields', async () => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc',
      () => DEGRADED_PAYLOAD
    )
    handlers.set('/api/relay-observer/sessions/session-abc/turns', () => ({
      success: true,
      message: '',
      data: {
        page_size: 50,
        items: [],
        meta: { next_cursor: '', has_more: false },
      },
    }))
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('Degraded'))
    const text = textOf(host)
    assert.ok(text.includes('The store timed out'), 'degraded reason shown')
    assert.ok(!text.includes('edge-1'), 'no summary fields on degraded data')
  })

  test('shows an error state when getSession fails', async () => {
    handlers.set('/api/relay-observer/sessions/session-abc', () => {
      throw new Error('boom')
    })
    handlers.set('/api/relay-observer/sessions/session-abc/turns', () => ({
      success: true,
      message: '',
      data: {
        page_size: 50,
        items: [],
        meta: { next_cursor: '', has_more: false },
      },
    }))
    const { host } = renderTab('session-abc')
    await waitFor(host, () =>
      textOf(host).includes('Failed to load session details')
    )
  })
})

describe('SessionDetailTab — turns timeline', () => {
  beforeEach(() => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc',
      () => SESSION_PAYLOAD
    )
    handlers.set(
      '/api/relay-observer/sessions/session-abc/turns',
      () => TURN_PAGE_1
    )
  })

  test('renders turn rows: time, model, success, code, latency, tokens, attempts', async () => {
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('gpt-4o'))

    const text = textOf(host)
    assert.ok(text.includes('2026-08-01 01:02:10'), 'occurred_at formatted')
    assert.ok(text.includes('claude-3-5-sonnet'), 'model shown')
    assert.ok(
      text.includes('Success') && text.includes('Failed'),
      'status badges'
    )
    assert.ok(
      text.includes('200') && text.includes('529'),
      'status codes shown'
    )
    assert.ok(text.includes('1.23s'), 'latency formatted as seconds >= 1000')
    assert.ok(text.includes('100 / 50'), 'prompt / completion tokens')
    assert.ok(text.includes('cache 10'), 'cached tokens annotated')
    assert.ok(text.includes('2'), 'attempt count shown for the retried turn')
  })

  test('paginates with cursors: next fetches cursor-1, back restores page 1', async () => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc/turns?cursor=cursor-1',
      () => TURN_PAGE_2
    )
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('gpt-4o'))

    clickButton(host, 1) // Next
    await waitFor(host, () => textOf(host).includes('gemini-2.0-flash'))
    assert.ok(
      calls.includes(
        '/api/relay-observer/sessions/session-abc/turns?cursor=cursor-1'
      ),
      'next page fetched with the previous page cursor'
    )
    assert.match(textOf(host), /Page 2/)

    clickButton(host, 0) // Previous
    await waitFor(host, () => textOf(host).includes('gpt-4o'))
    assert.match(textOf(host), /Page 1/, 'back restores the first page')
    assert.ok(!textOf(host).includes('gemini-2.0-flash'))
  })

  test('shows the empty state when the session has no turns', async () => {
    handlers.set('/api/relay-observer/sessions/session-abc/turns', () => ({
      success: true,
      message: '',
      data: {
        page_size: 50,
        items: [],
        meta: { next_cursor: '', has_more: false },
      },
    }))
    const { host } = renderTab('session-abc')
    await waitFor(host, () =>
      textOf(host).includes('No turns recorded for this session yet.')
    )
    assert.ok(!textOf(host).includes('Success'), 'no table rows on empty turns')
  })

  test('shows an error state when listTurns fails', async () => {
    handlers.set('/api/relay-observer/sessions/session-abc/turns', () => {
      throw new Error('boom')
    })
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('Failed to load turns'))
  })

  test('shows the degraded alert for turns data', async () => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc/turns',
      () => DEGRADED_PAYLOAD
    )
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('The store timed out'))
  })
})

describe('SessionDetailTab — on-demand turn context', () => {
  beforeEach(() => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc',
      () => SESSION_PAYLOAD
    )
    handlers.set(
      '/api/relay-observer/sessions/session-abc/turns',
      () => TURN_PAGE_1
    )
  })

  test('fires the context query only after a turn row is selected (enabled)', async () => {
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('gpt-4o'))
    assert.ok(
      !calls.some((u) => u.includes('/context')),
      'no context request while no turn is selected'
    )

    clickRow(host, 0)
    await waitFor(host, () => textOf(host).includes('Turn Context'))
    assert.ok(
      calls.includes(
        '/api/relay-observer/turns/turn-1/context?session_id=session-abc'
      ),
      'context request carries turn id and mandatory session_id'
    )
  })

  test('turn rows are keyboard-selectable with Enter and Space', async () => {
    handlers.set(
      '/api/relay-observer/turns/turn-1/context',
      () => CONTEXT_PAYLOAD
    )
    handlers.set(
      '/api/relay-observer/turns/turn-2/context',
      () => EMPTY_CONTEXT_PAYLOAD
    )
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('gpt-4o'))

    const rows = host.querySelectorAll('tbody tr')
    assert.equal(rows[0]?.getAttribute('role'), 'button')
    assert.equal(rows[0]?.getAttribute('tabindex'), '0')

    pressRow(host, 0, 'Enter')
    await waitFor(host, () => textOf(host).includes('Hello observer'))
    assert.ok(
      calls.includes(
        '/api/relay-observer/turns/turn-1/context?session_id=session-abc'
      )
    )

    pressRow(host, 1, ' ')
    await waitFor(host, () =>
      textOf(host).includes('No content captured for this turn.')
    )
    assert.ok(
      calls.includes(
        '/api/relay-observer/turns/turn-2/context?session_id=session-abc'
      )
    )
  })

  test('re-queries when the selection moves to another turn', async () => {
    handlers.set(
      '/api/relay-observer/turns/turn-1/context',
      () => CONTEXT_PAYLOAD
    )
    handlers.set(
      '/api/relay-observer/turns/turn-2/context',
      () => EMPTY_CONTEXT_PAYLOAD
    )
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('gpt-4o'))

    clickRow(host, 0)
    await waitFor(host, () => textOf(host).includes('Turn Context'))
    assert.ok(
      calls.includes(
        '/api/relay-observer/turns/turn-1/context?session_id=session-abc'
      )
    )

    clickRow(host, 1)
    await waitFor(host, () =>
      textOf(host).includes('No content captured for this turn.')
    )
    assert.ok(
      calls.includes(
        '/api/relay-observer/turns/turn-2/context?session_id=session-abc'
      ),
      'selecting a different turn fires its own context request'
    )
  })

  test('renders canonical items: kind/role/text, media summary, tool call, truncation', async () => {
    handlers.set(
      '/api/relay-observer/turns/turn-1/context',
      () => CONTEXT_PAYLOAD
    )
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('gpt-4o'))
    clickRow(host, 0)
    await waitFor(host, () => textOf(host).includes('Hello observer'))

    const text = textOf(host)
    assert.ok(text.includes('message'), 'item kind badge')
    assert.ok(text.includes('user'), 'item role')
    assert.ok(text.includes('Hello observer'), 'text part rendered')
    assert.ok(text.includes('Media'), 'media part label')
    assert.ok(text.includes('· image ·'), 'media kind in the summary line')
    assert.ok(text.includes('image/png'), 'media_type in the summary line')
    assert.match(text, /2,?048 bytes/, 'media logical_bytes with unit')
    assert.match(text, /bbbbbbbb…bbbb/, 'media hmac shortened for display')
    assert.ok(text.includes('Tool call'), 'tool call part label')
    assert.ok(text.includes('search'), 'tool call name rendered')
    assert.ok(text.includes('Tool result'), 'tool result part label')
    assert.ok(text.includes('{"hits": 3}'), 'tool result output rendered')
    assert.ok(text.includes('Truncated'), 'truncated marker shown')
    assert.match(text, /2,?064 bytes/, 'item footer logical_bytes')
  })

  test('shows the empty state when the context has no items', async () => {
    handlers.set(
      '/api/relay-observer/turns/turn-1/context',
      () => EMPTY_CONTEXT_PAYLOAD
    )
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('gpt-4o'))
    clickRow(host, 0)
    await waitFor(host, () =>
      textOf(host).includes('No content captured for this turn.')
    )
  })

  test('shows an error state when getTurnContext fails', async () => {
    handlers.set('/api/relay-observer/turns/turn-1/context', () => {
      throw new Error('boom')
    })
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('gpt-4o'))
    clickRow(host, 0)
    await waitFor(host, () =>
      textOf(host).includes('Failed to load turn context')
    )
  })

  test('shows the degraded alert for context data', async () => {
    handlers.set(
      '/api/relay-observer/turns/turn-1/context',
      () => DEGRADED_PAYLOAD
    )
    const { host } = renderTab('session-abc')
    await waitFor(host, () => textOf(host).includes('gpt-4o'))
    clickRow(host, 0)
    await waitFor(host, () => textOf(host).includes('The store timed out'))
  })
})
