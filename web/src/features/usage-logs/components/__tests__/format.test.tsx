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
*/
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

const { computeCacheRate } = await import('../../lib/format')

describe('computeCacheRate', () => {
  test('claude format: denominator = cache read + cache write + fresh input', () => {
    assert.equal(computeCacheRate(900, 50, undefined, true, 50), 90)
  })

  test('openai format: prompt_tokens already includes cache reads', () => {
    // cache=33408 / prompt=33409 → true hit rate ≈ 100%, not ~50%.
    assert.equal(computeCacheRate(33408, 33409, undefined, false), 100)
    assert.equal(computeCacheRate(39424, 42869, undefined, false), 92)
  })

  test('openai format: uses upstream input_tokens_total when present', () => {
    assert.equal(computeCacheRate(100, 50, 1000), 10)
  })

  test('no cache read returns null', () => {
    assert.equal(computeCacheRate(0, 50, undefined), null)
  })

  test('clamps at 100', () => {
    assert.equal(computeCacheRate(500, 1, undefined), 100)
  })

  test('zero total returns null', () => {
    assert.equal(computeCacheRate(100, 0, 0), null)
  })
})
