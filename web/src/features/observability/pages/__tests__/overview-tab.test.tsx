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
 * OverviewTab behavior tests (node:test + happy-dom, pattern:
 * features/observability/components/__tests__/cursor-pagination.test.tsx).
 *
 * The api layer is stubbed at the shared axios instance (pattern:
 * features/observability/__tests__/api.test.ts), so useQuery drives the real
 * query path. All ids/timestamps are fabricated fixtures.
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

// Import the shared axios instance first (same module instance the feature
// api.ts binds to), then swap api.get for a URL-routing stub.
const { api } = await import('../../../../lib/http-client')
const { OverviewTab } = await import('../overview-tab')

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

const STATUS = {
  Enabled: true,
  ReasonCode: '',
  IPTrust: 'direct',
  QueueCount: 3,
  QueueBytes: 2048,
  AcceptedTotal: 100,
  WrittenTotal: 98,
  DroppedTotal: 2,
  CircuitOpen: false,
  CircuitCooldown: 0,
  PGLatencyMS: 12,
  ContentGapsTotal: 1,
  RecentVolume: 50,
  LastRetentionPass: '2026-08-02T00:00:00Z',
  RetentionTurnsDeleted: 10,
  RetentionSessionsDeleted: 2,
  RetentionObjectsDeleted: 5,
  RetentionFailures: 0,
}

const OVERVIEW = {
  window_seconds: 300,
  windows: [
    { start: '2026-08-02T00:00:00Z', turns: 10, success: 9 },
    { start: '2026-08-02T00:05:00Z', turns: 20, success: 18 },
  ],
  session_count: 7,
  turn_count: 30,
  gap_count: 3,
}

function healthyHandler(url: string): unknown {
  if (url === '/api/relay-observer/status') {
    return { success: true, message: '', data: STATUS }
  }
  if (url === '/api/relay-observer/overview?window_seconds=300&windows=12') {
    return { success: true, message: '', data: OVERVIEW }
  }
  return { success: true, message: '', data: {} }
}

function renderTab() {
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
          <OverviewTab />
        </I18nextProvider>
      </QueryClientProvider>
    )
  })
  return { host, root }
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

function textOf(host: HTMLElement): string {
  return host.textContent ?? ''
}

describe('OverviewTab', () => {
  test('renders the status snapshot, window rows and totals', async () => {
    handler = healthyHandler
    const { host } = renderTab()
    await waitForText(host, 'Observer Status')

    const text = textOf(host)
    assert.ok(text.includes('Observer Status'), 'status card title')
    assert.ok(text.includes('Enabled'), 'enabled badge')
    assert.ok(text.includes('Queue Count'), 'queue section label')
    assert.ok(text.includes('2,048'), 'queue bytes value (toLocaleString)')
    assert.ok(text.includes('Accepted Total'), 'counters section label')
    assert.ok(text.includes('100'), 'accepted total value')
    assert.ok(text.includes('Circuit Open'), 'circuit section label')
    assert.ok(text.includes('Retention Turns Deleted'), 'retention section')
    assert.ok(text.includes('Window Aggregation'), 'windows card title')
    assert.ok(text.includes('10'), 'first window turn count')
    assert.ok(text.includes('20'), 'second window turn count')
    assert.ok(text.includes('Total Sessions'), 'totals card')
    assert.ok(text.includes('7'), 'total session count')
    assert.ok(text.includes('30'), 'total turn count')
    assert.ok(text.includes('3'), 'total gap count')
  })

  test('renders one table row per aggregate window', async () => {
    handler = healthyHandler
    const { host } = renderTab()
    await waitForText(host, 'Observer Status')

    const rows = host.querySelectorAll('tbody tr')
    assert.equal(rows.length, 2)
  })

  test('shows the degraded notice when the overview envelope is degraded', async () => {
    handler = (url: string) => {
      if (url === '/api/relay-observer/status') {
        return { success: true, message: '', data: STATUS }
      }
      return {
        success: true,
        message: '',
        data: { degraded: true, reason: 'timeout', message: 'store timeout' },
      }
    }
    const { host } = renderTab()
    await waitForText(host, 'Observer data is temporarily unavailable')

    const text = textOf(host)
    assert.ok(
      text.includes('Observer data is temporarily unavailable'),
      'degraded notice must render, not an error'
    )
    assert.ok(!text.includes('Observer Status'), 'no status card on degraded')
  })

  test('shows an empty state when no windows exist', async () => {
    handler = (url: string) => {
      if (url === '/api/relay-observer/status') {
        return { success: true, message: '', data: STATUS }
      }
      return {
        success: true,
        message: '',
        data: { ...OVERVIEW, windows: [] },
      }
    }
    const { host } = renderTab()
    await waitForText(host, 'Observer Status')

    assert.ok(
      textOf(host).includes('No window aggregates available yet.'),
      'windows table empty state'
    )
  })

  test('shows skeletons while loading, then the data', async () => {
    handler = healthyHandler
    const { host } = renderTab()

    // Before the fetch settles the tab renders skeletons (native loading
    // pattern: features/usage-logs/usage-logs-table isLoadingData).
    assert.ok(
      host.querySelector('[data-slot=skeleton]') !== null,
      'skeleton while loading'
    )

    await waitForText(host, 'Observer Status')
    assert.ok(textOf(host).includes('Observer Status'), 'data after load')
    assert.equal(
      host.querySelectorAll('[data-slot=skeleton]').length,
      0,
      'skeletons removed after load'
    )
  })

  test('formats the circuit cooldown in seconds, not raw nanoseconds', async () => {
    // Backend Status has no JSON tags and CircuitCooldown is a time.Duration,
    // so the wire value is nanoseconds (dispatcher.go: CircuitCooldown =
    // time.Duration(circuitUntilNano - nowNano)).
    handler = (url: string) => {
      if (url === '/api/relay-observer/status') {
        return {
          success: true,
          message: '',
          data: {
            ...STATUS,
            CircuitOpen: true,
            CircuitCooldown: 30_000_000_000, // 30 s in nanoseconds
          },
        }
      }
      return { success: true, message: '', data: OVERVIEW }
    }
    const { host } = renderTab()
    await waitForText(host, 'Circuit Cooldown')

    const text = textOf(host)
    assert.ok(
      text.includes('30 s'),
      'cooldown must render in seconds, not raw nanoseconds'
    )
    assert.ok(
      !text.includes('30000000000 s'),
      'raw nanosecond value must not leak into the UI'
    )
  })

  test('renders the error state when the api fails', async () => {
    handler = () => {
      throw new Error('network down')
    }
    const { host } = renderTab()
    await waitForText(host, 'Failed to load overview')

    assert.ok(
      textOf(host).includes('Failed to load overview'),
      'error state title'
    )
  })
})
