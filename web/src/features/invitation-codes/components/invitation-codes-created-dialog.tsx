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
import { TicketPlus } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'

import { useInvitationCodes } from './invitation-codes-provider'

export function InvitationCodesCreatedDialog() {
  const { t } = useTranslation()
  const { createdCodes, setCreatedCodes } = useInvitationCodes()

  const handleOpenChange = (open: boolean) => {
    if (!open) {
      setCreatedCodes([])
    }
  }

  if (!createdCodes.length) return null

  return (
    <Dialog
      open
      onOpenChange={handleOpenChange}
      title={
        <>
          <TicketPlus className='h-5 w-5' />
          {t('Invitation Codes Created')}
        </>
      }
      description={t(
        'The following invitation codes were created successfully.'
      )}
      contentClassName='sm:max-w-md'
      titleClassName='flex items-center gap-2'
      contentHeight='auto'
      bodyClassName='space-y-4'
      footer={
        <Button onClick={() => handleOpenChange(false)}>{t('Done')}</Button>
      }
    >
      <div className='space-y-4 py-4'>
        <div className='rounded-lg border p-4'>
          <div className='grid grid-cols-2 gap-2'>
            {createdCodes.map((code) => (
              <div
                key={code}
                className='bg-muted rounded-md p-2 text-center font-mono text-sm'
              >
                {code}
              </div>
            ))}
          </div>
        </div>

        <CopyButton
          value={createdCodes.join('\n')}
          variant='outline'
          size='default'
          className='w-full'
          iconClassName='mr-2 size-4'
          tooltip={t('Copy all invitation codes')}
          aria-label={t('Copy all invitation codes')}
        >
          {t('Copy All Codes')}
        </CopyButton>
      </div>
    </Dialog>
  )
}
