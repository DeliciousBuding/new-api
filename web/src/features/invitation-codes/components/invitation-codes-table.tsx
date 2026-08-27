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
import { useQuery } from '@tanstack/react-query'
import { getRouteApi } from '@tanstack/react-router'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import {
  DISABLED_ROW_DESKTOP,
  DISABLED_ROW_MOBILE,
  DataTablePage,
  useDataTable,
} from '@/components/data-table'
import { useMediaQuery } from '@/hooks'
import { useTableUrlState } from '@/hooks/use-table-url-state'

import { getInvitationCodes, searchInvitationCodes } from '../api'
import {
  ERROR_MESSAGES,
  INVITATION_STATUS,
  getInvitationStatusOptions,
} from '../constants'
import { isInvitationExpired, isInvitationExhausted } from '../lib'
import type { InvitationCode } from '../types'
import { DataTableBulkActions } from './data-table-bulk-actions'
import { useInvitationCodesColumns } from './invitation-codes-columns'
import { InvitationCodesMobileList } from './invitation-codes-mobile-list'
import { useInvitationCodes } from './invitation-codes-provider'

const route = getRouteApi('/_authenticated/invitation-codes/')

function isDisabledInvitationRow(invitationCode: InvitationCode) {
  return (
    invitationCode.status !== INVITATION_STATUS.ENABLED ||
    isInvitationExpired(invitationCode.expired_time, invitationCode.status) ||
    isInvitationExhausted(
      invitationCode.used_count,
      invitationCode.max_uses,
      invitationCode.status
    )
  )
}

export function InvitationCodesTable() {
  const { t } = useTranslation()
  const columns = useInvitationCodesColumns()
  const { refreshTrigger } = useInvitationCodes()
  const isMobile = useMediaQuery('(max-width: 640px)')

  const {
    globalFilter,
    onGlobalFilterChange,
    columnFilters,
    onColumnFiltersChange,
    pagination,
    onPaginationChange,
    ensurePageInRange,
  } = useTableUrlState({
    search: route.useSearch(),
    navigate: route.useNavigate(),
    pagination: { defaultPage: 1, defaultPageSize: isMobile ? 10 : 20 },
    globalFilter: { enabled: true, key: 'filter' },
    columnFilters: [{ columnId: 'status', searchKey: 'status', type: 'array' }],
  })
  const statusFilter =
    (columnFilters.find((filter) => filter.id === 'status')?.value as
      | string[]
      | undefined) ?? []
  const statusFilterValue = statusFilter[0] ?? ''

  // Fetch data with React Query
  const { data, isLoading, isFetching } = useQuery({
    queryKey: [
      'invitationCodes',
      pagination.pageIndex + 1,
      pagination.pageSize,
      globalFilter,
      statusFilterValue,
      refreshTrigger,
    ],
    queryFn: async () => {
      const hasFilter = globalFilter?.trim()
      const hasStatusFilter = statusFilterValue !== ''
      const params = {
        p: pagination.pageIndex + 1,
        page_size: pagination.pageSize,
      }

      const result =
        hasFilter || hasStatusFilter
          ? await searchInvitationCodes({
              ...params,
              keyword: globalFilter,
              status: statusFilterValue,
            })
          : await getInvitationCodes(params)

      if (!result.success) {
        toast.error(
          result.message ||
            t(
              hasFilter || hasStatusFilter
                ? ERROR_MESSAGES.SEARCH_FAILED
                : ERROR_MESSAGES.LOAD_FAILED
            )
        )
        return { items: [], total: 0 }
      }

      return {
        items: result.data?.items || [],
        total: result.data?.total || 0,
      }
    },
    placeholderData: (previousData) => previousData,
  })

  const invitationCodes = data?.items || []

  const { table } = useDataTable({
    data: invitationCodes,
    columns,
    enableRowSelection: true,
    columnFilters,
    globalFilter,
    pagination,
    globalFilterFn: (row, _columnId, filterValue) => {
      const name = String(row.getValue('name')).toLowerCase()
      const id = String(row.getValue('id'))
      const searchValue = String(filterValue).toLowerCase()

      return name.includes(searchValue) || id.includes(searchValue)
    },
    onPaginationChange,
    onGlobalFilterChange,
    onColumnFiltersChange,
    manualPagination: true,
    manualFiltering: true,
    totalCount: data?.total || 0,
    ensurePageInRange,
  })

  const invitationStatusOptions = useMemo(
    () => getInvitationStatusOptions(t),
    [t]
  )

  return (
    <DataTablePage
      table={table}
      columns={columns}
      isLoading={isLoading}
      isFetching={isFetching}
      emptyTitle={t('No Invitation Codes Found')}
      emptyDescription={t(
        'No invitation codes available. Create your first invitation code to get started.'
      )}
      skeletonKeyPrefix='invitation-codes-skeleton'
      applyHeaderSize
      toolbarProps={{
        searchPlaceholder: t('Filter by name or ID...'),
        searchDebounceMs: 500,
        filters: [
          {
            columnId: 'status',
            title: t('Status'),
            options: invitationStatusOptions,
            singleSelect: true,
          },
        ],
      }}
      mobile={<InvitationCodesMobileList table={table} isLoading={isLoading} />}
      getRowClassName={(row, { isMobile }) => {
        if (!isDisabledInvitationRow(row.original)) return undefined
        return isMobile ? DISABLED_ROW_MOBILE : DISABLED_ROW_DESKTOP
      }}
      bulkActions={<DataTableBulkActions table={table} />}
    />
  )
}
