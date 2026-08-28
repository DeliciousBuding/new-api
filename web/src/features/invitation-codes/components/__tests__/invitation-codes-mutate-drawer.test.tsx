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
import {
  fireEvent,
  render,
  screen,
  waitFor,
  type RenderResult,
} from '@testing-library/react'
import { afterEach, describe, expect, test } from 'vitest'

import type { InvitationCode } from '../../types'

const i18n = (await import('i18next')).default
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { Toaster, toast } = await import('sonner')
const { api } = await import('@/lib/api')
const { InvitationCodesProvider } = await import('../invitation-codes-provider')
const { InvitationCodesMutateDrawer } =
  await import('../invitation-codes-mutate-drawer')

await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        'Failed to load': 'Failed to load',
        'Loading...': 'Loading...',
        'Save changes': 'Save changes',
        'Something went wrong!': 'Something went wrong!',
      },
    },
  },
})

type ApiMethod = (url: string, data?: unknown) => Promise<{ data: unknown }>
type MockableApi = {
  get: ApiMethod
  put: ApiMethod
}
type RenderedDrawer = {
  result: RenderResult
}

const apiClient = api as unknown as MockableApi
const originalGet = apiClient.get
const originalPut = apiClient.put
const originalConsoleLog = Reflect.get(console, 'log')
let renderedDrawer: RenderedDrawer | null = null

