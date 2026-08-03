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
import { after, describe, test } from 'node:test'

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
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        Previous: 'Previous',
        Next: 'Next',
        'Page {{current}}': 'Page {{current}}',
      },
    },
  },
})

const { CursorPagination } = await import('../cursor-pagination')

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

function renderBar(props: {
  pageIndex: number
  canGoBack: boolean
  hasMore: boolean
  loading?: boolean
  onBack: () => void
  onNext: () => void
}) {
  const host = document.createElement('div')
  document.body.appendChild(host)
  const root = createRoot(host)
  act(() => {
    root.render(
      <I18nextProvider i18n={i18n}>
        <CursorPagination {...props} />
      </I18nextProvider>
    )
  })
  return { host, root }
}

function textOf(host: HTMLElement): string {
  return host.textContent ?? ''
}

after(() => {
  document.body.innerHTML = ''
})

describe('CursorPagination (keyset footer bar)', () => {
  test('renders page index, previous and next buttons', () => {
    const { host } = renderBar({
      pageIndex: 3,
      canGoBack: true,
      hasMore: true,
      onBack: () => {},
      onNext: () => {},
    })
    assert.match(textOf(host), /Page 3/)
    assert.ok(host.querySelector('button[disabled]') === null)
  })

  test('previous is disabled on the first page', () => {
    const { host } = renderBar({
      pageIndex: 1,
      canGoBack: false,
      hasMore: true,
      onBack: () => {},
      onNext: () => {},
    })
    const [previous, next] = host.querySelectorAll('button')
    assert.ok(previous.disabled, 'previous must be disabled without a back page')
    assert.ok(!next.disabled, 'next stays enabled while has_more is true')
  })

  test('next is disabled when the backend reports no more pages', () => {
    const { host } = renderBar({
      pageIndex: 1,
      canGoBack: false,
      hasMore: false,
      onBack: () => {},
      onNext: () => {},
    })
    const [previous, next] = host.querySelectorAll('button')
    assert.ok(previous.disabled)
    assert.ok(next.disabled, 'next must be disabled when has_more is false')
  })

  test('both buttons are disabled while the list is loading', () => {
    const { host } = renderBar({
      pageIndex: 2,
      canGoBack: true,
      hasMore: true,
      loading: true,
      onBack: () => {},
      onNext: () => {},
    })
    const buttons = host.querySelectorAll('button')
    for (const button of buttons) {
      assert.ok(button.disabled, 'buttons must be disabled while loading')
    }
  })

  test('clicking next/previous fires the navigation callbacks', () => {
    let nextClicks = 0
    let backClicks = 0
    const { host } = renderBar({
      pageIndex: 2,
      canGoBack: true,
      hasMore: true,
      onBack: () => {
        backClicks++
      },
      onNext: () => {
        nextClicks++
      },
    })
    const [previous, next] = host.querySelectorAll('button')
    act(() => {
      next.dispatchEvent(
        new domWindow.Event('click', { bubbles: true }) as unknown as Event
      )
    })
    act(() => {
      previous.dispatchEvent(
        new domWindow.Event('click', { bubbles: true }) as unknown as Event
      )
    })
    assert.equal(nextClicks, 1)
    assert.equal(backClicks, 1)
  })
})
