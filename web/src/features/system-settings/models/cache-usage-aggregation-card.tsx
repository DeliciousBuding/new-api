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
import { useQuery } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import * as z from 'zod'

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
import { Switch } from '@/components/ui/switch'

import { getCacheUsageAggregationStatus, updateSystemOption } from '../api'
import {
  SettingsControlGroup,
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'

const schema = z.object({
  enabled: z.boolean(),
  interval_minutes: z.coerce
    .number()
    .int({ message: 'Must be an integer' })
    .min(5, { message: 'Must be at least 5' })
    .max(60, { message: 'Must be at most 60' }),
})

type CacheUsageAggregationFormValues = z.output<typeof schema>
type CacheUsageAggregationFormInput = z.input<typeof schema>

type CacheUsageAggregationCardProps = {
  defaultValues: CacheUsageAggregationFormInput
}

export function CacheUsageAggregationCard({
  defaultValues,
}: CacheUsageAggregationCardProps) {
  const { t } = useTranslation()

  const form = useForm<
    CacheUsageAggregationFormInput,
    unknown,
    CacheUsageAggregationFormValues
  >({
    resolver: zodResolver(schema),
    defaultValues: {
      enabled: Boolean(defaultValues.enabled),
      interval_minutes: Number(defaultValues.interval_minutes ?? 15),
    },
  })

  const statusQuery = useQuery({
    queryKey: ['cache-usage-aggregation-status'],
    queryFn: getCacheUsageAggregationStatus,
    refetchInterval: 30_000,
  })

  const onSubmit = async (values: CacheUsageAggregationFormValues) => {
    const normalized = {
      'cache_usage_aggregation.enabled': values.enabled,
      'cache_usage_aggregation.interval_minutes': values.interval_minutes,
    }
    const defaults = {
      'cache_usage_aggregation.enabled': Boolean(defaultValues.enabled),
      'cache_usage_aggregation.interval_minutes': Number(
        defaultValues.interval_minutes ?? 15
      ),
    }

    const hasChanges = (
      Object.keys(normalized) as Array<keyof typeof normalized>
    ).some((key) => normalized[key] !== defaults[key])

    if (!hasChanges) {
      toast.info(t('No changes to save'))
      return
    }

    for (const [key, value] of Object.entries(normalized)) {
      const result = await updateSystemOption({ key, value })
      if (!result.success) {
        toast.error(result.message || t('Failed to save settings'))
        return
      }
    }
    toast.success(t('Settings saved successfully'))
    statusQuery.refetch()
  }

  const status = statusQuery.data?.data

  return (
    <SettingsSection title={t('Cache Usage Aggregation')}>
      <Form {...form}>
        {/* eslint-disable-next-line react-hooks/refs */}
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)}>
          <SettingsPageFormActions onSave={form.handleSubmit(onSubmit)} />

          <SettingsControlGroup>
            <FormField
              control={form.control}
              name='enabled'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>
                      {t('Cache Usage Aggregation Enabled')}
                    </FormLabel>
                    <FormDescription>
                      {t(
                        'Aggregate cache usage from logs into an hourly table so the Keys page and dashboard trend load in under a second instead of running slow scans over the logs table.'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                </SettingsSwitchItem>
              )}
            />

            <FormField
              control={form.control}
              name='interval_minutes'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>
                    {t('Aggregation Interval (minutes)')}
                  </FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={5}
                      max={60}
                      {...field}
                      value={String(field.value ?? '')}
                      onChange={(event) => field.onChange(event.target.value)}
                    />
                  </FormControl>
                  <FormDescription>
                    {t('Between 5 and 60 minutes. Default is 15.')}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
          </SettingsControlGroup>
        </SettingsForm>
      </Form>

      <CacheUsageAggregationStatusBlock
        status={status}
        isLoading={statusQuery.isLoading}
      />
    </SettingsSection>
  )
}

type CacheUsageAggregationStatusBlockProps = {
  status:
    | {
        covered_from_hour: number
        ready_hour: number
        last_run_at: number
        latest_task: {
          status: 'pending' | 'running' | 'succeeded' | 'failed'
          error: string
          result: string
        } | null
      }
    | undefined
  isLoading: boolean
}

function CacheUsageAggregationStatusBlock({
  status,
  isLoading,
}: CacheUsageAggregationStatusBlockProps) {
  const { t } = useTranslation()

  if (isLoading || !status) {
    return null
  }

  const formatHour = (hour: number): string => {
    if (hour <= 0) {
      return t('Never')
    }
    return new Date(hour * 3600 * 1000).toLocaleString()
  }

  const taskStatusLabel = (status?.latest_task?.status ?? '').toUpperCase()

  return (
    <div className='space-y-1 text-sm'>
      <p>
        {t('Covered From')}: {formatHour(status.covered_from_hour)}
      </p>
      <p>
        {t('Ready Through')}: {formatHour(status.ready_hour)}
      </p>
      <p>
        {t('Last Run')}:{' '}
        {status.last_run_at > 0
          ? new Date(status.last_run_at * 1000).toLocaleString()
          : t('Never')}
      </p>
      <p>
        {t('Latest Task')}:{' '}
        {status.latest_task ? taskStatusLabel : t('No task yet')}
      </p>
    </div>
  )
}
