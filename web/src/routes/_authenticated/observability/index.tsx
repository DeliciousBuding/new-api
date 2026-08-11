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
import { createFileRoute } from '@tanstack/react-router'
import { z } from 'zod'

import { ObservabilitySessionsPage } from '@/features/observability/pages/sessions-page'

/**
 * Observability workspace — single unified page: master-detail sessions view
 * (left session log list + right chat transcript). The `?session=<id>` URL
 * state feeds the detail pane: clicking a session row writes the param, the
 * detail pane reads it. Same pattern as usage-logs/$section.tsx
 * validateSearch.
 */
export const observabilitySessionsSearchSchema = z.object({
  session: z.string().optional(),
})

export const Route = createFileRoute('/_authenticated/observability/')({
  validateSearch: observabilitySessionsSearchSchema,
  component: ObservabilitySessionsPage,
})
