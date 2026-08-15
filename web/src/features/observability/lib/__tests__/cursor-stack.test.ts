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
import { describe, expect, test } from 'vitest'

import {
  currentPageIndex,
  initialCursorStack,
  popCursor,
  pushCursor,
} from '../cursor-stack'

describe('cursor stack (keyset pagination bookkeeping)', () => {
  test('starts on the first page with no cursor', () => {
    const state = initialCursorStack()
    expect(state).toEqual({ history: [], current: undefined })
    expect(currentPageIndex(state)).toBe(1)
  })

  test('push advances: previous cursor (undefined on page 1) becomes history', () => {
    const first = initialCursorStack()
    const second = pushCursor(first, 'cursor-1')
    expect(second).toEqual({ history: [undefined], current: 'cursor-1' })
    expect(currentPageIndex(second)).toBe(2)
    expect(second.history.length).toBe(1)

    const third = pushCursor(second, 'cursor-2')
    expect(third).toEqual({
      history: [undefined, 'cursor-1'],
      current: 'cursor-2',
    })
    expect(currentPageIndex(third)).toBe(3)
  })

  test('push with an empty next_cursor is a no-op (has_more=false case)', () => {
    const state = pushCursor(pushCursor(initialCursorStack(), 'cursor-1'), '')
    expect(state).toEqual({ history: [undefined], current: 'cursor-1' })
    expect(currentPageIndex(state)).toBe(2)
  })

  test('pop restores the cursor the previous page was fetched with', () => {
    const state = pushCursor(
      pushCursor(initialCursorStack(), 'cursor-1'),
      'cursor-2'
    )
    const back = popCursor(state)
    expect(back).toEqual({ history: [undefined], current: 'cursor-1' })
    expect(currentPageIndex(back)).toBe(2)

    const first = popCursor(back)
    expect(first).toEqual({ history: [], current: undefined })
    expect(currentPageIndex(first)).toBe(1)
  })

  test('pop on the first page is a no-op', () => {
    const state = popCursor(initialCursorStack())
    expect(state).toEqual({ history: [], current: undefined })
  })

  test('stack is immutable: push/pop never mutate the input state', () => {
    const first = initialCursorStack()
    const second = pushCursor(first, 'cursor-1')
    const third = pushCursor(second, 'cursor-2')
    expect(first).toEqual({ history: [], current: undefined })
    expect(second).toEqual({ history: [undefined], current: 'cursor-1' })
    popCursor(third)
    expect(third).toEqual({
      history: [undefined, 'cursor-1'],
      current: 'cursor-2',
    })
  })
})
