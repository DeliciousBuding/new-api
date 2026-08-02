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
 * Root-only relay observer workspace (T4.1 skeleton).
 *
 * Three tabs, one per T4.2/T4.3 work item: Overview (aggregate windows),
 * Sessions (keyset-paginated session list), and Session Detail (one
 * session's turns + canonical context). The guard lives in the route
 * (_authenticated/observability/route.tsx); this component renders only for
 * ROLE.SUPER_ADMIN.
 */
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

import { OverviewTab } from './pages/overview-tab'
import { SessionsTab } from './pages/sessions-tab'
import { SessionDetailTab } from './pages/session-detail-tab'

export function Observability() {
  const { t } = useTranslation()

  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>
        <span className='inline-flex min-w-0 items-center gap-2'>
          <span className='truncate'>{t('Observability')}</span>
          <Badge variant='outline' className='shrink-0'>
            Root
          </Badge>
        </span>
      </SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <Tabs defaultValue='overview'>
          <TabsList>
            <TabsTrigger value='overview'>{t('Overview')}</TabsTrigger>
            <TabsTrigger value='sessions'>{t('Sessions')}</TabsTrigger>
            <TabsTrigger value='session-detail'>
              {t('Session Detail')}
            </TabsTrigger>
          </TabsList>
          <TabsContent value='overview'>
            <OverviewTab />
          </TabsContent>
          <TabsContent value='sessions'>
            <SessionsTab />
          </TabsContent>
          <TabsContent value='session-detail'>
            <SessionDetailTab />
          </TabsContent>
        </Tabs>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
