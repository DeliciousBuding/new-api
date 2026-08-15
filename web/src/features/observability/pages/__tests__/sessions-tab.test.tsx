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
import { afterAll, afterEach, describe, expect, test, vi } from 'vitest'

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createInstance } from 'i18next'
import { act } from 'react'
import { createRoot } from 'react-dom/client'
import { I18nextProvider, initReactI18next } from 'react-i18next'

import { api } from '../../../../lib/http-client'
import { SessionsTab } from '../sessions-tab'

// Session rows render client profile badges through the fork's lobe-icon
// loader. @lobehub/icons uses ESM directory imports that Vite's resolver
// cannot follow; stub the loader — these tests cover row structure and
// labels, not upstream SVG internals.
// Vitest hoists vi.mock, so the stub applies to the imports above.
vi.mock('@/lib/lobe-icon', async () => {
  const React = await import('react')
  return {
    getLobeIcon: () =>
      React.createElement('svg', { 'data-mock-lobe-icon': 'true' }),
  }
})

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        // Missing keys fall back to the key itself, so assertions use the
        // English source strings (project i18n convention: key = English).
        'Page {{current}}': 'Page {{current}}',
        'Selected session: {{sessionId}}': 'Selected session: {{sessionId}}',
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
const stubGet = (async (url: unknown) => {
  calls.push(String(url))
  return { data: handler(String(url)) }
}) as typeof api.get
api.get = stubGet

afterAll(() => {
  // Chain-safe restore: only unwrap when this file's stub is still the one
  // installed (bun runs test files concurrently and every file swaps the
  // shared http-client singleton).
  if (api.get === stubGet) api.get = originalGet
})

afterEach(() => {
  for (const root of activeRoots.splice(0)) {
    act(() => {
      root.unmount()
    })
  }
  calls.length = 0
  handler = () => ({ success: true, message: '', data: {} })
  document.body.innerHTML = ''
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

// createRoot trees are not covered by RTL's automatic cleanup (this file
// renders through react-dom/client directly), so roots are tracked and
// unmounted after each test. Unmounting stops motion-driven skeleton
// animations; without it the frame loop survives into teardown and fires a
// post-teardown requestAnimationFrame against a torn-down environment.
const activeRoots: ReturnType<typeof createRoot>[] = []

function renderTab(props?: {
  selectedSessionId?: string | null
  onSelectSession?: (id: string | null) => void
}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const host = document.createElement('div')
  document.body.appendChild(host)
  const root = createRoot(host)
  activeRoots.push(root)
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

/** Poll a predicate (e.g. for network calls) inside act() until it holds. */
async function waitFor(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 2000
  while (Date.now() < deadline) {
    let reached = false
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10))
      reached = predicate()
    })
    if (reached) return
  }
  expect(predicate(), 'waitFor timeout').toBeTruthy()
}

function setInputValue(input: Element, value: string) {
  const descriptor = Object.getOwnPropertyDescriptor(
    HTMLInputElement.prototype,
    'value'
  )
  expect(
    descriptor?.set,
    'HTMLInputElement value setter must exist'
  ).toBeTruthy()
  if (!descriptor?.set) {
    throw new Error('HTMLInputElement value setter must exist')
  }
  descriptor.set.call(input, value)
  input.dispatchEvent(new Event('input', { bubbles: true }))
}

function clickEvent(): Event {
  return new Event('click', { bubbles: true })
}

function keyEvent(key: 'Enter' | ' '): KeyboardEvent {
  return new KeyboardEvent('keydown', {
    bubbles: true,
    cancelable: true,
    key,
  })
}

function clickButton(host: HTMLElement, label: string) {
  const button = [...host.querySelectorAll('button')].find((b) =>
    b.textContent?.includes(label)
  )
  expect(button, `button "${label}" not found`).toBeTruthy()
  if (!button) throw new Error(`button "${label}" not found`)
  act(() => {
    button.dispatchEvent(clickEvent())
  })
}

function findRow(host: HTMLElement, text: string): HTMLElement {
  const row = [...host.querySelectorAll('tbody tr')].find((r) =>
    r.textContent?.includes(text)
  )
  expect(row, `table row containing "${text}" not found`).toBeTruthy()
  if (!row) throw new Error(`table row containing "${text}" not found`)
  return row as HTMLElement
}

