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
import { createFileRoute, redirect } from '@tanstack/react-router'
import { z } from 'zod'

import { Observability } from '@/features/observability'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

/**
 * seam — the ?session=<id> URL-state that feeds the Session Detail tab
 * (pattern: usage-logs/$section.tsx validateSearch; the workspace index
 * reads it with getRouteApi and passes it to SessionDetailTab). Exported so
 * the seam is directly testable.
 */
export const observabilitySearchSchema = z.object({
  session: z.string().optional(),
})

export const Route = createFileRoute('/_authenticated/observability')({
  beforeLoad: () => {
    const { auth } = useAuthStore.getState()

    if (auth.user?.role !== ROLE.SUPER_ADMIN) {
      throw redirect({
        to: '/403',
      })
    }
  },
  // Static route metadata (this router version's replacement for the removed
  // `meta` route option; consumed via StaticDataRouteOption augmentation).
  validateSearch: observabilitySearchSchema,
  staticData: {
    title: 'Observability',
  },
  component: Observability,
})
