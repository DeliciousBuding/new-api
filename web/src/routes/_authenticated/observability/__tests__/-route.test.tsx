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
import { afterAll, describe, expect, test } from 'vitest'

import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { Route } from '../route'

// The guard itself is what we test; a missing beforeLoad must fail the test,
// not silently pass.
const beforeLoad = Route.options.beforeLoad
expect(beforeLoad, 'route must define beforeLoad').toBeTruthy()
if (!beforeLoad) throw new Error('route must define beforeLoad')

function expectRedirectTo403(fn: () => void) {
  // TanStack Router's redirect() returns a Response-shaped object carrying
  // the target in options.to (this version's public shape).
  expect(fn).toThrowError(
    expect.objectContaining({
      options: expect.objectContaining({ to: '/403' }),
    })
  )
}

afterAll(() => {
  useAuthStore.getState().auth.setUser(null)
})

describe('observability route guard', () => {
  test('route carries the page title as static route metadata', () => {
    const staticData = Route.options.staticData as { title?: string }
    expect(staticData.title).toBe('Observability')
  })

  test('anonymous user is redirected to /403', () => {
    useAuthStore.getState().auth.setUser(null)
    expectRedirectTo403(() => beforeLoad({} as never))
  })

  test('regular user is redirected to /403', () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'user', role: ROLE.USER })
    expectRedirectTo403(() => beforeLoad({} as never))
  })

  test('admin is redirected to /403', () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 2, username: 'admin', role: ROLE.ADMIN })
    expectRedirectTo403(() => beforeLoad({} as never))
  })

  test('SUPER_ADMIN passes the guard', () => {
    useAuthStore.getState().auth.setUser({
      id: 100,
      username: 'root',
      role: ROLE.SUPER_ADMIN,
    })
    expect(() => beforeLoad({} as never)).not.toThrow()
  })
})
