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
 * SessionsTab behavior tests (node:test + happy-dom, pattern:
 * features/observability/components/__tests__/cursor-pagination.test.tsx).
 *
 * The api layer is stubbed at the shared axios instance (pattern:
 * features/observability/__tests__/api.test.ts) with a cursor-routing
 * handler, so filter serialization and pagination round-trip through the
 * real query path. All session ids are fabricated UUIDs.
 */
import assert from 'node:assert/strict'
import { after, afterEach, describe, test } from 'node:test'

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

const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { QueryClient, QueryClientProvider } = await import('@tanstack/react-query')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')

const { api } = await import('../../../../lib/http-client')
const { SessionsTab } = await import('../sessions-tab')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        // Missing keys fall back to the key itself, so assertions use the
        // English source strings (project i18n convention: key = English).
        'Page {{current}}': 'Page {{current}}',
      },
    },
  },
})

const SESSION_A = {
  session_id: '11111111-1111-4111-8111-111111111111',
  node_scope: 'scope-a',
  user_id: 7,
  client_family: 'openai',
  first_seen: '2026-08-01T10:00:00Z',
  last_seen: '2026-08-01T10:05:00Z',
  turn_count: 4,
  gap_count: 0,
}
const SESSION_B = {
  session_id: '22222222-2222-4222-8222-222222222222',
  node_scope: 'scope-b',
  user_id: 9,
  client_family: 'claude',
  first_seen: '2026-08-01T11:00:00Z',
  last_seen: '2026-08-01T11:10:00Z',
  turn_count: 8,
  gap_count: 1,
}
const SESSION_C = {
  session_id: '33333333-3333-4333-8333-333333333333',
  node_scope: 'scope-a',
  user_id: 12,
  client_family: 'anthropic',
  first_seen: '2026-08-01T12:00:00Z',
  last_seen: '2026-08-01T12:02:00Z',
  turn_count: 2,
  gap_count: 0,
}

const PAGE_1 = {
  page_size: 25,
  items: [SESSION_A, SESSION_B],
  meta: { next_cursor: 'cursor-2', has_more: true },
}
const PAGE_2 = {
  page_size: 25,
  items: [SESSION_C],
  meta: { next_cursor: '', has_more: false },
}
const EMPTY_PAGE = {
  page_size: 25,
  items: [],
  meta: { next_cursor: '', has_more: false },
}

/** URL-routing stub: the cursor query parameter selects the page. */
function pageHandler(url: string): unknown {
  if (url.includes('cursor=cursor-2')) {
    return { success: true, message: '', data: PAGE_2 }
  }
  return { success: true, message: '', data: PAGE_1 }
}

const calls: string[] = []
let handler: (url: string) => unknown = () => ({
  success: true,
  message: '',
  data: {},
})
const originalGet = api.get
api.get = (async (url: unknown) => {
  calls.push(String(url))
  return { data: handler(String(url)) }
}) as typeof api.get

after(() => {
  api.get = originalGet
})

afterEach(() => {
  calls.length = 0
  handler = () => ({ success: true, message: '', data: {} })
  document.body.innerHTML = ''
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

function renderTab(props?: { selectedSessionId?: string | null; onSelectSession?: (id: string | null) => void }) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const host = document.createElement('div')
  document.body.appendChild(host)
  const root = createRoot(host)
  act(() => {
    root.render(
      <QueryClientProvider client={queryClient}>
        <I18nextProvider i18n={i18n}>
          <SessionsTab
            selectedSessionId={props?.selectedSessionId}
            onSelectSession={props?.onSelectSession}
          />
        </I18nextProvider>
      </QueryClientProvider>
    )
  })
  return { host, root }
}

function textOf(host: HTMLElement): string {
  return host.textContent ?? ''
}

/** Poll until the UI reaches the expected state, flushing timers and
 * microtasks inside act() (async tests wait for explicit UI state, never a
 * fixed sleep — web/AGENTS.md 3.14). */
async function waitForText(host: HTMLElement, text: string): Promise<boolean> {
  const deadline = Date.now() + 2000
  while (Date.now() < deadline) {
    let reached = false
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10))
      reached = textOf(host).includes(text)
    })
    if (reached) return true
  }
  return textOf(host).includes(text)
}

