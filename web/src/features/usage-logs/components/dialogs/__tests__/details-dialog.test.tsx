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
import { render, screen } from '@testing-library/react'
import i18next from 'i18next'
import type { ReactNode } from 'react'
import { beforeAll, describe, expect, test, vi } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import type { UsageLog } from '../../../data/schema'
import { DetailsDialog } from '../details-dialog'

vi.mock('@/components/dialog', () => ({
  Dialog: ({ children }: { children?: ReactNode }) => children ?? null,
}))

// @lobehub/icons (behind the fork's lobe-icon loader) uses ESM directory
// imports that Vite's resolver cannot follow. The client-profile badge only
// renders brand icons; this test covers the error section, so stub the loader.
vi.mock('@/lib/lobe-icon', async () => {
  const React = await import('react')
  return {
    getLobeIcon: () =>
      React.createElement('svg', { 'data-mock-lobe-icon': 'true' }),
  }
})

beforeAll(() => {
  i18next.addResourceBundle('en', 'translation', {
    'Upstream Error': 'Upstream Error',
    'Error Code': 'Error Code',
    'Error Type': 'Error Type',
    Message: 'Message',
  })
})

type UpstreamError = {
  code?: string
  type?: string
  message?: string
}

function buildLog(upstreamError: UpstreamError): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: 0,
    type: 5,
    content: 'test',
    username: '',
    token_name: '',
    model_name: '',
    quota: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    use_time: 0,
    is_stream: false,
    channel: 0,
    channel_name: '',
    token_id: 0,
    group: '',
    ip: '',
    other: JSON.stringify({ admin_info: { upstream_error: upstreamError } }),
    request_id: '',
    upstream_request_id: '',
  }
}

function renderDialog(log: UsageLog, isAdmin: boolean) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <DetailsDialog
        log={log}
        isAdmin={isAdmin}
        isRoot={false}
        open
        onOpenChange={() => undefined}
      />
    </QueryClientProvider>
  )
}

describe('DetailsDialog upstream error section', () => {
  test('admin sees the upstream error with code and message', () => {
    renderDialog(buildLog({ code: 'Arrearage', message: 'Access denied' }), true)

    expect(screen.getByText('Upstream Error')).toBeInTheDocument()
    expect(screen.getByText('Error Code')).toBeInTheDocument()
    expect(screen.getByText('Arrearage')).toBeInTheDocument()
    expect(screen.getByText('Message')).toBeInTheDocument()
    expect(screen.getByText('Access denied')).toBeInTheDocument()
  })

  test('non-admin does not see the upstream error section', () => {
    renderDialog(
      buildLog({ code: 'Arrearage', message: 'Access denied' }),
      false
    )

    expect(screen.queryByText('Upstream Error')).not.toBeInTheDocument()
    expect(screen.queryByText('Arrearage')).not.toBeInTheDocument()
  })

  test('renders error type when it differs from code', () => {
    renderDialog(
      buildLog({ code: '429', type: 'overloaded_error', message: 'Overloaded' }),
      true
    )

    expect(screen.getByText('Error Code')).toBeInTheDocument()
    expect(screen.getByText('Error Type')).toBeInTheDocument()
    expect(screen.getByText('overloaded_error')).toBeInTheDocument()
  })

  test('omits a redundant error type equal to the code', () => {
    renderDialog(
      buildLog({ code: 'Arrearage', type: 'Arrearage', message: 'Access denied' }),
      true
    )

    expect(screen.getByText('Error Code')).toBeInTheDocument()
    expect(screen.queryByText('Error Type')).not.toBeInTheDocument()
  })

  test('shows a message-only error without code or type', () => {
    renderDialog(buildLog({ message: 'quota exceeded' }), true)

    expect(screen.getByText('Message')).toBeInTheDocument()
    expect(screen.getByText('quota exceeded')).toBeInTheDocument()
    expect(screen.queryByText('Error Code')).not.toBeInTheDocument()
    expect(screen.queryByText('Error Type')).not.toBeInTheDocument()
  })

  test('does not render a payload format row', () => {
    renderDialog(buildLog({ code: 'X', message: 'm' }), true)

    expect(screen.queryByText('Payload Format')).not.toBeInTheDocument()
  })
})
