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
        'Country:': 'Country:',
        'City:': 'City:',
        'ASN:': 'ASN:',
      },
    },
  },
})

const { IpGeoBadge } = await import('../ip-geo-badge')

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

function renderBadge(props: {
  ip: string
  geo?: { country_code?: string; country?: string; city?: string; asn?: number; asn_org?: string }
}) {
  const host = document.createElement('div')
  document.body.appendChild(host)
  const root = createRoot(host)
  act(() => {
    root.render(
      <I18nextProvider i18n={i18n}>
        <IpGeoBadge {...props} />
      </I18nextProvider>
    )
  })
  return { host, root }
}

after(() => {
  document.body.innerHTML = ''
})

describe('IpGeoBadge', () => {
  test('shows the raw IP address (no masking for admins)', () => {
    const { host, root } = renderBadge({ ip: '1.2.3.4' })
    assert.ok(host.textContent?.includes('1.2.3.4'), 'IP must be visible in plain text')
    root.unmount()
  })

  test('renders a country flag when country_code is present', () => {
    const { host, root } = renderBadge({
      ip: '1.2.3.4',
      geo: { country_code: 'CN', country: 'China' },
    })
    assert.ok(
      host.querySelector('.fi-cn'),
      'flag class fi-cn should be rendered for CN'
    )
    root.unmount()
  })

  test('falls back to a globe icon without country_code', () => {
    const { host, root } = renderBadge({ ip: '1.2.3.4' })
    assert.ok(host.querySelector('svg'), 'globe fallback icon should render')
    root.unmount()
  })

  test('ignores malformed country codes', () => {
    const { host, root } = renderBadge({
      ip: '1.2.3.4',
      geo: { country_code: 'cn', country: 'China' },
    })
    assert.ok(!host.querySelector('[class*="fi-"]'), 'lowercase code must not map to a flag')
    root.unmount()
  })

  test('exposes locality details via popover trigger when geo is resolved', () => {
    const { host, root } = renderBadge({
      ip: '1.2.3.4',
      geo: { country_code: 'CN', country: 'China', city: 'Shenzhen', asn: 4134, asn_org: 'Chinanet' },
    })
    assert.ok(
      host.querySelector('button'),
      'resolved geo should wrap the chip in a popover trigger'
    )
    root.unmount()
  })

  test('stays a plain chip without geo details', () => {
    const { host, root } = renderBadge({ ip: '1.2.3.4' })
    assert.ok(!host.querySelector('button'), 'no geo means no popover trigger')
    root.unmount()
  })
})
