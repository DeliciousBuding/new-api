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

const { Route } = await import('../route')
const { useAuthStore } = await import('@/stores/auth-store')
const { ROLE } = await import('@/lib/roles')

// The guard itself is what we test; a missing beforeLoad must fail the test,
// not silently pass.
const beforeLoad = Route.options.beforeLoad
assert.ok(beforeLoad, 'route must define beforeLoad')

function expectRedirectTo403(fn: () => void) {
  let thrown: unknown
  try {
    fn()
  } catch (err) {
    thrown = err
  }
  assert.ok(thrown, 'beforeLoad must throw')
  // TanStack Router's redirect() returns a Response-shaped object carrying
  // the target in options.to (this version's public shape).
  const options = (thrown as { options?: { to?: string } }).options
  assert.equal(options?.to, '/403')
}

after(() => {
  useAuthStore.getState().auth.setUser(null)
})

describe('observability route guard', () => {
  test('route carries the page title as static route metadata', () => {
    const staticData = Route.options.staticData as { title?: string }
    assert.equal(staticData.title, 'Observability')
  })

  test('anonymous user is redirected to /403', () => {
    useAuthStore.getState().auth.setUser(null)
    expectRedirectTo403(() => beforeLoad({} as never))
  })

  test('regular user is redirected to /403', () => {
    useAuthStore.getState().auth.setUser({ id: 1, username: 'user', role: ROLE.USER })
    expectRedirectTo403(() => beforeLoad({} as never))
  })

  test('admin is redirected to /403', () => {
    useAuthStore.getState().auth.setUser({ id: 2, username: 'admin', role: ROLE.ADMIN })
    expectRedirectTo403(() => beforeLoad({} as never))
  })

  test('SUPER_ADMIN passes the guard', () => {
    useAuthStore.getState().auth.setUser({
      id: 100,
      username: 'root',
      role: ROLE.SUPER_ADMIN,
    })
    assert.doesNotThrow(() => beforeLoad({} as never))
  })
})
