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
import { useEffect, useRef } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import * as z from 'zod'

import { JsonCodeEditor } from '@/components/json-code-editor'
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
import { Textarea } from '@/components/ui/textarea'

import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsControlGroup,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'
import { formatJsonForTextarea, normalizeJsonString, validateJsonString } from './utils'

const arrayOfStrings = (message: string) =>
  z.string().superRefine((value, ctx) => {
    const result = validateJsonString(value, {
      allowEmpty: true,
      predicate: Array.isArray,
      predicateMessage: message,
    })
    if (!result.valid) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: result.message || 'Invalid JSON',
      })
    }
  })

const schema = z.object({
  enabled: z.boolean(),
  target_models: arrayOfStrings('Must be a JSON array of model glob patterns'),
  models: arrayOfStrings('Must be a JSON array of model names'),
  base_url: z.string(),
  // 敏感键：后端 GetOptions 不返回现有值，留空 = 不修改（照 ionet apiKey 模式）
  api_key: z.string().optional(),
  prompt: z.string(),
  timeout_sec: z.coerce
    .number()
    .int({ message: 'Must be an integer' })
    .min(1, { message: 'Must be at least 1' }),
  sidecall_secret: z.string().optional(),
})

type VisionRelaySettingsFormValues = z.output<typeof schema>
type VisionRelaySettingsFormInput = z.input<typeof schema>

type FlatVisionRelaySettings = {
  'vision_relay.enabled': boolean
  'vision_relay.target_models': string
  'vision_relay.models': string
  'vision_relay.base_url': string
  'vision_relay.api_key': string
  'vision_relay.prompt': string
  'vision_relay.timeout_sec': number
  'vision_relay.sidecall_secret': string
}

type VisionRelaySettingsCardProps = {
  defaultValues: VisionRelaySettingsFormInput
}

