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
 * API client for the Root-only relay observer endpoints (T3 HTTP contract).
 *
 * One function per route registered in router/api-router.go under
 * /api/relay-observer (all GET, all behind middleware.RootAuth). Parameter
 * names are the backend query parameters verbatim. Errors are handled by the
 * shared axios interceptors (401/403 refresh, business-error toast); the
 * degraded envelope (HTTP 200) is NOT an error and passes through as data.
 */
import { api } from '@/lib/http-client'

import {
  observerOverviewSchema,
  observerSessionPageSchema,
  observerSessionSchema,
  observerStatusSchema,
  observerTranscriptPageSchema,
  observerTurnContextSchema,
  observerTurnPageSchema,
  parseObserverResponse,
  type ObserverOverview,
  type ObserverResponse,
  type ObserverSession,
  type ObserverSessionPage,
  type ObserverStatus,
  type ObserverTranscriptPage,
  type ObserverTurnContext,
  type ObserverTurnPage,
} from './types'

// ============================================================================
// Query parameter types (snake_case, one field per backend query parameter)
// ============================================================================

/** GET /api/relay-observer/overview */
export interface OverviewQueryParams {
  window_seconds?: number
  windows?: number
}

/** GET /api/relay-observer/sessions */
export interface SessionQueryParams {
  page_size?: number
  cursor?: string
  node_scope?: string
  user_id?: number
  client_family?: string
  model?: string
  success?: boolean
  country?: string
  asn?: number
  ip?: string
  ip_trust?: 'direct' | 'proxy' | 'none'
  from?: string // RFC3339
  to?: string // RFC3339
}

/** GET /api/relay-observer/sessions/:id/turns */
export interface TurnQueryParams {
  page_size?: number
  cursor?: string
  user_id?: number
  model?: string
  success?: boolean
  error_type?: string
  ip_trust?: 'direct' | 'proxy' | 'none'
}

// ============================================================================
// Helpers
// ============================================================================

/** Serialize query params into a URL query string, dropping empty values
 * (mirrors the usage-logs buildQueryParams convention). */
function buildQueryString(params: object): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') {
      search.append(key, String(value))
    }
  }
  const query = search.toString()
  return query ? `?${query}` : ''
}

// ============================================================================
// Endpoints
// ============================================================================

/** GET /api/relay-observer/status — safe in-memory snapshot, no DB query. */
export async function getStatus(): Promise<ObserverResponse<ObserverStatus>> {
  const res = await api.get('/api/relay-observer/status')
  return parseObserverResponse(observerStatusSchema, res.data)
}

/** GET /api/relay-observer/overview — aggregate windows and totals. */
export async function getOverview(
  params: OverviewQueryParams = {}
): Promise<ObserverResponse<ObserverOverview>> {
  const res = await api.get(
    `/api/relay-observer/overview${buildQueryString(params)}`
  )
  return parseObserverResponse(observerOverviewSchema, res.data)
}

/** GET /api/relay-observer/sessions — one keyset page of sessions. */
export async function listSessions(
  params: SessionQueryParams = {}
): Promise<ObserverResponse<ObserverSessionPage>> {
  const res = await api.get(
    `/api/relay-observer/sessions${buildQueryString(params)}`
  )
  return parseObserverResponse(observerSessionPageSchema, res.data)
}

/** GET /api/relay-observer/sessions/:id — one session summary; 404 when
 * unknown. */
export async function getSession(
  sessionId: string
): Promise<ObserverResponse<ObserverSession>> {
  const res = await api.get(`/api/relay-observer/sessions/${sessionId}`)
  return parseObserverResponse(observerSessionSchema, res.data)
}

/** GET /api/relay-observer/sessions/:id/turns — one keyset page of turns. */
export async function listTurns(
  sessionId: string,
  params: TurnQueryParams = {}
): Promise<ObserverResponse<ObserverTurnPage>> {
  const res = await api.get(
    `/api/relay-observer/sessions/${sessionId}/turns${buildQueryString(params)}`
  )
  return parseObserverResponse(observerTurnPageSchema, res.data)
}

/** GET /api/relay-observer/turns/:id/context — canonical content
 * reconstruction; session_id is a mandatory query parameter. */
export async function getTurnContext(
  turnId: string,
  sessionId: string
): Promise<ObserverResponse<ObserverTurnContext>> {
  const res = await api.get(
    `/api/relay-observer/turns/${turnId}/context${buildQueryString({
      session_id: sessionId,
    })}`
  )
  return parseObserverResponse(observerTurnContextSchema, res.data)
}

/** GET /api/relay-observer/sessions/:id/transcript — one page of the
 * flattened conversation stream. `direction=latest` (default) returns the
 * trailing page; `direction=older` with `cursor` (the previous page's
 * `prev_cursor`) returns the page before it. */
export interface TranscriptQueryParams {
  page_size?: number
  cursor?: number
  direction?: 'latest' | 'older'
}

export async function getSessionTranscript(
  sessionId: string,
  params: TranscriptQueryParams = {}
): Promise<ObserverResponse<ObserverTranscriptPage>> {
  const res = await api.get(
    `/api/relay-observer/sessions/${sessionId}/transcript${buildQueryString(
      params
    )}`
  )
  return parseObserverResponse(observerTranscriptPageSchema, res.data)
}
