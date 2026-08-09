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
/**
 * Root-only relay observer workspace (redesigned).
 *
 * Two tabs instead of three: Overview (aggregate windows + StatCards) and
 * Sessions. The old "Session Detail" tab is gone — the Sessions tab now hosts
 * a ResizablePanelGroup master/detail (list left, selected session right), so
 * selecting a row reveals its detail in place instead of forcing a tab jump.
 *
 * The `?session=<id>` URL param is preserved: the Sessions tab is still
 * controlled by route search, so deep links and back/forward keep working.
 *
 * react-resizable-panels is a project dependency with a shadcn wrapper
 * (@/components/ui/resizable) but had no consumers until now — this is the
 * first native use, following the wrapper-component convention.
 */
import { getRouteApi, useNavigate } from '@tanstack/react-router'
import { CheckCircle2, AlertTriangle } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from '@/components/ui/resizable'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

import { getStatus } from './api'
import { OverviewTab } from './pages/overview-tab'
import { SessionDetailTab } from './pages/session-detail-tab'
import { SessionsTab } from './pages/sessions-tab'
import { observabilityQueryKeys } from './query-keys'
import { isObserverDegraded, type ObserverStatus } from './types'

const route = getRouteApi('/_authenticated/observability')

/** Compact live-health badge for the title bar: green dot when enabled and
 * circuit closed, amber when circuit open, red when disabled. */
function HealthBadge() {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: observabilityQueryKeys.status(),
    queryFn: getStatus,
    retry: false,
    // The badge is best-effort; the Overview tab owns the authoritative
    // status view. A stale/empty fetch just hides the dot.
    staleTime: 15_000,
  })
  const data = query.data?.data
  if (!data || isObserverDegraded(data)) return null
  const status = data as ObserverStatus
  const healthy = status.Enabled && !status.CircuitOpen
  return (
    <Badge variant={healthy ? 'default' : 'destructive'} className='gap-1.5'>
      {healthy ? (
        <CheckCircle2 className='size-3' />
      ) : (
        <AlertTriangle className='size-3' />
      )}
      {healthy ? t('Healthy') : t('Degraded')}
    </Badge>
  )
}

export function Observability() {
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
    <SectionPageLayout>
      <SectionPageLayout.Title>
        <span className='inline-flex min-w-0 items-center gap-2'>
          <span className='truncate'>{t('Observability')}</span>
          <Badge variant='outline' className='shrink-0'>
            Root
          </Badge>
          <HealthBadge />
        </span>
      </SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <Tabs defaultValue='overview'>
          <TabsList>
            <TabsTrigger value='overview'>{t('Overview')}</TabsTrigger>
            <TabsTrigger value='sessions'>{t('Sessions')}</TabsTrigger>
          </TabsList>
          <TabsContent value='overview'>
            <OverviewTab />
          </TabsContent>
          <TabsContent value='sessions'>
            <ResizablePanelGroup
              orientation='horizontal'
              className='min-h-[600px] rounded-lg border'
            >
              <ResizablePanel defaultSize={42} minSize={28}>
                <SessionsTab
                  selectedSessionId={sessionId}
                  onSelectSession={handleSelectSession}
                />
              </ResizablePanel>
              <ResizableHandle withHandle />
              <ResizablePanel defaultSize={58} minSize={32}>
                <SessionDetailTab sessionId={sessionId} />
              </ResizablePanel>
            </ResizablePanelGroup>
          </TabsContent>
        </Tabs>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