function invitationCode(id: number, maxUses = 5): InvitationCode {
  return {
    id,
    code: `AB${String(id).padStart(4, '0')}`,
    status: 1,
    name: `invite-${id}`,
    max_uses: maxUses,
    used_count: 0,
    created_time: 1,
    expired_time: 0,
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<T>((promiseResolve, promiseReject) => {
    resolve = promiseResolve
    reject = promiseReject
  })
  return { promise, reject, resolve }
}

function drawerTree(currentRow: InvitationCode) {
  return (
    <I18nextProvider i18n={i18n}>
      <InvitationCodesProvider>
        <InvitationCodesMutateDrawer
          open
          currentRow={currentRow}
          onOpenChange={() => undefined}
        />
      </InvitationCodesProvider>
      <Toaster duration={60_000} />
    </I18nextProvider>
  )
}

async function renderDrawer(currentRow: InvitationCode): Promise<void> {
  renderedDrawer = { result: render(drawerTree(currentRow)) }
}

async function rerenderDrawer(currentRow: InvitationCode): Promise<void> {
  if (!renderedDrawer) {
    throw new Error('Expected a rendered invitation code drawer')
  }
  renderedDrawer.result.rerender(drawerTree(currentRow))
}

function getSaveButton(): HTMLButtonElement {
  return screen.getByRole('button', { name: 'Save changes' })
}

function getControlByLabel(labelText: 'Name'): HTMLInputElement
function getControlByLabel(labelText: 'Max Uses'): HTMLInputElement
function getControlByLabel(labelText: string): HTMLElement {
  const label = [...document.querySelectorAll<HTMLLabelElement>('label')].find(
    (candidate) => candidate.textContent?.trim() === labelText
  )
  if (!label) {
    throw new Error(`Expected label "${labelText}"`)
  }
  const control =
    label.control ??
    label
      .closest('[data-slot="form-item"]')
      ?.querySelector<HTMLInputElement>('input')
  if (!control) {
    throw new Error(`Expected control for label "${labelText}"`)
  }
  return control
}

function changeInput(input: HTMLInputElement, value: string): void {
  fireEvent.change(input, { target: { value } })
}

function submitForm(): void {
  const form = document.querySelector('#invitation-form')
  if (!form) {
    throw new Error('Expected invitation form')
  }
  fireEvent.submit(form)
}

async function waitForLoadedForm(): Promise<void> {
  await waitFor(() => expect(getSaveButton()).toBeEnabled())
}

afterEach(() => {
  apiClient.get = originalGet
  apiClient.put = originalPut
  Reflect.set(console, 'log', originalConsoleLog)
  toast.dismiss()
  localStorage.clear()
  renderedDrawer = null
})

describe('invitation code drawer', () => {
  test('loads the existing name and max uses for updates', async () => {
    const original = invitationCode(1, 25)
    apiClient.get = async () => ({ data: { success: true, data: original } })

    await renderDrawer(original)
    await waitForLoadedForm()

    expect(getControlByLabel('Name').value).toBe('invite-1')
    expect(getControlByLabel('Max Uses').value).toBe('25')
  })

  test('blocks updates and reports an error when loading rejects', async () => {
    const updates: unknown[] = []
    Reflect.set(console, 'log', () => undefined)
    apiClient.get = async () => {
      throw new Error('network failure')
    }
    apiClient.put = async (_url, data) => {
      updates.push(data)
      return { data: { success: true } }
    }

    await renderDrawer(invitationCode(1))
    await waitFor(() =>
      expect(document.body).toHaveTextContent('Something went wrong!')
    )

    expect(getSaveButton()).toBeDisabled()
    submitForm()
    expect(updates).toEqual([])
  })

  test('blocks updates and uses localized feedback for unsuccessful responses', async () => {
    apiClient.get = async () => ({
      data: { success: false, message: 'raw server message' },
    })

    await renderDrawer(invitationCode(1))
    await waitFor(() =>
      expect(document.body).toHaveTextContent('Failed to load')
    )

    expect(getSaveButton()).toBeDisabled()
    expect(document.body).not.toHaveTextContent('raw server message')
  })

  test('sends the loaded max uses together with edited fields', async () => {
    const original = invitationCode(1, 25)
    const updates: Array<Record<string, unknown>> = []
    apiClient.get = async () => ({ data: { success: true, data: original } })
    apiClient.put = async (_url, data) => {
      expect(data && typeof data === 'object').toBeTruthy()
      updates.push(data as Record<string, unknown>)
      return { data: { success: true, data: original } }
    }

    await renderDrawer(original)
    await waitForLoadedForm()

    changeInput(getControlByLabel('Name'), 'renamed')
    submitForm()
    await waitFor(() => expect(updates).toHaveLength(1))

    expect(updates[0]?.id).toBe(1)
    expect(updates[0]?.name).toBe('renamed')
    expect(updates[0]?.max_uses).toBe(25)
    expect(updates[0]?.expired_time).toBe(0)
  })

  test('sends the edited max uses value', async () => {
    const original = invitationCode(1, 25)
    const updates: Array<Record<string, unknown>> = []
    apiClient.get = async () => ({ data: { success: true, data: original } })
    apiClient.put = async (_url, data) => {
      expect(data && typeof data === 'object').toBeTruthy()
      updates.push(data as Record<string, unknown>)
      return { data: { success: true, data: original } }
    }

    await renderDrawer(original)
    await waitForLoadedForm()
    changeInput(getControlByLabel('Max Uses'), '42')
    submitForm()
    await waitFor(() => expect(updates).toHaveLength(1))

    expect(updates[0]?.max_uses).toBe(42)
  })

  test('ignores an older response after switching records', async () => {
    const first = invitationCode(1, 25)
    const second = invitationCode(2, 10)
    const firstRequest = deferred<{ data: unknown }>()
    const secondRequest = deferred<{ data: unknown }>()
    const requestedUrls: string[] = []
    const updates: Array<Record<string, unknown>> = []
    apiClient.get = (url) => {
      requestedUrls.push(url)
      if (url === '/api/invitation_code/1') return firstRequest.promise
      if (url === '/api/invitation_code/2') return secondRequest.promise
      throw new Error(`Unexpected GET ${url}`)
    }
    apiClient.put = async (_url, data) => {
      expect(data && typeof data === 'object').toBeTruthy()
      updates.push(data as Record<string, unknown>)
      return { data: { success: true, data: second } }
    }

    await renderDrawer(first)
    await rerenderDrawer(second)
    await waitFor(() =>
      expect(requestedUrls).toContain('/api/invitation_code/2')
    )
    secondRequest.resolve({ data: { success: true, data: second } })
    await waitForLoadedForm()

    firstRequest.resolve({ data: { success: true, data: first } })
    expect(getControlByLabel('Name').value).toBe('invite-2')

    changeInput(getControlByLabel('Name'), 'second')
    submitForm()
    await waitFor(() => expect(updates).toHaveLength(1))

    expect(updates[0]?.id).toBe(2)
    expect(updates[0]?.max_uses).toBe(10)
  })
})
