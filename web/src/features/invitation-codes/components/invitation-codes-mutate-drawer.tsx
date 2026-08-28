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
import { zodResolver } from '@hookform/resolvers/zod'
import { type FormEvent, useEffect, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { DateTimePicker } from '@/components/datetime-picker'
import {
  SideDrawerSection,
  sideDrawerContentClassName,
  sideDrawerFooterClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Sheet,
  SheetClose,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { handleServerError } from '@/lib/handle-server-error'
import { addTimeToDate } from '@/lib/time'

import {
  createInvitationCode,
  updateInvitationCode,
  getInvitationCode,
} from '../api'
import { SUCCESS_MESSAGES } from '../constants'
import {
  getInvitationFormSchema,
  type InvitationFormValues,
  INVITATION_FORM_DEFAULT_VALUES,
  transformFormDataToPayload,
  transformInvitationCodeToFormDefaults,
} from '../lib'
import type { InvitationCode } from '../types'
import { useInvitationCodes } from './invitation-codes-provider'

type InvitationCodesMutateDrawerProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  currentRow?: InvitationCode
}

export function InvitationCodesMutateDrawer({
  open,
  onOpenChange,
  currentRow,
}: InvitationCodesMutateDrawerProps) {
  const { t } = useTranslation()
  const isUpdate = !!currentRow
  const invitationCodeId = currentRow?.id
  const { triggerRefresh, setCreatedCodes } = useInvitationCodes()
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [invitationLoadState, setInvitationLoadState] = useState<
    'idle' | 'loading' | 'ready' | 'error'
  >('idle')
  const [loadedInvitationCode, setLoadedInvitationCode] =
    useState<InvitationCode | null>(null)

  const form = useForm<InvitationFormValues>({
    resolver: zodResolver(getInvitationFormSchema(t)),
    defaultValues: INVITATION_FORM_DEFAULT_VALUES,
  })

  // Load existing data when updating
  useEffect(() => {
    if (!open) {
      setInvitationLoadState('idle')
      setLoadedInvitationCode(null)
      return
    }

    if (!isUpdate || invitationCodeId === undefined) {
      form.reset(INVITATION_FORM_DEFAULT_VALUES)
      setInvitationLoadState('ready')
      setLoadedInvitationCode(null)
      return
    }

    let ignoreResult = false

    form.reset(INVITATION_FORM_DEFAULT_VALUES)
    setInvitationLoadState('loading')
    setLoadedInvitationCode(null)

    void getInvitationCode(invitationCodeId)
      .then((result) => {
        if (ignoreResult) return

        if (
          !result.success ||
          !result.data ||
          result.data.id !== invitationCodeId
        ) {
          setInvitationLoadState('error')
          toast.error(t('Failed to load'))
          return
        }

        form.reset(transformInvitationCodeToFormDefaults(result.data))
        setLoadedInvitationCode(result.data)
        setInvitationLoadState('ready')
      })
      .catch((error: unknown) => {
        if (ignoreResult) return

        setInvitationLoadState('error')
        handleServerError(error)
      })

    return () => {
      ignoreResult = true
    }
  }, [open, isUpdate, invitationCodeId, form, t])

  const isUpdateReady =
    !isUpdate ||
    (invitationLoadState === 'ready' &&
      loadedInvitationCode?.id === invitationCodeId)
  const isLoadingInvitationCode = invitationLoadState === 'loading'

  const onSubmit = async (data: InvitationFormValues) => {
    if (isUpdate && (!currentRow || !loadedInvitationCode || !isUpdateReady)) {
      return
    }

    setIsSubmitting(true)
    try {
      const payload = transformFormDataToPayload(data)

      if (isUpdate && currentRow && loadedInvitationCode) {
        const result = await updateInvitationCode({
          ...payload,
          id: currentRow.id,
        })
        if (result.success) {
          toast.success(t(SUCCESS_MESSAGES.INVITATION_UPDATED))
          onOpenChange(false)
          triggerRefresh()
        }
      } else {
        // Create mode
        const result = await createInvitationCode(payload)
        if (result.success) {
          const codes = result.data || []
          const count = codes.length
          toast.success(
            count > 1
              ? t('Successfully created {{count}} invitation codes', {
                  count,
                })
              : t(SUCCESS_MESSAGES.INVITATION_CREATED)
          )
          setCreatedCodes(codes)
          onOpenChange(false)
          triggerRefresh()
        }
      }
    } finally {
      setIsSubmitting(false)
    }
  }

  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    void form.handleSubmit(onSubmit)(event)
  }

  const handleSetExpiry = (months: number, days: number, hours: number) => {
    const newDate = addTimeToDate(months, days, hours)
    form.setValue('expired_time', newDate)
  }

  let submitButtonLabel = t('Save changes')
  if (isLoadingInvitationCode) {
    submitButtonLabel = t('Loading...')
  } else if (isSubmitting) {
    submitButtonLabel = t('Saving...')
  }

  return (
    <Sheet
      open={open}
      onOpenChange={(v) => {
        onOpenChange(v)
        if (!v) {
          form.reset()
        }
      }}
    >
      <SheetContent className={sideDrawerContentClassName('sm:max-w-[600px]')}>
        <SheetHeader className={sideDrawerHeaderClassName()}>
          <SheetTitle>
            {isUpdate
              ? t('Update Invitation Code')
              : t('Create Invitation Code')}
          </SheetTitle>
          <SheetDescription>
            {isUpdate
              ? t('Update the invitation code by providing necessary info.')
              : t(
                  'Add new invitation code(s) by providing necessary info.'
                )}{' '}
            {t('Click save when you&apos;re done.')}
          </SheetDescription>
        </SheetHeader>
        <Form {...form}>
          <form
            id='invitation-form'
            onSubmit={handleSubmit}
            className={sideDrawerFormClassName()}
            aria-busy={isLoadingInvitationCode}
          >
            <fieldset
              disabled={!isUpdateReady || isSubmitting}
              className='contents'
            >
              <SideDrawerSection>
                <FormField
                  control={form.control}
                  name='name'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Name')}</FormLabel>
                      <FormControl>
                        <Input {...field} placeholder={t('Enter a name')} />
                      </FormControl>
                      <FormDescription>
                        {t('Name for this invitation code (1-20 characters)')}
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />

                <FormField
                  control={form.control}
                  name='max_uses'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Max Uses')}</FormLabel>
                      <FormControl>
                        <Input
                          {...field}
                          type='number'
                          min='1'
                          step='1'
                          placeholder={t('Max Uses')}
                          value={
                            Number.isFinite(field.value) ? field.value : ''
                          }
                          onChange={(e) =>
                            field.onChange(
                              Number.parseInt(e.target.value, 10) || 0
                            )
                          }
                        />
                      </FormControl>
                      <FormDescription>
                        {t('Maximum number of times this code can be used')}
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />

                <FormField
                  control={form.control}
                  name='expired_time'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Expiration Time')}</FormLabel>
                      <div className='flex flex-col gap-2'>
                        <FormControl>
                          <DateTimePicker
                            value={field.value}
                            onChange={field.onChange}
                            placeholder={t('Never expires')}
                          />
                        </FormControl>
                        <div className='grid grid-cols-4 gap-1.5 sm:flex sm:gap-2'>
                          <Button
                            type='button'
                            variant='outline'
                            size='sm'
                            onClick={() => handleSetExpiry(0, 0, 0)}
                          >
                            {t('Never')}
                          </Button>
                          <Button
                            type='button'
                            variant='outline'
                            size='sm'
                            onClick={() => handleSetExpiry(1, 0, 0)}
                          >
                            {t('1M')}
                          </Button>
                          <Button
                            type='button'
                            variant='outline'
                            size='sm'
                            onClick={() => handleSetExpiry(0, 7, 0)}
                          >
                            {t('1W')}
                          </Button>
                          <Button
                            type='button'
                            variant='outline'
                            size='sm'
                            onClick={() => handleSetExpiry(0, 1, 0)}
                          >
                            {t('1 Day')}
                          </Button>
                        </div>
                      </div>
                      <FormDescription>
                        {t('Leave empty for never expires')}
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />

                {!isUpdate && (
                  <FormField
                    control={form.control}
                    name='count'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{t('Quantity')}</FormLabel>
                        <FormControl>
                          <Input
                            {...field}
                            type='number'
                            min='1'
                            max='100'
                            placeholder={t('Number of codes to create')}
                            onChange={(e) =>
                              field.onChange(
                                Number.parseInt(e.target.value, 10) || 1
                              )
                            }
                          />
                        </FormControl>
                        <FormDescription>
                          {t(
                            'Create multiple invitation codes at once (1-100)'
                          )}
                        </FormDescription>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                )}
              </SideDrawerSection>
            </fieldset>
          </form>
        </Form>
        <SheetFooter className={sideDrawerFooterClassName()}>
          <SheetClose render={<Button variant='outline' />}>
            {t('Close')}
          </SheetClose>
          <Button
            form='invitation-form'
            type='submit'
            disabled={isSubmitting || !isUpdateReady}
          >
            {submitButtonLabel}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}
