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
 * React binding of the keyset cursor stack (pure logic in
 * lib/cursor-stack.ts). Kept out of cursor-pagination.tsx so that file only
 * exports components (fast-refresh rule).
 *
 * Usage: drive your useQuery with `pagination.cursor` and feed the fetched
 * page's `meta.next_cursor` into `pagination.push` (see the module doc of
 * components/cursor-pagination.tsx for the full pattern).
 */
import { useCallback, useState } from 'react'

import {
  currentPageIndex,
  initialCursorStack,
  popCursor,
  pushCursor,
  type CursorStackState,
} from '../lib/cursor-stack'

export interface CursorPaginationController {
  /** Cursor to pass into the list query; undefined = first page. */
  cursor: string | undefined
  /** True when a page exists behind the current one. */
  canGoBack: boolean
  /** 1-based index of the page the stack is on (history depth + 1). */
  pageIndex: number
  /** Commit the fetched page's next_cursor (no-op for ''). */
  push: (nextCursor: string) => void
  /** Step back to the previous page. */
  back: () => void
  /** Clear the stack (call when filters change). */
  reset: () => void
}

/**
 * Page state for one keyset-paginated list. Independent of React Query —
 * the caller wires cursor into its query key and query function.
 */
export function useCursorPagination(): CursorPaginationController {
  const [state, setState] = useState<CursorStackState>(initialCursorStack)

  const push = useCallback((nextCursor: string) => {
    if (!nextCursor) return
    setState((prev) => pushCursor(prev, nextCursor))
  }, [])

  const back = useCallback(() => {
    setState((prev) => popCursor(prev))
  }, [])

  const reset = useCallback(() => {
    setState(initialCursorStack)
  }, [])

  return {
    cursor: state.current,
    canGoBack: state.history.length > 0,
    pageIndex: currentPageIndex(state),
    push,
    back,
    reset,
  }
}
