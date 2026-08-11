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
 * SessionDetailTab behavior tests for the Agent Timeline: transcript
 * latest/older pagination, task summary card, consecutive tool calls
 * collapsing into one tree node, tool results attached to the preceding
 * assistant turn, and the degraded / empty / error states. All data is
 * fabricated; no real session ids or addresses.
 *
 * pattern: sessions-tab.test.tsx (node:test + happy-dom + manual i18n
 * instance) and api.test.ts (stubbing the shared http-client `api.get`
 * singleton instead of mocking modules).
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
        'Select a session from the list to view its agent timeline.':
          'Select a session from the list to view its agent timeline.',
        'Agent Run': 'Agent Run',
        Completed: 'Completed',
        Active: 'Active',
        Incomplete: 'Incomplete',
        Truncated: 'Truncated',
        turns: 'turns',
        items: 'items',
        'Load earlier': 'Load earlier',
        Loading: 'Loading',
        'Failed to load earlier messages': 'Failed to load earlier messages',
        'Failed to load agent timeline': 'Failed to load agent timeline',
        'No conversation recorded for this session yet.':
          'No conversation recorded for this session yet.',
        'Tool Call': 'Tool Call',
        'Tool Result': 'Tool Result',
        Degraded: 'Degraded',
        'The store timed out': 'The store timed out',
        'The store is temporarily unavailable':
          'The store is temporarily unavailable',
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
const stubGet = (async (url: unknown) => {
  const u = String(url)
  calls.push(u)
  const exact = handlers.get(u)
  const key =
    exact !== undefined
      ? u
      : [...handlers.keys()]
          .filter((k) => u.startsWith(k))
          .sort((a, b) => b.length - a.length)[0]
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
api.get = stubGet

after(() => {
  // Chain-safe restore: only unwrap when this file's stub is still the one
  // installed (bun runs test files concurrently and every file swaps the
  // shared http-client singleton).
  if (api.get === stubGet) api.get = originalGet
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
    session_id: 'session-abc-1234',
    node_scope: 'edge-1',
    user_id: 42,
    username: 'bob',
    client_family: 'codex_cli',
    first_seen: '2026-08-01T01:02:03Z',
    last_seen: '2026-08-01T01:02:45Z',
    turn_count: 3,
    gap_count: 0,
  },
}

const DEGRADED_PAYLOAD = {
  success: true,
  message: '',
  data: { degraded: true, reason: 'timeout', message: 'store timed out' },
}

const TRANSCRIPT_PAGE = {
  success: true,
  message: '',
  data: {
    page_size: 50,
    items: [
      {
        turn_id: 'turn-1',
        turn_seq: 0,
        seq: 0,
        kind: 'message',
        role: 'user',
        content: [
          {
            type: 'text',
            text: 'List the files in this repo',
            logical_bytes: 30,
            hmac: 'a'.repeat(64),
          },
        ],
        logical_bytes: 30,
        hmac: 'b'.repeat(64),
      },
      {
        turn_id: 'turn-2',
        turn_seq: 1,
        seq: 0,
        kind: 'message',
        role: 'assistant',
        content: [
          {
            type: 'text',
            text: 'Let me inspect the tree.',
            logical_bytes: 22,
            hmac: 'c'.repeat(64),
          },
          {
            type: 'tool_call',
            call: {
              id: 'call-1',
              name: 'run_command',
              arguments: '{"command":"ls -la"}',
            },
            logical_bytes: 40,
            hmac: 'd'.repeat(64),
          },
          {
            type: 'tool_call',
            call: {
              id: 'call-2',
              name: 'grep',
              arguments: '{"pattern":"session"}',
            },
            logical_bytes: 40,
            hmac: 'e'.repeat(64),
          },
        ],
        logical_bytes: 120,
        hmac: 'f'.repeat(64),
      },
      {
        turn_id: 'turn-3',
        turn_seq: 2,
        seq: 0,
        kind: 'tool_result',
        role: 'tool',
        content: [
          {
            type: 'tool_result',
            result: { tool_call_id: 'call-1', output: '{"files": 12}' },
            logical_bytes: 24,
            hmac: 'g'.repeat(64),
          },
        ],
        logical_bytes: 24,
        hmac: 'h'.repeat(64),
      },
      {
        turn_id: 'turn-4',
        turn_seq: 3,
        seq: 0,
        kind: 'message',
        role: 'assistant',
        content: [
          {
            type: 'text',
            text: 'Done.',
            logical_bytes: 5,
            hmac: 'i'.repeat(64),
          },
        ],
        logical_bytes: 5,
        hmac: 'j'.repeat(64),
      },
    ],
    meta: { prev_cursor: 123, has_older: true },
  },
}

const OLDER_PAGE = {
  success: true,
  message: '',
  data: {
    page_size: 50,
    items: [
      {
        turn_id: 'turn-0',
        turn_seq: -1,
        seq: 0,
        kind: 'message',
        role: 'user',
        content: [
          {
            type: 'text',
            text: 'An older prompt',
            logical_bytes: 16,
            hmac: 'k'.repeat(64),
          },
        ],
        logical_bytes: 16,
        hmac: 'l'.repeat(64),
      },
    ],
    meta: { prev_cursor: 0, has_older: false },
  },
}

const EMPTY_TRANSCRIPT_PAGE = {
  success: true,
  message: '',
  data: {
    page_size: 50,
    items: [],
    meta: { prev_cursor: 0, has_older: false },
  },
}

const GAP_PAGE = {
  success: true,
  message: '',
  data: {
    page_size: 50,
    items: [
      {
        turn_id: 'turn-gap',
        turn_seq: 0,
        seq: 0,
        kind: 'gap',
        gap: {
          position: 'tail',
          reason: 'retention',
          omitted_items: 3,
          logical_bytes: 1024,
        },
        logical_bytes: 1024,
        hmac: 'm'.repeat(64),
      },
    ],
    meta: { prev_cursor: 0, has_older: false },
  },
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

function clickButton(host: HTMLElement, label: string): void {
  const button = [...host.querySelectorAll('button')].find((b) =>
    b.textContent?.includes(label)
  )
  assert.ok(button, `button "${label}" not found`)
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
  handlers.set('/api/relay-observer/sessions/session-abc-1234', () => ({
    ...SESSION_PAYLOAD,
  }))
  handlers.set(
    '/api/relay-observer/sessions/session-abc-1234/transcript',
    () => ({ ...TRANSCRIPT_PAGE })
  )
})

after(() => {
  document.body.innerHTML = ''
})

// ============================================================================
// Tests
// ============================================================================

describe('SessionDetailTab — default empty state', () => {
  test('renders the selection hint and fires no requests at all', async () => {
    const { host } = renderTab(null)
    assert.match(
      textOf(host),
      /Select a session from the list to view its agent timeline\./
    )
    // Give any (mis-)configured query a chance to fire before asserting.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 50))
    })
    assert.deepEqual(calls, [], 'no requests may fire without a sessionId')
  })
})

