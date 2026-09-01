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
import { useEffect, useRef } from 'react'

import { cn } from '@/lib/utils'

import {
  type BarSpec,
  type Palette,
  NEWAPI_PALETTES,
  TOKENDANCE_BARS,
} from './palette'

/*
 * SparkCellularHero — an independent re-creation of a cellular-growth hero
 * animation, inspired by the visual style of Anthropic's site. Ported from
 * the spark-cellular-hero demo project (TokenDance bars mode) with the same
 * engine constants: grid=26, TICK=480, RATE=3, CAP=34, scale=1.15, and the
 * single-generation monotonic pull-back camera. The animation body is a
 * line-for-line translation; only the render wrapper (no built-in headline)
 * and theme-aware palettes differ so it can sit under NewAPI's own hero copy.
 */

const GRID = 26

function rng(seed: number) {
  return function () {
    seed |= 0
    seed = (seed + 0x6d2b79f5) | 0
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed)
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

function c2d(
  el: HTMLCanvasElement,
  wrf = false
): CanvasRenderingContext2D | null {
  return el.getContext('2d', { willReadFrequently: wrf })
}

function must2d(
  ctx: CanvasRenderingContext2D | null
): CanvasRenderingContext2D {
  if (!ctx) throw new Error('2D canvas context unavailable')
  return ctx
}

const cidx = (gx: number, gy: number, grid: number) => gy * grid + gx

interface SkeletonCell {
  idx: number
  gx: number
  gy: number
  dist: number
}

interface StartSpec {
  idx: number
  dir: number
  delay: number
  speed: number
}

interface Skeleton {
  cells: SkeletonCell[]
  neighbors: (idx: number) => number[]
  size: number
  starts: StartSpec[]
  layers: number[][]
}

interface GrownLayout {
  full: HTMLCanvasElement[]
  flip: HTMLCanvasElement[]
  solid: HTMLCanvasElement
}

/* ---------- glyph helpers ---------- */
function computeLayers(
  neighbors: (idx: number) => number[],
  starts: number[]
): number[][] {
  const dist = new Map<number, number>()
  for (const s of starts) dist.set(s, 0)
  const layers = [[...starts]]
  let frontier = [...starts]
  let d = 0
  while (frontier.length) {
    d++
    const next: number[] = []
    for (const i of frontier) {
      for (const nb of neighbors(i)) {
        if (!dist.has(nb)) {
          dist.set(nb, d)
          next.push(nb)
        }
      }
    }
    if (!next.length) break
    layers.push(next)
    frontier = next
  }
  return layers
}

/* ---------- hand-built bar skeleton (no rasterisation) ----------
   Bars use integer cell coords {gx,gy,w,h}. End caps are a 3-step dome
   (middle 2 / middle 4 / full on 6-wide), mirrored top/bottom. Each bar
   seeds a directed up/down pair in EVERY column of its centre row, so a
   bar rises/falls as a solid column; centre pairs are fastest and speed
   and launch delay fall off continuously toward the flanks. Mirror
   columns share the same jitter so a bar grows left and right in unison. */
function skeletonFromBars(bars: BarSpec[], grid: number): Skeleton {
  const cells: SkeletonCell[] = []
  for (const b of bars) {
    for (let r = 0; r < b.h; r++) {
      for (let c = 0; c < b.w; c++) {
        const edge = Math.min(r, b.h - 1 - r)
        let domeInset = 0
        if (edge === 0) domeInset = 2
        else if (edge === 1) domeInset = 1
        const inset = Math.min(domeInset, (b.w - 1) >> 1)
        // Dome profile: skip the inset cells at both bar ends (if any).
        if (c < inset || c >= b.w - inset) continue
        const gx = b.gx + c
        const gy = b.gy + r
        cells.push({
          idx: cidx(gx, gy, grid),
          gx,
          gy,
          dist: Math.hypot(gx - grid / 2, gy - grid / 2),
        })
      }
    }
  }
  cells.sort((a, b) => a.dist - b.dist || a.idx - b.idx)

  const active = new Set(cells.map((c) => c.idx))
  const adj = new Map<number, number[]>()
  for (const { idx, gx, gy } of cells) {
    const nb: number[] = []
    for (const [dx, dy] of [
      [1, 0],
      [-1, 0],
      [0, 1],
      [0, -1],
    ]) {
      const x = gx + dx
      const y = gy + dy
      if (x < 0 || y < 0 || x >= grid || y >= grid) continue
      const n = cidx(x, y, grid)
      if (active.has(n)) nb.push(n)
    }
    adj.set(idx, nb)
  }
  const neighbors = (idx: number) => adj.get(idx) || []

  const r = rng(0xb1e5)
  const starts: StartSpec[] = []
  for (const b of bars) {
    const row = b.gy + (b.h >> 1)
    const mid = (b.w - 1) / 2
    const jit = new Map<number, { s: number; d: number }>()
    const j = (rank: number) => {
      if (!jit.has(rank)) jit.set(rank, { s: 0.94 + r() * 0.12, d: r() * 0.3 })
      return jit.get(rank) as { s: number; d: number }
    }
    for (let c = 0; c < b.w; c++) {
      const i = cidx(b.gx + c, row, grid)
      const off = Math.abs(c - mid)
      const { s, d } = j(Math.min(c, b.w - 1 - c))
      const speed = (2.05 - off * 0.28) * s
      const delay = off * 0.3 + d
      starts.push(
        { idx: i, dir: -1, delay, speed },
        { idx: i, dir: 1, delay, speed }
      )
    }
  }
  return {
    cells,
    neighbors,
    size: cells.length,
    starts,
    layers: computeLayers(
      neighbors,
      starts.map((s) => s.idx)
    ),
  }
}

/* ---------- tiles / boards / layouts (all grid-aware) ---------- */
function makeTiles(pal: Palette): HTMLCanvasElement[] {
  const tiles: HTMLCanvasElement[] = []
  for (let k = 0; k < 12; k++) {
    const rnd = rng(9301 * (k + 1) + 49297)
    const c = document.createElement('canvas')
    c.width = c.height = 128
    const g = must2d(c2d(c))
    g.globalAlpha = 0.88
    g.fillStyle = pal.tiles[(rnd() * pal.tiles.length) | 0]
    g.fillRect(0, 0, 128, 128)
    for (let i = 0; i < 5; i++) {
      const x = 128 * rnd()
      const y = 128 * rnd()
      const rad = 128 * (0.25 + 0.35 * rnd())
      const gr = g.createRadialGradient(x, y, 0, x, y, rad)
      gr.addColorStop(0, pal.tiles[(rnd() * pal.tiles.length) | 0])
      gr.addColorStop(1, 'rgba(0,0,0,0)')
      g.globalAlpha = 0.14 + 0.14 * rnd()
      g.fillStyle = gr
      g.fillRect(0, 0, 128, 128)
    }
    g.globalAlpha = 0.06
    for (let i = 0; i < 220; i++) {
      g.fillStyle = pal.tiles[(rnd() * pal.tiles.length) | 0]
      const s = 1 + 2 * rnd()
      g.fillRect(128 * rnd(), 128 * rnd(), s, s)
    }
    g.globalAlpha = 0.14
    g.strokeStyle = pal.edge
    g.lineWidth = 6.4
    g.shadowColor = pal.edge
    g.shadowBlur = 12.8
    g.beginPath()
    g.roundRect(8.96, 8.96, 110.08, 110.08, 25.6)
    g.stroke()
    g.shadowBlur = 0
    g.globalAlpha = 1
    g.globalCompositeOperation = 'destination-in'
    g.fillStyle = '#000'
    const a = rnd() * Math.PI * 2
    const b = rnd() * Math.PI * 2
    const c2 = rnd() * Math.PI * 2
    g.beginPath()
    for (let i = 0; i <= 56; i++) {
      const t = (i / 56) * Math.PI * 2
      const co = Math.cos(t)
      const si = Math.sin(t)
      const rr =
        (1 / Math.pow(co ** 4 + si ** 4, 0.25)) *
        58.88 *
        (1 +
          0.03 * Math.sin(3 * t + a) +
          0.018 * Math.sin(7 * t + b) +
          0.01 * Math.sin(11 * t + c2))
      const x = 64 + co * rr
      const y = 64 + si * rr
      if (i === 0) g.moveTo(x, y)
      else g.lineTo(x, y)
    }
    g.closePath()
    g.fill()
    g.globalCompositeOperation = 'source-over'
    tiles.push(c)
  }
  return tiles
}

function makeMip(canvas: HTMLCanvasElement): HTMLCanvasElement[] {
  const levels = [canvas]
  let cur = canvas
  while (cur.width > 38) {
    const sz = Math.max(19, cur.width >> 1)
    const c = document.createElement('canvas')
    c.width = c.height = sz
    const g = must2d(c2d(c))
    g.imageSmoothingEnabled = true
    g.imageSmoothingQuality = 'high'
    g.drawImage(cur, 0, 0, sz, sz)
    levels.push(c)
    cur = c
  }
  return levels
}

const pickFor = (mip: HTMLCanvasElement[], size: number) => {
  for (let i = mip.length - 1; i >= 0; i--) {
    if (mip[i].width >= size) return mip[i]
  }
  return mip[0]
}

function drawBoard(
  tiles: HTMLCanvasElement[],
  seed: number,
  size: number,
  layout: { gx: number; gy: number }[],
  count: number,
  grid: number
): HTMLCanvasElement {
  const c = document.createElement('canvas')
  c.width = c.height = size
  const g = must2d(c2d(c))
  const r = size / grid
  const s = 0.78 * r
  const off = (r - s) / 2
  for (let i = 0; i < count; i++) {
    const { gx, gy } = layout[i]
    const hash =
      ((0x466f45d * gx) ^ (0x127409f * gy) ^ (0x4f9ffb7 * seed)) >>> 0
    g.save()
    g.translate(gx * r + off + s / 2, gy * r + off + s / 2)
    g.scale((hash >> 4) & 1 ? -1 : 1, (hash >> 5) & 1 ? -1 : 1)
    g.drawImage(tiles[hash % 12], -s / 2, -s / 2, s, s)
    g.restore()
  }
  g.globalCompositeOperation = 'destination-in'
  g.fillStyle = '#000'
  g.beginPath()
  g.roundRect(0, 0, size, size, 0.18 * size)
  g.fill()
  return c
}

function makeSolid(pal: Palette, seed: number): HTMLCanvasElement {
  const rnd = rng((0x165667b1 * seed) >>> 0)
  const c = document.createElement('canvas')
  c.width = c.height = 152
  const g = must2d(c2d(c))
  g.beginPath()
  g.roundRect(0, 0, 152, 152, 27.36)
  g.fillStyle = pal.solid[seed % pal.solid.length]
  g.fill()
  g.save()
  g.clip()
  for (let i = 0; i < 4; i++) {
    const x = 152 * rnd()
    const y = 152 * rnd()
    const rad = 152 * (0.35 + 0.35 * rnd())
    const gr = g.createRadialGradient(x, y, 0, x, y, rad)
    gr.addColorStop(0, pal.solid[(rnd() * pal.solid.length) | 0])
    gr.addColorStop(1, 'rgba(0,0,0,0)')
    g.globalAlpha = 0.22
    g.fillStyle = gr
    g.fillRect(0, 0, 152, 152)
  }
  g.restore()
  return c
}

function growLayouts(
  pal: Palette,
  tiles: HTMLCanvasElement[],
  grid: number
): GrownLayout[] {
  const layouts: GrownLayout[] = []
  const total = grid * grid
  for (let k = 0; k < 8; k++) {
    const rnd = rng((0x9e3779b1 * (k + 1)) >>> 0)
    const inSet = new Set<number>()
    const order: { gx: number; gy: number }[] = []
    const add = (gx: number, gy: number) => {
      const i = cidx(gx, gy, grid)
      if (!inSet.has(i)) {
        inSet.add(i)
        order.push({ gx, gy })
        return true
      }
      return false
    }
    const mid = (grid / 2) | 0
    add(mid, mid)
    const trails = 5 + ((3 * rnd()) | 0)
    for (let t = 0; t < trails; t++) {
      let gx = mid
      let gy = mid
      for (let step = 0; step < grid * 2; step++) {
        const opts: ReadonlyArray<readonly [number, number]> = (
          [
            [1, 0],
            [-1, 0],
            [0, 1],
            [0, -1],
          ] as const
        ).filter(([dx, dy]) => {
          const x = gx + dx
          const y = gy + dy
          return (
            x >= 0 &&
            y >= 0 &&
            x < grid &&
            y < grid &&
            !inSet.has(cidx(x, y, grid))
          )
        })
        if (!opts.length) break
        let best = opts[0]
        let score = -Infinity
        for (const [dx, dy] of opts) {
          const w = Math.hypot(gx + dx - mid, gy + dy - mid) + 1.2 * rnd()
          if (w > score) {
            score = w
            best = [dx, dy] as const
          }
        }
        gx += best[0]
        gy += best[1]
        add(gx, gy)
        if (gx === 0 || gy === 0 || gx === grid - 1 || gy === grid - 1) break
      }
    }
    const frontier: { gx: number; gy: number }[] = []
    const pf = (gx: number, gy: number) => {
      for (const [dx, dy] of [
        [1, 0],
        [-1, 0],
        [0, 1],
        [0, -1],
      ]) {
        const x = gx + dx
        const y = gy + dy
        if (
          x >= 0 &&
          y >= 0 &&
          x < grid &&
          y < grid &&
          !inSet.has(cidx(x, y, grid))
        ) {
          frontier.push({ gx: x, gy: y })
        }
      }
    }
    for (const { gx, gy } of order) pf(gx, gy)
    while (order.length < total) {
      let t: { gx: number; gy: number } | undefined
      do {
        const j = (rnd() * frontier.length) | 0
        t = frontier[j]
        frontier[j] = frontier[frontier.length - 1]
        frontier.pop()
      } while (t && inSet.has(cidx(t.gx, t.gy, grid)))
      if (!t) break
      add(t.gx, t.gy)
      pf(t.gx, t.gy)
    }
    const full = makeMip(drawBoard(tiles, k + 1, 608, order, total, grid))
    const flip = Array.from({ length: 8 }, (_, a) =>
      drawBoard(
        tiles,
        k + 1,
        152,
        order,
        Math.ceil(((a + 1) / 8) * total),
        grid
      )
    )
    const solid = makeSolid(pal, k + 1)
    layouts.push({ full, flip, solid })
  }
  return layouts
}

interface Agent {
  idx: number
  prev: number
  since: number
  bornAt: number
  dieAt: number
  dir: number
  speed: number
  startAt: number
}

interface Colony {
  born: number
  cells: Map<number, number>
  agents: Agent[]
  lastTick: number
  ticks: number
  layer: number
  doneAt: number
}

interface SparkCellularHeroProps {
  theme: 'light' | 'dark'
  bars?: BarSpec[]
  scale?: number
  className?: string
}

export function SparkCellularHero(props: SparkCellularHeroProps) {
  const { theme, bars = TOKENDANCE_BARS, scale = 1.15, className } = props
  const heroRef = useRef<HTMLDivElement>(null)
  const canvasRef = useRef<HTMLCanvasElement>(null)

  useEffect(() => {
    const canvasEl = canvasRef.current
    const heroEl = heroRef.current
    if (!canvasEl || !heroEl) return
    const ctxEl = c2d(canvasEl)
    if (!ctxEl) return
    // Bind non-null aliases after the guards: the animation closures run
    // outside this effect's narrowing scope, so they need definite types.
    const canvas = canvasEl
    const hero = heroEl
    const ctx = ctxEl

    const isBars = !!(bars && bars.length)
    const TICK = isBars ? 480 : 450
    const RATE = isBars ? 3 : 7
    const CAP = isBars ? 34 : 48
    const T = isBars ? Math.log(GRID / 0.78) * 0.45 : Math.log(GRID / 0.78)
    const pal = NEWAPI_PALETTES[theme] || NEWAPI_PALETTES.light
    const skeleton = skeletonFromBars(bars, GRID)
    const LAYOUTS = growLayouts(pal, makeTiles(pal), GRID)

    let S = 0
    let N = 0
    let W = 1
    let D = 0
    let E = 0
    let B = 0
    let colony: Colony
    let P = 0
    let F = isBars ? 0.12 * T : 0
    let R = 0
    let running = false
    let destroyed = false
    let rafId = 0
    let lastFrame = 0
    const simRand = rng(0xc1a0de)

    function resize() {
      W = Math.min(2, window.devicePixelRatio || 1)
      S = hero.clientWidth
      N = hero.clientHeight
      canvas.width = Math.round(S * W)
      canvas.height = Math.round(N * W)
      ctx.setTransform(W, 0, 0, W, 0, 0)
      ctx.imageSmoothingEnabled = true
      ctx.imageSmoothingQuality = 'high'
      const desk = S >= 992
      let boardScale = 0.75 * Math.min(0.9 * S, 0.9 * N)
      if (desk) {
        D = (isBars ? 0.73 : 0.66) * S
        E = (isBars ? 0.53 : 0.5) * N
        boardScale = isBars
          ? 0.56 * Math.min(0.56 * S, 0.9 * N)
          : 0.58 * Math.min(0.6 * S, N)
      } else {
        D = 0.5 * S
        E = 0.5 * N
      }
      // The responsive layout renders inside a dedicated block below the copy.
      // Keep the artwork large there; the former 0.45 * height clamp reduced it
      // to a thumbnail on phones.
      B = boardScale * scale
    }

    const newColony = (born: number): Colony => ({
      born,
      cells: new Map(),
      agents: [],
      lastTick: born,
      ticks: 0,
      layer: 0,
      doneAt: 0,
    })

    const seedColony = (col: Colony, now: number, seeded: boolean) => {
      for (const s of skeleton.starts) {
        col.agents.push({
          idx: s.idx,
          prev: s.idx,
          since: now,
          bornAt: seeded ? -Infinity : now,
          dieAt: 0,
          dir: s.dir,
          speed: s.speed,
          startAt: seeded ? -Infinity : now + s.delay * TICK,
        })
      }
    }

    function drawGridDots(cx: number, cy: number, spacing: number, a: number) {
      if (spacing < 2) return
      const n = Math.max(0, Math.min(1, (spacing - 2) / 7))
      const alpha = (0.45 + 0.45 * Math.min(1, spacing / 90)) * a * n
      if (alpha < 0.01) return
      const rx = (((cx + spacing / 2) % spacing) + spacing) % spacing
      const ry = (((cy + spacing / 2) % spacing) + spacing) % spacing
      ctx.fillStyle = pal.grid
      ctx.globalAlpha = alpha
      for (let y = ry; y <= N; y += spacing) {
        for (let x = rx; x <= S; x += spacing) {
          ctx.fillRect(x - 1, y - 1, 2, 2)
        }
      }
      ctx.globalAlpha = 1
    }

    function render(now: number, boardSize: number) {
      /* clearRect is required: the palette bg is transparent so the page
         background shows through, and filling with a transparent color is a
         no-op that leaves every previous frame's pixels on the canvas. */
      ctx.clearRect(0, 0, S, N)
      const cell = boardSize / GRID
      const gridSpacing = (boardSize / GRID) * 2
      // Decouple the mobile artwork scale from the dot pitch so enlarging the
      // bars does not turn the fine Spark grid into a coarse checkerboard.
      drawGridDots(D, E, S >= 992 ? gridSpacing : Math.min(9, gridSpacing), 1)
      drawGridDots(D, E, 0.78 * cell, 1)
      drawCells(now, cell)
    }

    function drawCells(now: number, cs: number) {
      if (cs < 0.4) return
      const t = 2 * cs * 0.78
      for (const [idx, bornAt] of colony.cells) {
        const x = D + 2 * cs * ((idx % GRID) - GRID / 2)
        const y = E + 2 * cs * (((idx / GRID) | 0) - GRID / 2)
        const lay = LAYOUTS[idx % 8]
        /* sub-tick birth jitter: cells born in the same tick stagger their
           flip start by a stable per-cell hash, so a row ripples open from
           the centre instead of snapping on all at once. */
        const jit = isBars
          ? ((((idx * 0x9e3779b9) >>> 0) % 1000) / 1000) * TICK * 0.5
          : 0
        const age = now - (bornAt + jit)
        if (age < 700) {
          const fi = Math.max(0, Math.min(7, ((age / 700) * 8) | 0))
          ctx.drawImage(lay.flip[fi], x - t / 2, y - t / 2, t, t)
        } else {
          const eAge = Math.min(1, (age - 700) / 1600)
          if (eAge < 1) {
            ctx.drawImage(pickFor(lay.full, t * W), x - t / 2, y - t / 2, t, t)
          }
          if (eAge > 0) {
            ctx.globalAlpha = eAge
            ctx.drawImage(lay.solid, x - t / 2, y - t / 2, t, t)
            ctx.globalAlpha = 1
          }
        }
      }
    }

    function simulate(now: number) {
      if (colony.doneAt) return
      for (; now - colony.lastTick >= TICK; ) {
        colony.lastTick += TICK
        colony.ticks++
        const tickT = colony.lastTick
        const occupied = new Set(
          colony.agents.filter((g) => !g.dieAt).map((g) => g.idx)
        )
        const next: Agent[] = []
        for (const g of colony.agents) {
          if (g.dieAt) {
            if (tickT - g.dieAt < 350) next.push(g)
            continue
          }
          /* staggered launch: a waiting column agent holds at its seed cell
             (not yet colonised) until its beat arrives. */
          if (g.startAt && tickT < g.startAt) {
            next.push(g)
            continue
          }
          const cands = skeleton
            .neighbors(g.idx)
            .filter((nb) => !colony.cells.has(nb) && !occupied.has(nb))
          colony.cells.set(g.idx, tickT)
          if (!cands.length) {
            g.dieAt = tickT
            next.push(g)
            continue
          }
          if (g.dir) {
            /* directed column agent: march ~`speed` rows per tick (faster
               near the centre, slower at the flanks). `speed` is fractional:
               the integer part always advances, the remainder is a per-tick
               chance to take one more step. */
            let move = g.speed || 1
            let done = false
            while (move > 0) {
              const nb = skeleton
                .neighbors(g.idx)
                .find(
                  (n) =>
                    !colony.cells.has(n) &&
                    !occupied.has(n) &&
                    ((n / GRID) | 0) - ((g.idx / GRID) | 0) === g.dir
                )
              if (nb === undefined) {
                done = true
                break
              }
              colony.cells.set(nb, tickT)
              g.prev = g.idx
              g.idx = nb
              g.since = tickT
              occupied.add(nb)
              move -= 1
              if (move > 0 && move < 1 && simRand() >= move) break
            }
            if (done) g.dieAt = tickT
            next.push(g)
            continue
          }
          const pick = cands[(simRand() * cands.length) | 0]
          g.prev = g.idx
          g.idx = pick
          g.since = tickT
          occupied.add(pick)
          next.push(g)
        }
        colony.agents = next

        const alive = colony.agents.filter((g) => !g.dieAt).length
        const frontier = (() => {
          const out: number[] = []
          const seen = new Set<number>()
          for (const key of colony.cells.keys()) {
            for (const nb of skeleton.neighbors(key)) {
              if (!colony.cells.has(nb) && !seen.has(nb)) {
                seen.add(nb)
                out.push(nb)
              }
            }
          }
          return out
        })()
        const avail = frontier.filter((b) => !occupied.has(b))
        if (!avail.length && !alive) {
          colony.doneAt = tickT
          break
        }

        const rate = RATE / (1 + 0.25 * P)
        /* bars: a fully choreographed column march — every cell is colonised
           by its own column's directed pair, no random spawns ever. */
        const target = isBars
          ? 0
          : Math.min(
              CAP,
              Math.max(
                skeleton.starts.length,
                Math.floor((colony.ticks / rate) ** 2)
              )
            )
        let got = alive
        for (; got < target && avail.length; ) {
          const j = (simRand() * avail.length) | 0
          const idx = avail[j]
          avail[j] = avail[avail.length - 1]
          avail.pop()
          colony.agents.push({
            idx,
            prev: idx,
            since: tickT,
            bornAt: tickT,
            dieAt: 0,
            dir: 0,
            speed: 1,
            startAt: tickT,
          })
          occupied.add(idx)
          got++
        }
      }
    }

    function frame(now: number) {
      if (destroyed || !running) return
      const dt = Math.min(50, now - lastFrame)
      lastFrame = now
      simulate(now)

      const ratio = colony.cells.size / skeleton.size
      const limited = P + 1 >= (isBars ? 1 : 3)
      if (colony.doneAt && now - colony.doneAt >= 500 && !limited) {
        colony = newColony(now)
        colony.lastTick = now
        seedColony(colony, now, true)
        P++
      }

      const n = (() => {
        if (colony.doneAt && !limited) return 1
        return isBars ? 0.12 + 0.18 * ratio : -0.04 + 0.34 * ratio
      })()
      const phaseTarget = (P + n) * T
      const steps = Math.max(1, Math.ceil(dt / 12))
      const h = dt / steps
      for (let i = 0; i < steps; i++) {
        const d = 81e-8 * (phaseTarget - F) - 0.00153 * R
        R += d * h
        F += R * h
      }

      render(now, B * Math.exp(P * T - F))

      if (
        limited &&
        colony.doneAt &&
        now - colony.doneAt >= 2300 &&
        Math.abs(R) < 1e-5
      ) {
        running = false
        return
      }
      rafId = requestAnimationFrame(frame)
    }

    function prime() {
      const now = performance.now()
      colony = newColony(now)
      const born = now - 700 - 1600
      for (const { idx } of skeleton.cells) colony.cells.set(idx, born)
      colony.doneAt = now
      P = 0
      /* prime paints the SETTLED framing: the full board at the opening
         phase would kiss the viewport edges — settle first */
      if (isBars) F = 0.3 * T
      render(now, B * Math.exp(P * T - F))
    }

    /* primeOpening paints the OPENING framing (dot grid, empty colony) so the
       first frame before the observer starts the growth never flashes the
       settled full board. prime() stays reserved for reduced-motion and for
       the settled state after a resize once the choreography has finished. */
    function primeOpening() {
      const now = performance.now()
      colony = newColony(now)
      P = 0
      F = isBars ? 0.12 * T : -0.04 * T
      R = 0
      render(now, B * Math.exp(P * T - F))
    }

    function startAnimation() {
      running = true
      primeOpening()
      seedColony(colony, performance.now(), false)
      lastFrame = performance.now()
      rafId = requestAnimationFrame(frame)
    }

    resize()
    const reduced = window.matchMedia(
      '(prefers-reduced-motion: reduce)'
    ).matches
    let io: IntersectionObserver | null = null
    let ro: ResizeObserver | null = null
    if (reduced) {
      prime()
    } else {
      primeOpening()
      if (typeof IntersectionObserver !== 'undefined') {
        io = new IntersectionObserver(
          ([entry]) => {
            if (entry.isIntersecting) {
              if (running) return
              startAnimation()
            } else if (running) {
              running = false
              cancelAnimationFrame(rafId)
            }
          },
          { threshold: 0.1 }
        )
        io.observe(hero)
      } else {
        // No IntersectionObserver (e.g. jsdom tests): start immediately.
        startAnimation()
      }
      ro = new ResizeObserver(() => {
        resize()
        if (running) {
          // Mid-growth resize: keep the live colony, just reframe it.
          render(performance.now(), B * Math.exp(P * T - F))
        } else if (colony.doneAt) {
          prime()
        } else {
          primeOpening()
        }
      })
      ro.observe(hero)
    }

    return () => {
      destroyed = true
      running = false
      cancelAnimationFrame(rafId)
      if (io) io.disconnect()
      if (ro) ro.disconnect()
    }
  }, [theme, bars, scale])

  return (
    <div
      ref={heroRef}
      aria-hidden='true'
      className={cn('relative h-full w-full overflow-hidden', className)}
    >
      <canvas
        ref={canvasRef}
        className='absolute inset-0 block h-full w-full'
      />
    </div>
  )
}
