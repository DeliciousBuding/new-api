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
 * React Query cache keys for the relay observer feature (modelsQueryKeys
 * pattern: all / lists / list / detail).
 *
 * The cursor is part of the list key: with keyset pagination each page is its
 * own cache entry and must not collide with the previous one. Pages built on
 * this module MUST set `retry: false` on their queries — a degraded envelope
 * is a deliberate HTTP 200 answer, and 404/400 responses carry their own
 * error toasts, so automatic retries add nothing.
 */
import type { OverviewQueryParams, SessionQueryParams, TurnQueryParams } from './api'

export const observabilityQueryKeys = {
  all: ['observability'] as const,
  status: () => [...observabilityQueryKeys.all, 'status'] as const,
  overview: (filters?: OverviewQueryParams) =>
    [...observabilityQueryKeys.all, 'overview', filters] as const,
  sessions: {
    lists: () => [...observabilityQueryKeys.all, 'sessions', 'list'] as const,
    list: (filters: SessionQueryParams) =>
      [...observabilityQueryKeys.sessions.lists(), filters] as const,
    detail: (sessionId: string) =>
      [...observabilityQueryKeys.all, 'sessions', 'detail', sessionId] as const,
  },
  turns: {
    lists: () => [...observabilityQueryKeys.all, 'turns', 'list'] as const,
    list: (sessionId: string, filters: TurnQueryParams) =>
      [...observabilityQueryKeys.turns.lists(), sessionId, filters] as const,
    all: (filters: TurnQueryParams) =>
      [...observabilityQueryKeys.all, 'turns', 'all', filters] as const,
  },
  context: (turnId: string, sessionId: string) =>
    [...observabilityQueryKeys.all, 'context', turnId, sessionId] as const,
  transcript: {
    lists: () => [...observabilityQueryKeys.all, 'transcript', 'list'] as const,
    list: (sessionId: string) =>
      [...observabilityQueryKeys.transcript.lists(), sessionId] as const,
  },
}
