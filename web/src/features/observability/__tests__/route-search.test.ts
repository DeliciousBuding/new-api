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
 * seam test: the observability route must accept the `?session=<id>`
 * search param (URL-state) that index.tsx reads to feed the Session Detail
 * tab. The schema lives on the route; a route without validateSearch parses
 * nothing and the Session Detail tab can never receive a session id.
 */
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { observabilitySearchSchema } from '@/routes/_authenticated/observability/route'

describe('observability route — ?session= URL-state', () => {
  test('parses the session search param', () => {
    const parsed = observabilitySearchSchema.parse({ session: 'abc-123' })
    assert.deepEqual(parsed, { session: 'abc-123' })
  })

  test('an absent session parses to an empty search state', () => {
    const parsed = observabilitySearchSchema.parse({})
    assert.deepEqual(parsed, {})
  })
})
