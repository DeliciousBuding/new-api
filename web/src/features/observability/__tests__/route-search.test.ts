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
 * Seam test: the observability route must accept the `?session=<id>` search
 * param (URL-state) that index.tsx reads to feed the Session Detail tab. The
 * schema lives on the index route's validateSearch; a route without
 * validateSearch parses nothing and the Session Detail tab can never receive
 * a session id.
 *
 * Note: importing the route module loads the full sessions-page component
 * tree. The import is dynamic and happens after the happy-dom globals below
 * are installed: a hoisted static import would evaluate those components
 * without a DOM and corrupt the shared React event state, breaking the
 * concurrent test runs in this directory.
 */
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { Window } from 'happy-dom'

const domWindow = new Window()
for (const key of [
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
] as const) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

const { observabilitySessionsSearchSchema } = await import(
  '@/routes/_authenticated/observability/index'
)

describe('observability route — ?session= URL-state', () => {
  test('parses the session search param', () => {
    const parsed = observabilitySessionsSearchSchema.parse({
      session: 'abc-123',
    })
    assert.deepEqual(parsed, { session: 'abc-123' })
  })

  test('an absent session parses to an empty search state', () => {
    const parsed = observabilitySessionsSearchSchema.parse({})
    assert.deepEqual(parsed, {})
  })
})