export function VisionRelaySettingsCard({
  defaultValues,
}: VisionRelaySettingsCardProps) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()
  const normalizedDefaultsRef = useRef<FlatVisionRelaySettings>({
    'vision_relay.enabled': Boolean(defaultValues.enabled),
    'vision_relay.target_models': normalizeJsonString(
      defaultValues.target_models ?? ''
    ),
    'vision_relay.models': normalizeJsonString(defaultValues.models ?? ''),
    'vision_relay.base_url': defaultValues.base_url ?? '',
    'vision_relay.api_key': '',
    'vision_relay.prompt': defaultValues.prompt ?? '',
    'vision_relay.timeout_sec': Number(defaultValues.timeout_sec ?? 15),
    'vision_relay.sidecall_secret': '',
  })

  const buildFormDefaults = (
    values: VisionRelaySettingsFormInput
  ): VisionRelaySettingsFormInput => ({
    enabled: Boolean(values.enabled),
    target_models: formatJsonForTextarea(values.target_models ?? ''),
    models: formatJsonForTextarea(values.models ?? ''),
    base_url: values.base_url ?? '',
    api_key: '',
    prompt: values.prompt ?? '',
    timeout_sec: Number(values.timeout_sec ?? 15),
    sidecall_secret: '',
  })

  const form = useForm<
    VisionRelaySettingsFormInput,
    unknown,
    VisionRelaySettingsFormValues
  >({
    resolver: zodResolver(schema),
    defaultValues: buildFormDefaults(defaultValues),
  })

  useEffect(() => {
    normalizedDefaultsRef.current = {
      'vision_relay.enabled': Boolean(defaultValues.enabled),
      'vision_relay.target_models': normalizeJsonString(
        defaultValues.target_models ?? ''
      ),
      'vision_relay.models': normalizeJsonString(defaultValues.models ?? ''),
      'vision_relay.base_url': defaultValues.base_url ?? '',
      'vision_relay.api_key': '',
      'vision_relay.prompt': defaultValues.prompt ?? '',
      'vision_relay.timeout_sec': Number(defaultValues.timeout_sec ?? 15),
      'vision_relay.sidecall_secret': '',
    }
    form.reset(buildFormDefaults(defaultValues))
  }, [defaultValues, form])

  const onSubmit = async (values: VisionRelaySettingsFormValues) => {
    const normalized: FlatVisionRelaySettings = {
      'vision_relay.enabled': values.enabled,
      'vision_relay.target_models': normalizeJsonString(
        values.target_models ?? ''
      ),
      'vision_relay.models': normalizeJsonString(values.models ?? ''),
      'vision_relay.base_url': values.base_url ?? '',
      'vision_relay.api_key': (values.api_key ?? '').trim(),
      'vision_relay.prompt': values.prompt ?? '',
      'vision_relay.timeout_sec': values.timeout_sec,
      'vision_relay.sidecall_secret': (values.sidecall_secret ?? '').trim(),
    }

    const updates = (
      Object.keys(normalized) as Array<keyof FlatVisionRelaySettings>
    )
      .filter((key) => normalized[key] !== normalizedDefaultsRef.current[key])
      // enabled 最后提交：后端 enabled=true 守卫校验已存端点配置
      // （base_url/api_key/models/sidecall_secret）完整性，先写字段再开开关
      .sort((a) => (a === 'vision_relay.enabled' ? 1 : -1))

    if (updates.length === 0) {
      toast.info(t('No changes to save'))
      return
    }

    for (const key of updates) {
      await updateOption.mutateAsync({ key, value: String(normalized[key]) })
    }

    // 保存成功后同步 baseline（对齐 grok 卡收尾）：连点保存不重复提交已存键；
    // 敏感键保持空（后端不回显现有值，空=不修改）
    normalizedDefaultsRef.current = normalized
    form.reset(
      buildFormDefaults({
        enabled: normalized['vision_relay.enabled'],
        target_models: normalized['vision_relay.target_models'],
        models: normalized['vision_relay.models'],
        base_url: normalized['vision_relay.base_url'],
        api_key: '',
        prompt: normalized['vision_relay.prompt'],
        timeout_sec: normalized['vision_relay.timeout_sec'],
        sidecall_secret: '',
      })
    )
  }

  return (
    <SettingsSection title={t('Vision Relay')}>
      <Form {...form}>
        {/* eslint-disable-next-line react-hooks/refs */}
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)}>
          <SettingsPageFormActions
            onSave={form.handleSubmit(onSubmit)}
            isSaving={updateOption.isPending}
          />

          <SettingsControlGroup>
            <FormField
              control={form.control}
              name='enabled'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Vision Relay Enabled')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Intercept image blocks in requests to pure-text models, describe them via a vision model, and forward the description as text.'
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
          </SettingsControlGroup>

          <FormField
            control={form.control}
            name='target_models'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Target Models')}</FormLabel>
                <FormControl>
                  <JsonCodeEditor
                    value={field.value}
                    onChange={field.onChange}
                    name={field.name}
                    onBlur={field.onBlur}
                    textareaRef={field.ref}
                    aria-invalid={Boolean(
                      form.formState.errors.target_models
                    )}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Model glob allowlist as a JSON array (e.g. ["deepseek*"]). Only requests to matching models are processed. Empty = no model is processed.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='models'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Vision Models')}</FormLabel>
                <FormControl>
                  <JsonCodeEditor
                    value={field.value}
                    onChange={field.onChange}
                    name={field.name}
                    onBlur={field.onBlur}
                    textareaRef={field.ref}
                    aria-invalid={Boolean(form.formState.errors.models)}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Vision model fallback chain as a JSON array (e.g. ["gemma-4-31b", "step-3.7-flash"]), tried in order.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='base_url'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Vision Base URL')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    value={String(field.value ?? '')}
                    onChange={(event) => field.onChange(event.target.value)}
                    placeholder='https://vision.example.com'
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'OpenAI-compatible vision endpoint. Use this instance internal address for self-loop (e.g. http://127.0.0.1:3000).'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='api_key'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Vision API Key')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    type='password'
                    value={String(field.value ?? '')}
                    onChange={(event) => field.onChange(event.target.value)}
                    placeholder={t('Leave empty to keep the existing value')}
                    autoComplete='new-password'
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Credentials for the vision endpoint. The current value is not displayed; leave empty to keep it.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='sidecall_secret'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Sidecall Secret')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    type='password'
                    value={String(field.value ?? '')}
                    onChange={(event) => field.onChange(event.target.value)}
                    placeholder={t('Leave empty to keep the existing value')}
                    autoComplete='new-password'
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Shared secret for recursion protection (HMAC marker). Required for self-loop mode; the current value is not displayed.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='prompt'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Vision Prompt')}</FormLabel>
                <FormControl>
                  <Textarea
                    {...field}
                    value={String(field.value ?? '')}
                    onChange={(event) => field.onChange(event.target.value)}
                    placeholder={t('Leave empty to use the default template')}
                    rows={3}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Instruction template for image description. Leave empty to use the default.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='timeout_sec'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Timeout (Seconds)')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    value={String(field.value ?? '')}
                    onChange={(event) => field.onChange(event.target.value)}
                    placeholder='15'
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Total time budget per request in seconds. Must be a positive integer.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
