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
 * Observability workspace — single unified master-detail page.
 *
 * The session list (master, left) and the selected session's detail (right)
 * share one page: clicking a session row reveals its full multi-turn
 * context inline — no tab switching. The selection lives in the
 * `?session=<id>` URL param (deep-linkable, survives reload).
 */
import { getRouteApi, useNavigate } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout'

import { SessionDetailTab } from './session-detail-tab'
import { SessionsTab } from './sessions-tab'

const route = getRouteApi('/_authenticated/observability/')

export function ObservabilitySessionsPage() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { session } = route.useSearch()
  const sessionId = session ?? null

  const handleSelectSession = (next: string | null) => {
    navigate({
      to: '/observability',
      search: { session: next ?? undefined },
    })
  }

  return (
    <SectionPageLayout fixedContent>
      <SectionPageLayout.Title>{t('Sessions')}</SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <div className='grid h-full min-h-0 gap-4 xl:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]'>
          <div className='min-h-0 overflow-auto xl:max-h-full'>
            <SessionsTab
              selectedSessionId={sessionId}
              onSelectSession={handleSelectSession}
            />
          </div>
          <div className='min-h-0 overflow-auto xl:max-h-full'>
            <SessionDetailTab sessionId={sessionId} />
          </div>
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
