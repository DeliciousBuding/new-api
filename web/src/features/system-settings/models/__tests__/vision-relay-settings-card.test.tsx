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
import assert from 'node:assert/strict'
import { after, describe, test } from 'node:test'

import { Window } from 'happy-dom'

const domWindow = new Window()
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLInputElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'CustomEvent',
  'MutationObserver',
  'requestAnimationFrame',
  'cancelAnimationFrame',
  'getComputedStyle',
] as const

for (const key of domGlobals) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { QueryClient, QueryClientProvider } =
  await import('@tanstack/react-query')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { VisionRelaySettingsCard } = await import('../vision-relay-settings-card')
const { SettingsPageProvider } = await import(
  '../../components/settings-page-context'
)

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        'Must be a JSON array of model glob patterns':
          'Must be a JSON array of model glob patterns',
        'Must be a JSON array of model names':
          'Must be a JSON array of model names',
        'Must be at least 1': 'Must be at least 1',
      },
    },
  },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

function changeInputValue(input: HTMLInputElement, value: string) {
  const valueSetter = Object.getOwnPropertyDescriptor(
    domWindow.HTMLInputElement.prototype,
    'value'
  )?.set
  assert.ok(valueSetter)
  valueSetter.call(input, value)
  input.dispatchEvent(
    new domWindow.Event('input', { bubbles: true }) as unknown as Event
  )
}

function changeTextareaValue(textarea: HTMLTextAreaElement, value: string) {
  const valueSetter = Object.getOwnPropertyDescriptor(
    domWindow.HTMLTextAreaElement.prototype,
    'value'
  )?.set
  assert.ok(valueSetter)
  valueSetter.call(textarea, value)
  textarea.dispatchEvent(
    new domWindow.Event('input', { bubbles: true }) as unknown as Event
  )
}

function defaultValues() {
  return {
    enabled: false,
    target_models: '[]',
    models: '["gemma-4-31b"]',
    base_url: 'http://127.0.0.1:3000',
    api_key: '',
    prompt: '',
    timeout_sec: 15,
    sidecall_secret: '',
  }
}

describe('vision relay settings card', () => {
  after(() => {
    domWindow.close()
  })

  async function renderCard(values: ReturnType<typeof defaultValues>) {
    const container = document.createElement('div')
    document.body.append(container)
    const actionsContainer = document.createElement('div')
    document.body.append(actionsContainer)
    const root = createRoot(container)
    const queryClient = new QueryClient({
      defaultOptions: { mutations: { retry: false } },
    })

    await act(async () => {
      root.render(
        <QueryClientProvider client={queryClient}>
          <I18nextProvider i18n={i18n}>
            <SettingsPageProvider
              actionsContainer={actionsContainer}
              titleStatusContainer={null}
            >
              <VisionRelaySettingsCard defaultValues={values} />
            </SettingsPageProvider>
          </I18nextProvider>
        </QueryClientProvider>
      )
    })
    return { container, actionsContainer, root, queryClient }
  }

  test('renders switch, JSON editors and sensitive inputs', async () => {
    const { container, actionsContainer, root, queryClient } =
      await renderCard(defaultValues())

    const enabledSwitch = container.querySelector<HTMLElement>(
      'span[role="switch"]'
    )
    assert.ok(enabledSwitch, 'enabled switch must render')

    const textareas = container.querySelectorAll<HTMLTextAreaElement>(
      'textarea'
    )
    assert.ok(textareas.length >= 2, 'target_models/models JSON editors render')

    const passwordInputs = container.querySelectorAll<HTMLInputElement>(
      'input[type="password"]'
    )
    assert.equal(passwordInputs.length, 2, 'api_key + sidecall_secret inputs')

    const saveButton = [...actionsContainer.querySelectorAll('button')].find(
      (button) => button.textContent === 'Save Changes'
    )
    assert.ok(saveButton, 'Save Changes action button must render')

    await act(async () => root.unmount())
    container.remove()
    actionsContainer.remove()
    queryClient.clear()
  })

  test('blocks malformed target_models JSON on save', async () => {
    const { container, actionsContainer, root, queryClient } =
      await renderCard(defaultValues())

    const editor = container.querySelector<HTMLTextAreaElement>('textarea')
    assert.ok(editor)

    await act(async () => {
      changeTextareaValue(editor, '{}')
    })

    const saveButton = [...actionsContainer.querySelectorAll('button')].find(
      (button) => button.textContent === 'Save Changes'
    )
    assert.ok(saveButton)

    await act(async () => {
      saveButton.click()
    })

    const alert = container.querySelector('p[data-slot="form-message"]')
    assert.ok(alert, 'validation error must be shown')
    assert.equal(
      alert.textContent,
      'Must be a JSON array of model glob patterns'
    )

    await act(async () => root.unmount())
    container.remove()
    actionsContainer.remove()
    queryClient.clear()
  })

  test('rejects non-positive timeout on save', async () => {
    const { container, actionsContainer, root, queryClient } =
      await renderCard(defaultValues())

    const timeoutInput = container.querySelector<HTMLInputElement>(
      'input[placeholder="15"]'
    )
    assert.ok(timeoutInput)

    await act(async () => {
      changeInputValue(timeoutInput, '0')
    })

    const saveButton = [...actionsContainer.querySelectorAll('button')].find(
      (button) => button.textContent === 'Save Changes'
    )
    assert.ok(saveButton)

    await act(async () => {
      saveButton.click()
    })

    const alert = container.querySelector('p[data-slot="form-message"]')
    assert.ok(alert, 'validation error must be shown')
    assert.equal(alert.textContent, 'Must be at least 1')

    await act(async () => root.unmount())
    container.remove()
    actionsContainer.remove()
    queryClient.clear()
  })
})
