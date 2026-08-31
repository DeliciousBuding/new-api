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
import { describe, expect, test, vi } from 'vitest'

import { SparkHero } from '../spark-hero'

vi.mock('@/hooks/use-status', () => ({
  useStatus: () => ({ status: null, loading: false, error: null }),
}))

vi.mock('@lobehub/icons', () => ({
  CherryStudio: { Color: () => <span aria-hidden='true'>CS</span> },
}))

vi.mock('@tanstack/react-router', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@tanstack/react-router')>()
  return {
    ...actual,
    Link: (props: { children: React.ReactNode; to: string }) => (
      <a href={props.to}>{props.children}</a>
    ),
  }
})

describe('spark hero layout', () => {
  test('renders slogan, actions and the full-bleed background', () => {
    render(<SparkHero theme='light' />)

    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(
      'One gateway.'
    )
    expect(screen.getByText('Every model.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /get started/i })).toHaveAttribute(
      'href',
      '/sign-up'
    )
    expect(screen.getByRole('link', { name: /view pricing/i })).toHaveAttribute(
      'href',
      '/pricing'
    )
    expect(
      document.querySelector('a[href="https://docs.newapi.pro"]')
    ).not.toBeNull()
  })

  test('keeps separate desktop and responsive animation stages', () => {
    const { container } = render(<SparkHero theme='dark' />)

    const canvases = container.querySelectorAll('canvas')
    expect(canvases).toHaveLength(2)
    for (const canvas of canvases) {
      expect(canvas.parentElement).toHaveAttribute('aria-hidden', 'true')
    }
  })
})
