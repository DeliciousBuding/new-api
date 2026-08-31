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
/**
 * TokenDance bars (6 wide × 19 tall, 2-cell gaps, dome caps) and the
 * NewAPI-native palettes consumed by the Spark cellular hero.
 */

export type BarSpec = {
  gx: number
  gy: number
  w: number
  h: number
}

export interface Palette {
  bg: string
  grid: string
  edge: string
  tiles: string[]
  solid: string[]
}

/** TokenDance logo bars: 6 wide × 19 tall, 2-cell gaps, dome caps. */
export const TOKENDANCE_BARS: BarSpec[] = [
  { gx: 2, gy: 2, w: 6, h: 19 },
  { gx: 10, gy: 4, w: 6, h: 19 },
  { gx: 18, gy: 6, w: 6, h: 19 },
]

/**
 * NewAPI-native palettes: the canvas stays transparent so the page
 * background (--background) shows through — theme changes are picked up
 * automatically — while the grid dots follow each theme's border tone and
 * the tile blues stay the TokenDance brand family in both themes.
 */
export const NEWAPI_PALETTES: Record<'light' | 'dark', Palette> = {
  light: {
    bg: 'transparent',
    grid: 'oklch(0.86 0.02 230)',
    edge: '#3FC1F0',
    tiles: [
      '#29ABE2',
      '#0071BC',
      '#1B96D0',
      '#0E88C8',
      '#35B5E8',
      '#0A6EA8',
      '#1FA0D8',
      '#1288CE',
    ],
    solid: ['#0071BC', '#0D7BBF'],
  },
  dark: {
    bg: 'transparent',
    grid: 'oklch(1 0 0 / 14%)',
    edge: '#3FC1F0',
    tiles: [
      '#29ABE2',
      '#0071BC',
      '#1B96D0',
      '#0E88C8',
      '#35B5E8',
      '#0A6EA8',
      '#1FA0D8',
      '#1288CE',
    ],
    solid: ['#0071BC', '#0D7BBF'],
  },
}
