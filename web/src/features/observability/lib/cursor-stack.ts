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
 * Pure keyset cursor-stack state for the Root observer pages.
 *
 * Contract (T3 HTTP): the backend returns `{ items, meta: { next_cursor,
 * has_more } }`. Cursors are opaque and forward-only — a page is addressed
 * by the PREVIOUS page's cursor, so going back requires remembering the
 * cursors seen so far. This module owns that bookkeeping as pure functions
 * (no React), which is what makes the pagination logic unit-testable.
 *
 * The React binding lives in components/cursor-pagination.tsx.
 */

export interface CursorStackState {
  /** Cursors the pages BEFORE the current one were fetched with, in visit
   * order (the first page's entry is undefined — it had no cursor). Empty
   * means the current page is the first page. */
  history: Array<string | undefined>
  /** Cursor the current page was fetched with; undefined = first page. */
  current: string | undefined
}

export const initialCursorStack = (): CursorStackState => ({
  history: [],
  current: undefined,
})

/**
 * Advance to the next page: the current page's cursor (undefined on the
 * first page) becomes history and the new page's next_cursor becomes
 * current. An empty next_cursor is a no-op (the backend returns '' when
 * has_more is false — there is nowhere to go).
 */
export function pushCursor(
  state: CursorStackState,
  nextCursor: string
): CursorStackState {
  if (!nextCursor) return state
  return {
    history: [...state.history, state.current],
    current: nextCursor,
  }
}

/**
 * Step back to the previous page, restoring the cursor it was fetched with.
 * A no-op when already on the first page.
 */
export function popCursor(state: CursorStackState): CursorStackState {
  if (state.history.length === 0) return state
  const history = [...state.history]
  return {
    history: history.slice(0, -1),
    current: history.at(-1),
  }
}

/** 1-based index of the page the state is currently on. */
export function currentPageIndex(state: CursorStackState): number {
  return state.history.length + 1
}