describe('SessionsTab', () => {
  test('renders session rows from the first page', async () => {
    handler = pageHandler
    const { host } = renderTab()
    // The session column renders the short id (first 8 chars), the same
    // compact display as the task summary card.
    await waitForText(host, '11111111')

    const text = textOf(host)
    expect(text.includes('22222222')).toBeTruthy()
    expect(text.includes('#7'), 'user id value without username').toBeTruthy()
    expect(
      text.includes('openai'),
      'client profile fallback label'
    ).toBeTruthy()
    expect(text.includes('Page 1'), 'first page index').toBeTruthy()
    expect(
      host.querySelectorAll('tbody tr').length,
      'one row per session'
    ).toBe(2)
  })

  test('passes applied filters to the sessions query', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111')

    const modelInput = host.querySelector('input[placeholder="Model"]')
    expect(modelInput, 'model filter input').toBeTruthy()
    if (!modelInput) throw new Error('model filter input')
    setInputValue(modelInput, 'gpt-4o')
    clickButton(host, 'Search')
    // The first page stays on screen while the refetch is in flight, so
    // wait for the filtered request itself instead of a text change.
    await waitFor(() => calls.some((url) => url.includes('model=gpt-4o')))

    const lastCall = calls.at(-1) ?? ''
    expect(
      lastCall.includes('model=gpt-4o'),
      `model filter must reach the api (got ${lastCall})`
    ).toBeTruthy()
    expect(
      lastCall.includes('cursor='),
      'applying filters resets to the first page'
    ).toBeFalsy()
  })

  test('passes advanced filters (expand + numeric user id) to the query', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111')

    clickButton(host, 'Expand')
    const userIdInput = host.querySelector('input[placeholder="User ID"]')
    expect(userIdInput, 'advanced filter input after expand').toBeTruthy()
    if (!userIdInput) throw new Error('advanced filter input after expand')
    setInputValue(userIdInput, '7')
    clickButton(host, 'Search')
    await waitFor(() => calls.some((url) => url.includes('user_id=7')))

    expect(
      calls.at(-1)?.includes('user_id=7'),
      'numeric user id filter must reach the api'
    ).toBeTruthy()
  })

  test('advances to the next page with the fetched cursor', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111')

    clickButton(host, 'Next')
    await waitForText(host, '33333333')

    expect(
      calls.at(-1)?.includes('cursor=cursor-2'),
      'next page must be fetched with the previous page cursor'
    ).toBeTruthy()
    expect(textOf(host).includes('Page 2'), 'page index advances').toBeTruthy()
  })

  test('steps back to the previous page', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111')
    clickButton(host, 'Next')
    await waitForText(host, '33333333')

    clickButton(host, 'Previous')
    await waitForText(host, '11111111')

    const lastCall = calls.at(-1) ?? ''
    expect(
      lastCall.includes('cursor='),
      `back must fetch the first page without a cursor (got ${lastCall})`
    ).toBeFalsy()
    expect(
      textOf(host).includes('Page 1'),
      'page index steps back'
    ).toBeTruthy()
  })

  test('shows the degraded notice when the sessions envelope is degraded', async () => {
    handler = () => ({
      success: true,
      message: '',
      data: { degraded: true, reason: 'unavailable', message: 'store down' },
    })
    const { host } = renderTab()
    await waitForText(host, 'Observer data is temporarily unavailable')

    expect(
      textOf(host).includes(
        'The observer store is degraded. Please try again later.'
      ),
      'degraded notice, not an error'
    ).toBeTruthy()
    expect(
      textOf(host).includes('Page 1'),
      'no pagination while degraded'
    ).toBeFalsy()
  })

  test('shows an empty state when no sessions match', async () => {
    handler = () => ({ success: true, message: '', data: EMPTY_PAGE })
    const { host } = renderTab()
    await waitForText(host, 'No Sessions Found')

    expect(
      textOf(host).includes(
        'No sessions have been recorded yet. Sessions will appear here once the observer captures traffic.'
      )
    ).toBeTruthy()
  })

  test('shows skeletons while loading, then the rows', async () => {
    let release!: (value: unknown) => void
    const pending = new Promise((resolve) => {
      release = resolve
    })
    handler = () => pending
    const { host } = renderTab()

    expect(
      host.querySelector('[data-slot=skeleton]') !== null,
      'table skeleton while loading'
    ).toBeTruthy()

    await act(async () => {
      release({
        success: true,
        message: '',
        data: PAGE_1,
      })
    })
    await waitForText(host, '11111111')
    expect(
      host.querySelectorAll('[data-slot=skeleton]').length,
      'skeletons removed after load'
    ).toBe(0)
  })

  test('selects a session row on click (uncontrolled) and toggles it off', async () => {
    handler = pageHandler
    const { host } = renderTab()
    await waitForText(host, '11111111')

    const row = findRow(host, '11111111')
    act(() => {
      row.dispatchEvent(clickEvent())
    })

    expect(row.dataset.state, 'clicked row highlights').toBe('selected')
    expect(
      textOf(host).includes(
        'Selected session: 11111111-1111-4111-8111-111111111111'
      ),
      'selection notice renders the session id'
    ).toBeTruthy()

    act(() => {
      row.dispatchEvent(clickEvent())
    })
    expect(row.dataset.state, 'second click deselects').toBe(undefined)
  })

  test('session rows are keyboard-selectable with Enter and Space', async () => {
    handler = pageHandler
    const selected: (string | null)[] = []
    const { host } = renderTab({
      selectedSessionId: null,
      onSelectSession: (id) => selected.push(id),
    })
    await waitForText(host, '11111111')

    const first = findRow(host, '11111111')
    const second = findRow(host, '22222222')
    expect(first.getAttribute('role')).toBe('button')
    expect(first.getAttribute('tabindex')).toBe('0')

    act(() => {
      first.dispatchEvent(keyEvent('Enter'))
      second.dispatchEvent(keyEvent(' '))
    })

    expect(selected).toEqual([
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
    await waitForText(host, '11111111')

    const row = findRow(host, '11111111')
    act(() => {
      row.dispatchEvent(clickEvent())
    })

    expect(selected).toEqual(['11111111-1111-4111-8111-111111111111'])
    expect(
      row.dataset.state,
      'controlled parent owns the highlight (selectedSessionId stays null)'
    ).toBe(undefined)
  })

  test('renders the error state when the api fails', async () => {
    handler = () => {
      throw new Error('network down')
    }
    const { host } = renderTab()
    await waitForText(host, 'Failed to load sessions')

    expect(textOf(host).includes('Failed to load sessions')).toBeTruthy()
  })
})