function setInputValue(input: Element, value: string) {
  const descriptor = Object.getOwnPropertyDescriptor(
    domWindow.HTMLInputElement.prototype,
    'value'
  )
  assert.ok(descriptor?.set, 'HTMLInputElement value setter must exist')
  const setter = descriptor.set
  setter.call(input, value)
  const inputEvent = new domWindow.Event('input', {
    bubbles: true,
  }) as unknown as Event
  input.dispatchEvent(inputEvent)
}

/** happy-dom Event is not assignable to the DOM Event type — cast it
 * (pattern: cursor-pagination.test.tsx `as unknown as Event`). */
function clickEvent(): Event {
  return new domWindow.Event('click', { bubbles: true }) as unknown as Event
}

function keyEvent(key: 'Enter' | ' '): KeyboardEvent {
  return new domWindow.KeyboardEvent('keydown', {
    bubbles: true,
    cancelable: true,
    key,
  }) as unknown as KeyboardEvent
}

function clickButton(host: HTMLElement, label: string) {
  const button = [...host.querySelectorAll('button')].find((b) =>
    b.textContent?.includes(label)
  )
  assert.ok(button, `button "${label}" not found`)
  act(() => {
    button.dispatchEvent(clickEvent())
  })
}

function findRow(host: HTMLElement, text: string): HTMLElement {
  const row = [...host.querySelectorAll('tbody tr')].find((r) =>
    r.textContent?.includes(text)
  )
  assert.ok(row, `table row containing "${text}" not found`)
  return row as HTMLElement
}

