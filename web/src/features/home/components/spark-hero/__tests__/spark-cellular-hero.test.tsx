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
import { render } from '@testing-library/react'
import { describe, expect, test } from 'vitest'

import { SparkCellularHero } from '../spark-cellular-hero'

describe('spark cellular hero', () => {
  test('renders the decorative canvas as a full-bleed hidden container', () => {
    const { container } = render(<SparkCellularHero theme='light' />)

    const stage = container.firstElementChild
    expect(stage).not.toBeNull()
    expect(stage?.getAttribute('aria-hidden')).toBe('true')

    const canvas = container.querySelector<HTMLCanvasElement>('canvas')
    expect(canvas).not.toBeNull()
    // Always present so the animation layer exists even before JS paints it.
    expect(canvas?.className).toContain('absolute')
  })
})