describe('SessionDetailTab — agent timeline transcript', () => {
  test('renders the transcript flow with the task summary card', async () => {
    const { host } = renderTab('session-abc-1234')
    await waitFor(host, () => textOf(host).includes('List the files'))

    const text = textOf(host)
    assert.ok(text.includes('bob'), 'username from the session summary')
    assert.ok(text.includes('session-'), 'short session id in the card')
    assert.ok(text.includes('Let me inspect the tree.'), 'assistant text')
    assert.ok(text.includes('Done.'), 'final assistant message')
    assert.ok(text.includes('Codex CLI'), 'client profile label')
    assert.ok(text.includes('42s'), 'duration formatted from first/last seen')
    assert.ok(
      text.includes('Completed'),
      'status badge without a trailing user turn or gap'
    )
    assert.ok(text.includes('4') && text.includes('turns'), 'turn count')
  })

  test('collapses consecutive tool calls into one tree node', async () => {
    const { host } = renderTab('session-abc-1234')
    await waitFor(host, () => textOf(host).includes('List the files'))

    assert.ok(
      textOf(host).includes('Tool Call × 2'),
      'two consecutive calls render as a single collapsible node'
    )

    clickButton(host, 'Tool Call')
    await waitFor(host, () => textOf(host).includes('run_command('))
    const text = textOf(host)
    assert.ok(text.includes('run_command('), 'leaf signature rendered')
    assert.ok(text.includes('grep('), 'second leaf signature rendered')
  })

  test('attaches tool results to the preceding assistant turn', async () => {
    const { host } = renderTab('session-abc-1234')
    await waitFor(host, () => textOf(host).includes('List the files'))

    clickButton(host, 'Tool Call')
    await waitFor(host, () => textOf(host).includes('run_command('))
    // The role=tool message is a separate transcript item; it must be joined
    // onto the assistant's calls (positionally) and surface inside the leaf
    // node when expanded — not as a standalone bubble.
    assert.ok(
      textOf(host).includes('run_command('),
      'leaf signature rendered'
    )
    clickButton(host, 'run_command(')
    await waitFor(host, () => textOf(host).includes('"files": 12'))
    assert.ok(
      textOf(host).includes('"files": 12'),
      'attached result output rendered inside the tool leaf'
    )
  })

  test('loads earlier pages by prepending with the prev cursor', async () => {
    const { host } = renderTab('session-abc-1234')
    await waitFor(host, () => textOf(host).includes('List the files'))

    handlers.set(
      '/api/relay-observer/sessions/session-abc-1234/transcript?direction=older&cursor=123&page_size=50',
      () => ({ ...OLDER_PAGE })
    )
    clickButton(host, 'Load earlier')
    await waitFor(host, () => textOf(host).includes('An older prompt'))

    assert.ok(
      calls.some((u) =>
        u.includes('direction=older&cursor=123')
      ),
      'older request carries the previous page cursor'
    )
    const listText = textOf(host)
    assert.ok(
      listText.indexOf('An older prompt') < listText.indexOf('List the files'),
      'older page prepends before the newest content'
    )
  })

  test('shows the gap marker for truncated captures', async () => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc-1234/transcript',
      () => ({ ...GAP_PAGE })
    )
    const { host } = renderTab('session-abc-1234')
    await waitFor(host, () => textOf(host).includes('Truncated'))

    assert.ok(
      textOf(host).includes('3') && textOf(host).includes('items'),
      'gap marker reports the omitted item count'
    )
  })

  test('shows the empty state when the transcript has no items', async () => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc-1234/transcript',
      () => ({ ...EMPTY_TRANSCRIPT_PAGE })
    )
    const { host } = renderTab('session-abc-1234')
    await waitFor(host, () =>
      textOf(host).includes('No conversation recorded for this session yet.')
    )
  })

  test('shows the degraded alert when the transcript store is degraded', async () => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc-1234/transcript',
      () => ({ ...DEGRADED_PAYLOAD })
    )
    const { host } = renderTab('session-abc-1234')
    await waitFor(host, () => textOf(host).includes('The store timed out'))

    const text = textOf(host)
    assert.ok(text.includes('Degraded'), 'alert title rendered')
    assert.ok(!text.includes('List the files'), 'no transcript on degraded')
  })

  test('shows an error state when the transcript request fails', async () => {
    handlers.set(
      '/api/relay-observer/sessions/session-abc-1234/transcript',
      () => {
        throw new Error('boom')
      }
    )
    const { host } = renderTab('session-abc-1234')
    await waitFor(host, () =>
      textOf(host).includes('Failed to load agent timeline')
    )
  })
})

