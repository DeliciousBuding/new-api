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
 * VChart spec builders for the observability overview. Pure functions — the
 * pattern mirrors features/dashboard/lib/cache.ts (buildCacheRateSpec): the
 * component memoizes data, passes it here, and spreads the returned spec
 * alongside the resolved theme + transparent background.
 */
import type { ObserverOverviewWindow } from '../types'

export interface TurnVolumeLabels {
  turns: string
  success: string
  failed: string
  windowStart: string
}

/** RFC3339 starts share the same format, so lexicographic order is
 * chronological. Both spec builders sort windows oldest → newest. */
function compareByStart(
  a: ObserverOverviewWindow,
  b: ObserverOverviewWindow
): number {
  if (a.start < b.start) return -1
  if (a.start > b.start) return 1
  return 0
}

/** Build the overview window-volume stacked bar spec (turns / success /
 * failed per window). Failed is derived as `turns - success` so the chart
 * stays a single query. VChart stacks on a long-format `series` field, so
 * each window expands to three rows (turns/success/failed). */
export function buildTurnVolumeSpec(
  windows: ObserverOverviewWindow[],
  labels: TurnVolumeLabels
) {
  // Oldest → newest so the x-axis reads left-to-right.
  const ordered = [...windows].sort(compareByStart)
  const values: Array<{ label: string; series: string; value: number }> = []
  for (const w of ordered) {
    const label = w.start.slice(11, 16) // HH:mm of the RFC3339 start
    values.push({ label, series: labels.turns, value: w.turns })
    values.push({ label, series: labels.success, value: w.success })
    values.push({
      label,
      series: labels.failed,
      value: Math.max(0, w.turns - w.success),
    })
  }

  return {
    type: 'bar' as const,
    data: [{ id: 'turnVolume', values }],
    xField: 'label',
    yField: 'value',
    seriesField: 'series',
    stack: true,
    bar: { style: { cornerRadius: 3 } },
    legends: { visible: false },
    color: {
      range: ['var(--chart-1)', 'var(--chart-2)', 'var(--destructive)'],
      // Order matches the series insertion order: turns, success, failed.
    },
    axes: [
      {
        orient: 'bottom',
        label: { style: { fontSize: 10 }, autoHide: true, autoLimit: true },
        tick: { visible: false },
      },
      {
        orient: 'left',
        grid: { visible: true, style: { lineDash: [3, 3] } },
        label: { style: { fontSize: 10 } },
      },
    ],
    tooltip: {
      mark: {
        content: [
          {
            key: (d: { series?: string }) => String(d?.series ?? labels.turns),
            value: (d: { value?: number }) => String(d?.value ?? 0),
          },
        ],
      },
    },
  }
}

/** Sparkline series (recent accepted/written trend) for the StatCards. The
 * overview windows are the only time-series the API exposes, so the StatCard
 * sparkline reuses window turn counts as a stable, server-side trend. */
export function windowSparkline(windows: ObserverOverviewWindow[]): number[] {
  return [...windows].sort(compareByStart).map((w) => w.turns)
}
