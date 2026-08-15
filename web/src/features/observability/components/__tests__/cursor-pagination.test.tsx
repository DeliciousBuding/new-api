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
import { fireEvent, render } from '@testing-library/react'
import i18next from 'i18next'
import { beforeAll, describe, expect, test } from 'vitest'

import { CursorPagination } from '../cursor-pagination'

beforeAll(() => {
  i18next.addResourceBundle('en', 'translation', {
    Previous: 'Previous',
    Next: 'Next',
    'Page {{current}}': 'Page {{current}}',
  })
})

function renderBar(props: {
  pageIndex: number
  canGoBack: boolean
  hasMore: boolean
  loading?: boolean
  onBack: () => void
  onNext: () => void
}) {
  return render(<CursorPagination {...props} />)
}

function textOf(container: HTMLElement): string {
  return container.textContent ?? ''
}

describe('CursorPagination (keyset footer bar)', () => {
  test('renders page index, previous and next buttons', () => {
    const { container } = renderBar({
      pageIndex: 3,
      canGoBack: true,
      hasMore: true,
      onBack: () => {},
      onNext: () => {},
    })
    expect(textOf(container)).toMatch(/Page 3/)
    expect(container.querySelector('button[disabled]')).toBeNull()
  })

  test('previous is disabled on the first page', () => {
    const { container } = renderBar({
      pageIndex: 1,
      canGoBack: false,
      hasMore: true,
      onBack: () => {},
      onNext: () => {},
    })
    const [previous, next] = container.querySelectorAll('button')
    expect(previous.disabled).toBeTruthy()
    expect(next.disabled).toBeFalsy()
  })

  test('next is disabled when the backend reports no more pages', () => {
    const { container } = renderBar({
      pageIndex: 1,
      canGoBack: false,
      hasMore: false,
      onBack: () => {},
      onNext: () => {},
    })
    const [previous, next] = container.querySelectorAll('button')
    expect(previous.disabled).toBeTruthy()
    expect(next.disabled).toBeTruthy()
  })

  test('both buttons are disabled while the list is loading', () => {
    const { container } = renderBar({
      pageIndex: 2,
      canGoBack: true,
      hasMore: true,
      loading: true,
      onBack: () => {},
      onNext: () => {},
    })
    for (const button of container.querySelectorAll('button')) {
      expect(button.disabled).toBeTruthy()
    }
  })

  test('clicking next/previous fires the navigation callbacks', () => {
    let nextClicks = 0
    let backClicks = 0
    const { container } = renderBar({
      pageIndex: 2,
      canGoBack: true,
      hasMore: true,
      onBack: () => {
        backClicks++
      },
      onNext: () => {
        nextClicks++
      },
    })
    const [previous, next] = container.querySelectorAll('button')
    fireEvent.click(next)
    fireEvent.click(previous)
    expect(nextClicks).toBe(1)
    expect(backClicks).toBe(1)
  })
})
