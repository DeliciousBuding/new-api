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
 */
import { describe, expect, test, vi } from 'vitest'

import { observabilitySessionsSearchSchema } from '@/routes/_authenticated/observability/index'

// The route module pulls in the sessions page tree, which renders client
// profile badges through the fork's lobe-icon loader. @lobehub/icons uses
// ESM directory imports that Vite's resolver cannot follow, so stub the
// loader — this seam test only checks the route search schema.
// Vitest hoists vi.mock, so the stub applies to the import above.
vi.mock('@/lib/lobe-icon', async () => {
  const React = await import('react')
  return {
    getLobeIcon: () =>
      React.createElement('svg', { 'data-mock-lobe-icon': 'true' }),
  }
})

describe('observability route — ?session= URL-state', () => {
  test('parses the session search param', () => {
    const parsed = observabilitySessionsSearchSchema.parse({
      session: 'abc-123',
    })
    expect(parsed).toEqual({ session: 'abc-123' })
  })

  test('an absent session parses to an empty search state', () => {
    const parsed = observabilitySessionsSearchSchema.parse({})
    expect(parsed).toEqual({})
  })
})
