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
 * Keyset cursor pagination pattern for the Root observer pages (T4.1).
 *
 * The backend returns `{ items, meta: { next_cursor, has_more } }` and
 * cursors are opaque and forward-only, so "next" needs the previous page's
 * next_cursor and "back" needs a stack of cursors seen so far. The two
 * halves live in separate files:
 *
 *  - `use-cursor-pagination.ts` — the `useCursorPagination()` hook: page
 *    state (cursor stack) + navigation callbacks. Drive your useQuery with
 *    `pagination.cursor` and feed the fetched page's `meta.next_cursor`
 *    into `pagination.push`.
 *  - this file — `<CursorPagination />`, the footer bar (previous / page
 *    info / next).
 *
 * Usage (T4.2/T4.3):
 *
 *   const pagination = useCursorPagination()
 *   const query = useQuery({
 *     queryKey: observabilityQueryKeys.sessions.list({
 *       ...filters,
 *       cursor: pagination.cursor,
 *     }),
 *     queryFn: () => listSessions({ ...filters, cursor: pagination.cursor }),
 *     retry: false, // degraded envelope is HTTP 200; retries add nothing
 *   })
 *   const data = query.data?.data
 *   useEffect(() => {
 *     if (data && !isObserverDegraded(data)) pagination.push(data.meta.next_cursor)
 *   }, [data]) // eslint-disable-line react-hooks/exhaustive-deps
 *   ...
 *   <CursorPagination
 *     pageIndex={pagination.pageIndex}
 *     canGoBack={pagination.canGoBack}
 *     hasMore={hasMore}
 *     loading={query.isLoading}
 *     onBack={pagination.back}
 *     onNext={pagination.next}
 *   />
 *
 * This is deliberately NOT the shared offset-based ui/pagination: that
 * component models page numbers, which keyset cursors do not have. Do not
 * retrofit this pattern onto Admin tables — that boundary is intentional.
 *
 * URL state: like use-table-url-state, the cursor MAY go into the route
 * search params (optional — a page can also keep it purely in memory). If
 * you push it to the URL, reset the stack whenever the other filters change.
 */
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'

export interface CursorPaginationProps {
  /** Page index the controller is on (derived from the cursor stack). */
  pageIndex: number
  canGoBack: boolean
  /** Backend answer for the CURRENT page — drives the next button. */
  hasMore: boolean
  /** True while the list query is loading; disables both buttons. */
  loading?: boolean
  onBack: () => void
  onNext: () => void
}

/**
 * Footer bar of a keyset-paginated list: previous page, current page index,
 * next page. `hasMore` comes from the current page's meta — the "next"
 * button is enabled only while the backend reports more pages.
 */
export function CursorPagination(props: CursorPaginationProps) {
  const { t } = useTranslation()
  const isBusy = props.loading === true

  return (
    <div className='flex w-full items-center justify-between gap-2'>
      <Button
        variant='outline'
        size='sm'
        onClick={props.onBack}
        disabled={isBusy || !props.canGoBack}
      >
        {t('Previous')}
      </Button>
      <span className='text-muted-foreground text-sm'>
        {t('Page {{current}}', { current: props.pageIndex })}
      </span>
      <Button
        variant='outline'
        size='sm'
        onClick={props.onNext}
        disabled={isBusy || !props.hasMore}
      >
        {t('Next')}
      </Button>
    </div>
  )
}
