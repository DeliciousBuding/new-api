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
 * Session Detail tab (T4.1 placeholder). T4.3 fills this with the session
 * summary, its keyset-paginated turns, and the canonical turn context
 * (GET /api/relay-observer/sessions/:id, .../turns, /turns/:id/context).
 */
import { useTranslation } from 'react-i18next'

import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '@/components/ui/empty'

export function SessionDetailTab() {
  const { t } = useTranslation()

  return (
    <Empty>
      <EmptyHeader>
        <EmptyTitle>{t('Session Detail')}</EmptyTitle>
        <EmptyDescription>
          {t('This section will be implemented in an upcoming update.')}
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  )
}