describe('SessionsTab', () => {
  test('renders session rows from the first page', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    const text = textOf(host)
    assert.ok(text.includes('22222222-2222-4222-8222-222222222222'))
    assert.ok(text.includes('scope-a'))
    assert.ok(text.includes('openai'))
    assert.ok(text.includes('7'), 'user id value')
    assert.ok(text.includes('Page 1'), 'first page index')
    assert.equal(
      host.querySelectorAll('tbody tr').length,
      2,
      'one row per session'
    )
  })

  test('passes applied filters to the sessions query', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    const modelInput = host.querySelector('input[placeholder="Model"]')
    assert.ok(modelInput, 'model filter input')
    setInputValue(modelInput, 'gpt-4o')
    clickButton(host, 'Search')
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    const lastCall = calls.at(-1) ?? ''
    assert.ok(
      lastCall.includes('model=gpt-4o'),
      `model filter must reach the api (got ${lastCall})`
    )
    assert.ok(
      !lastCall.includes('cursor='),
      'applying filters resets to the first page'
    )
  })

  test('passes advanced filters (expand + numeric user id) to the query', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    clickButton(host, 'Expand')
    const userIdInput = host.querySelector('input[placeholder="User ID"]')
    assert.ok(userIdInput, 'advanced filter input after expand')
    setInputValue(userIdInput, '7')
    clickButton(host, 'Search')
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    assert.ok(
      calls.at(-1)?.includes('user_id=7'),
      'numeric user id filter must reach the api'
    )
  })

  test('advances to the next page with the fetched cursor', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    clickButton(host, 'Next')
    await waitForText(host, '33333333-3333-4333-8333-333333333333')

    assert.ok(
      calls.at(-1)?.includes('cursor=cursor-2'),
      'next page must be fetched with the previous page cursor'
    )
    assert.ok(textOf(host).includes('Page 2'), 'page index advances')
  })

  test('steps back to the previous page', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111-1111-4111-8111-111111111111')
    clickButton(host, 'Next')
    await waitForText(host, '33333333-3333-4333-8333-333333333333')

    clickButton(host, 'Previous')
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    const lastCall = calls.at(-1) ?? ''
    assert.ok(
      !lastCall.includes('cursor='),
      `back must fetch the first page without a cursor (got ${lastCall})`
    )
    assert.ok(textOf(host).includes('Page 1'), 'page index steps back')
  })

  test('shows the degraded notice when the sessions envelope is degraded', async () => {
    handler = () => ({
      success: true,
      message: '',
      data: { degraded: true, reason: 'unavailable', message: 'store down' },
    })
    const { host } = renderTab()
    await waitForText(host, 'Observer data is temporarily unavailable')

    assert.ok(
      textOf(host).includes('The observer store is degraded. Please try again later.'),
      'degraded notice, not an error'
    )
    assert.ok(!textOf(host).includes('Page 1'), 'no pagination while degraded')
  })

  test('shows an empty state when no sessions match', async () => {
    handler = () => ({ success: true, message: '', data: EMPTY_PAGE })
    const { host } = renderTab()
    await waitForText(host, 'No Sessions Found')

    assert.ok(
      textOf(host).includes(
        'No sessions have been recorded yet. Sessions will appear here once the observer captures traffic.'
      )
    )
  })

  test('shows skeletons while loading, then the rows', async () => {
    handler = pageHandler
    const { host } = renderTab()

    assert.ok(
      host.querySelector('[data-slot=skeleton]') !== null,
      'table skeleton while loading'
    )

    await waitForText(host, '11111111-1111-4111-8111-111111111111')
    assert.equal(
      host.querySelectorAll('[data-slot=skeleton]').length,
      0,
      'skeletons removed after load'
    )
  })

  test('selects a session row on click (uncontrolled) and toggles it off', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    const row = findRow(host, '11111111-1111-4111-8111-111111111111')
    act(() => {
      row.dispatchEvent(clickEvent())
    })

    assert.equal(row.dataset.state, 'selected', 'clicked row highlights')
    assert.ok(
      textOf(host).includes('Selected session: 11111111-1111-4111-8111-111111111111'),
      'selection notice renders the session id'
    )

    act(() => {
      row.dispatchEvent(clickEvent())
    })
    assert.equal(row.dataset.state, undefined, 'second click deselects')
  })

  test('session rows are keyboard-selectable with Enter and Space', async () => {
    handler = pageHandler
    const selected: (string | null)[] = []
    const { host } = renderTab({
      selectedSessionId: null,
      onSelectSession: (id) => selected.push(id),
    })
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    const first = findRow(host, '11111111-1111-4111-8111-111111111111')
    const second = findRow(host, '22222222-2222-4222-8222-222222222222')
    assert.equal(first.getAttribute('role'), 'button')
    assert.equal(first.getAttribute('tabindex'), '0')

    act(() => {
      first.dispatchEvent(keyEvent('Enter'))
      second.dispatchEvent(keyEvent(' '))
    })

    assert.deepEqual(selected, [
      '11111111-1111-4111-8111-111111111111',
      '22222222-2222-4222-8222-222222222222',
    ])
  })

  test('reports selection changes through the controlled props (T4.3 seam)', async () => {
    handler = pageHandler
    const selected: (string | null)[] = []
    const { host } = renderTab({
      selectedSessionId: null,
      onSelectSession: (id) => {
        selected.push(id)
      },
    })
    await waitForText(host, '11111111-1111-4111-8111-111111111111')

    const row = findRow(host, '11111111-1111-4111-8111-111111111111')
    act(() => {
      row.dispatchEvent(clickEvent())
    })

    assert.deepEqual(selected, [
      '11111111-1111-4111-8111-111111111111',
    ])
    assert.equal(
      row.dataset.state,
      undefined,
      'controlled parent owns the highlight (selectedSessionId stays null)'
    )
  })

  test('renders the error state when the api fails', async () => {
    handler = () => {
      throw new Error('network down')
    }
    const { host } = renderTab()
    await waitForText(host, 'Failed to load sessions')

    assert.ok(textOf(host).includes('Failed to load sessions'))
  })
})
