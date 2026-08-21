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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { beforeAll, describe, expect, test } from 'vitest'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { VisionRelaySettingsCard } from '../vision-relay-settings-card'

beforeAll(() => {
  i18next.addResourceBundle('en', 'translation', {
    'Must be a JSON array of model glob patterns':
      'Must be a JSON array of model glob patterns',
    'Must be a JSON array of model names':
      'Must be a JSON array of model names',
    'Must be at least 1': 'Must be at least 1',
  })
})

function defaultValues() {
  return {
    enabled: false,
    structured: false,
    structured_prompt: '',
    target_models: '[]',
    models: '["gemma-4-31b"]',
    base_url: 'http://127.0.0.1:3000',
    api_key: '',
    prompt: '',
    timeout_sec: 15,
    sidecall_secret: '',
    disable_proxy_fetch: false,
    cache_ttl_sec: 86400,
    max_images: 20,
    request_concurrency: 4,
    max_description_bytes: 8000,
    max_total_bytes: 48000,
    default_max_tokens: 2000,
    max_fallback_models: 3,
  }
}

describe('vision relay settings card', () => {
  async function renderCard(values: ReturnType<typeof defaultValues>) {
    const actionsContainer = document.createElement('div')
    document.body.append(actionsContainer)
    const queryClient = new QueryClient({
      defaultOptions: { mutations: { retry: false } },
    })
    const rendered = render(
      <QueryClientProvider client={queryClient}>
        <SettingsPageProvider
          actionsContainer={actionsContainer}
          titleStatusContainer={null}
        >
          <VisionRelaySettingsCard defaultValues={values} />
        </SettingsPageProvider>
      </QueryClientProvider>
    )
    return { actionsContainer, queryClient, ...rendered }
  }

  function findSaveButton(actionsContainer: HTMLElement): HTMLButtonElement {
    const button = [...actionsContainer.querySelectorAll('button')].find(
      (candidate) => candidate.textContent === 'Save Changes'
    )
    expect(button, 'Save Changes action button must render').toBeTruthy()
    return button as HTMLButtonElement
  }

  test('renders switches, JSON editors and sensitive inputs', async () => {
    const { container, actionsContainer, queryClient, unmount } =
      await renderCard(defaultValues())

    const switches =
      container.querySelectorAll<HTMLElement>('span[role="switch"]')
    expect(switches.length).toBe(3)

    const textareas =
      container.querySelectorAll<HTMLTextAreaElement>('textarea')
    expect(textareas.length).toBeGreaterThanOrEqual(2)

    const passwordInputs = container.querySelectorAll<HTMLInputElement>(
      'input[type="password"]'
    )
    expect(passwordInputs.length).toBe(2)

    expect(
      [...actionsContainer.querySelectorAll('button')].some(
        (button) => button.textContent === 'Save Changes'
      )
    ).toBe(true)

    unmount()
    actionsContainer.remove()
    queryClient.clear()
  })

  test('blocks malformed target_models JSON on save', async () => {
    const { container, actionsContainer, queryClient, unmount } =
      await renderCard(defaultValues())

    const editor = container.querySelector<HTMLTextAreaElement>('textarea')
    expect(editor).toBeTruthy()

    fireEvent.input(editor as HTMLTextAreaElement, {
      target: { value: '{}' },
    })

    const saveButton = findSaveButton(actionsContainer)
    const user = userEvent.setup()
    await user.click(saveButton)

    const alert = await waitFor(() =>
      container.querySelector('p[data-slot="form-message"]')
    )
    expect(alert).not.toBeNull()
    if (!alert) throw new Error('validation message must render')
    expect(alert.textContent).toBe(
      'Must be a JSON array of model glob patterns'
    )

    unmount()
    actionsContainer.remove()
    queryClient.clear()
  })

  test('rejects non-positive timeout on save', async () => {
    const { container, actionsContainer, queryClient, unmount } =
      await renderCard(defaultValues())

    const timeoutInput = container.querySelector<HTMLInputElement>(
      'input[placeholder="15"]'
    )
    expect(timeoutInput).toBeTruthy()

    fireEvent.input(timeoutInput as HTMLInputElement, {
      target: { value: '0' },
    })

    const saveButton = findSaveButton(actionsContainer)
    const user = userEvent.setup()
    await user.click(saveButton)

    const alert = await waitFor(() =>
      container.querySelector('p[data-slot="form-message"]')
    )
    expect(alert).not.toBeNull()
    if (!alert) throw new Error('validation message must render')
    expect(alert.textContent).toBe('Must be at least 1')

    unmount()
    actionsContainer.remove()
    queryClient.clear()
  })
})