describe('SessionDetailTab — session switching', () => {
  test('resets transcript state when the session changes', async () => {
    const { host, root } = renderTab('session-abc-1234')
    await waitFor(host, () => textOf(host).includes('List the files'))

    handlers.set(
      '/api/relay-observer/sessions/session-xyz-5678',
      () => ({ ...SESSION_PAYLOAD, data: { ...SESSION_PAYLOAD.data, session_id: 'session-xyz-5678' } })
    )
    handlers.set(
      '/api/relay-observer/sessions/session-xyz-5678/transcript',
      () => ({
        success: true,
        message: '',
        data: {
          page_size: 50,
          items: [
            {
              turn_id: 'turn-x',
              turn_seq: 0,
              seq: 0,
              kind: 'message',
              role: 'user',
              content: [
                {
                  type: 'text',
                  text: 'Another session prompt',
                  logical_bytes: 21,
                  hmac: 'n'.repeat(64),
                },
              ],
              logical_bytes: 21,
              hmac: 'o'.repeat(64),
            },
          ],
          meta: { prev_cursor: 0, has_older: false },
        },
      })
    )

    act(() => {
      root.render(
        <QueryClientProvider client={queryClient}>
          <I18nextProvider i18n={i18n}>
            <SessionDetailTab sessionId='session-xyz-5678' />
          </I18nextProvider>
        </QueryClientProvider>
      )
    })

    await waitFor(host, () => textOf(host).includes('Another session prompt'))
    const text = textOf(host)
    assert.ok(!text.includes('List the files'), 'previous session cleared')
    assert.ok(!text.includes('Tool Call × 2'), 'tool state not carried over')
  })
})
