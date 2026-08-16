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
 * Zod schemas and types for the Root-only relay observer API.
 *
 * Field names keep the backend JSON snake_case verbatim — the SSOT is
 * controller/relay_observer_query.go (T3.2 implementation is the spec).
 * Timestamps are RFC3339 strings (Go time.Time JSON encoding). The one
 * exception is /api/relay-observer/status, whose DTO (relayobserver.Status)
 * carries no JSON tags and therefore serializes with Go's default PascalCase
 * field names.
 *
 * Response shape: every endpoint returns `{ success: true, message, data }`.
 * A store failure or query timeout is NOT an HTTP error — it is the degraded
 * envelope (HTTP 200, `data.degraded === true`), see observerDegradedSchema.
 * Use `isObserverDegraded(data)` to branch before parsing the real shape.
 */
import { z } from 'zod'

// ============================================================================
// Status (PascalCase keys — the Go DTO has no JSON tags)
// ============================================================================

export const observerStatusSchema = z.object({
  Enabled: z.boolean(),
  ReasonCode: z.string(),
  IPTrust: z.string(),
  QueueCount: z.number(),
  QueueBytes: z.number(),
  PendingContentCount: z.number(),
  PendingContentBytes: z.number(),
  AcceptedTotal: z.number(),
  WrittenTotal: z.number(),
  DroppedTotal: z.number(),
  CircuitOpen: z.boolean(),
  CircuitCooldown: z.number(),
  PGLatencyMS: z.number(),
  ContentGapsTotal: z.number(),
  ContentRetriedTotal: z.number(),
  ContentDroppedTotal: z.number(),
  RecentVolume: z.number(),
  LastRetentionPass: z.string(),
  RetentionTurnsDeleted: z.number(),
  RetentionSessionsDeleted: z.number(),
  RetentionObjectsDeleted: z.number(),
  RetentionFailures: z.number(),
  RetentionTurnsPending: z.number(),
  RetentionSessionsPending: z.number(),
  RetentionObjectsPending: z.number(),
  RetentionBacklogAge: z.number(),
  RetentionBacklogTruncated: z.boolean(),
})
export type ObserverStatus = z.infer<typeof observerStatusSchema>

// ============================================================================
// Overview
// ============================================================================

export const observerOverviewWindowSchema = z.object({
  start: z.string(), // RFC3339
  turns: z.number(),
  success: z.number(),
})
export type ObserverOverviewWindow = z.infer<
  typeof observerOverviewWindowSchema
>

export const observerOverviewSchema = z.object({
  window_seconds: z.number(),
  windows: z.array(observerOverviewWindowSchema),
  session_count: z.number(),
  turn_count: z.number(),
  gap_count: z.number(),
})
export type ObserverOverview = z.infer<typeof observerOverviewSchema>

// ============================================================================
// Sessions
// ============================================================================

export const observerSessionSchema = z.object({
  session_id: z.string(), // UUID
  node_scope: z.string(),
  user_id: z.number(),
  username: z.string().optional(),
  client_family: z.string(),
  first_seen: z.string(), // RFC3339
  last_seen: z.string(), // RFC3339
  turn_count: z.number(),
  gap_count: z.number(),
})
export type ObserverSession = z.infer<typeof observerSessionSchema>

// ============================================================================
// Turns
// ============================================================================

export const observerAttemptSchema = z.object({
  channel_id: z.number(),
  group: z.string(),
  status_code: z.number(),
  error_code: z.string(),
  elapsed_ms: z.number(),
})
export type ObserverAttempt = z.infer<typeof observerAttemptSchema>

export const observerTurnSchema = z.object({
  turn_id: z.string(), // UUID
  event_id: z.string(),
  // Nullable: worker-buffered turns may not be assigned to a session yet.
  session_id: z.string().nullable(),
  occurred_at: z.string(), // RFC3339
  node_scope: z.string(),
  user_id: z.number(),
  model: z.string(),
  upstream_model: z.string(),
  relay_format: z.string(),
  success: z.boolean(),
  status_code: z.number(),
  error_type: z.string(),
  error_code: z.string(),
  latency_ms: z.number(),
  stream: z.boolean(),
  prompt_tokens: z.number(),
  completion_tokens: z.number(),
  cached_tokens: z.number(),
  quota: z.number(),
  attempts: z.array(observerAttemptSchema),
  content_state: z.string(),
})
export type ObserverTurn = z.infer<typeof observerTurnSchema>

// ============================================================================
// Keyset page envelope
// ============================================================================

export const observerPageMetaSchema = z.object({
  next_cursor: z.string(), // opaque keyset cursor; '' when has_more is false
  has_more: z.boolean(),
})
export type ObserverPageMeta = z.infer<typeof observerPageMetaSchema>

export const observerSessionPageSchema = z.object({
  page_size: z.number(),
  items: z.array(observerSessionSchema),
  meta: observerPageMetaSchema,
})
export type ObserverSessionPage = z.infer<typeof observerSessionPageSchema>

export const observerTurnPageSchema = z.object({
  page_size: z.number(),
  items: z.array(observerTurnSchema),
  meta: observerPageMetaSchema,
})
export type ObserverTurnPage = z.infer<typeof observerTurnPageSchema>

// ============================================================================
// Turn context (canonical content reconstruction)
// ============================================================================

export const observerMediaRefSchema = z.object({
  kind: z.string(),
  media_type: z.string().optional(),
  logical_bytes: z.number(),
  hmac: z.string(),
})
export type ObserverMediaRef = z.infer<typeof observerMediaRefSchema>

export const observerToolCallRefSchema = z.object({
  id: z.string().optional(),
  name: z.string().optional(),
  arguments: z.unknown().optional(), // keeps its original JSON shape
})
export type ObserverToolCallRef = z.infer<typeof observerToolCallRefSchema>

export const observerToolResultRefSchema = z.object({
  tool_call_id: z.string().optional(),
  output: z.unknown().optional(),
})
export type ObserverToolResultRef = z.infer<
  typeof observerToolResultRefSchema
>

export const observerCanonicalPartSchema = z.object({
  type: z.string(),
  text: z.string().optional(),
  media: observerMediaRefSchema.optional(),
  call: observerToolCallRefSchema.optional(),
  result: observerToolResultRefSchema.optional(),
  logical_bytes: z.number().optional(),
  hmac: z.string().optional(),
})
export type ObserverCanonicalPart = z.infer<
  typeof observerCanonicalPartSchema
>

export const observerCanonicalItemSchema = z.object({
  kind: z.string(),
  role: z.string().optional(),
  tool_call_id: z.string().optional(),
  content: z.array(observerCanonicalPartSchema).optional(),
  logical_bytes: z.number(),
  hmac: z.string(),
  truncated: z.boolean().optional(),
})
export type ObserverCanonicalItem = z.infer<
  typeof observerCanonicalItemSchema
>

export const observerTurnContextSchema = z.object({
  turn_id: z.string(), // UUID
  ordinal: z.number(),
  items: z.array(observerCanonicalItemSchema),
})
export type ObserverTurnContext = z.infer<typeof observerTurnContextSchema>

// ============================================================================
// Session transcript (flattened conversation stream)
// ============================================================================

export const observerOversizedUnitSchema = z.object({
  kind: z.string(),
  call_ids: z.array(z.string()).optional(),
  logical_bytes: z.number(),
})
export type ObserverOversizedUnit = z.infer<
  typeof observerOversizedUnitSchema
>

export const observerGapInfoSchema = z.object({
  position: z.string(),
  reason: z.string(),
  omitted_items: z.number(),
  logical_bytes: z.number(),
  source_truncated: z.boolean().optional(),
  oversized_units: z.array(observerOversizedUnitSchema).optional(),
})
export type ObserverGapInfo = z.infer<typeof observerGapInfoSchema>

export const observerTranscriptMessageSchema = z.object({
  turn_id: z.string(), // UUID
  turn_seq: z.number(),
  seq: z.number(),
  kind: z.string(),
  role: z.string().optional(),
  content: z.array(observerCanonicalPartSchema).optional(),
  gap: observerGapInfoSchema.optional(),
  logical_bytes: z.number(),
  hmac: z.string(),
  truncated: z.boolean().optional(),
})
export type ObserverTranscriptMessage = z.infer<
  typeof observerTranscriptMessageSchema
>

export const observerTranscriptMetaSchema = z.object({
  prev_cursor: z.number(),
  has_older: z.boolean(),
})
export type ObserverTranscriptMeta = z.infer<
  typeof observerTranscriptMetaSchema
>

export const observerTranscriptPageSchema = z.object({
  page_size: z.number(),
  items: z.array(observerTranscriptMessageSchema),
  meta: observerTranscriptMetaSchema,
})
export type ObserverTranscriptPage = z.infer<
  typeof observerTranscriptPageSchema
>

// ============================================================================
// Degraded envelope & response wrapper
// ============================================================================

export const observerDegradedSchema = z.object({
  degraded: z.literal(true),
  reason: z.enum(['timeout', 'unavailable']),
  message: z.string(),
})
export type ObserverDegraded = z.infer<typeof observerDegradedSchema>

/** Type guard for the degraded envelope: HTTP 200, `data.degraded === true`. */
export function isObserverDegraded(data: unknown): data is ObserverDegraded {
  return (
    typeof data === 'object' &&
    data !== null &&
    (data as { degraded?: unknown }).degraded === true
  )
}

/** Common envelope of every relay-observer endpoint (`success` is true on
 * both healthy and degraded responses; real errors are HTTP 4xx/5xx). */
export interface ObserverResponse<T> {
  success: boolean
  message?: string
  data?: T | ObserverDegraded
}

/** Parse one observer response at the network boundary. The feature used to
 * export schemas but only cast Axios data at compile time, so a malformed or
 * drifted backend payload could reach React components and fail far from the
 * request that caused it. Keeping validation here turns contract drift into a
 * normal query error handled by the existing error states. */
export function parseObserverResponse<T>(
  dataSchema: z.ZodType<T>,
  value: unknown
): ObserverResponse<T> {
  return z
    .object({
      success: z.boolean(),
      message: z.string().optional(),
      data: z.union([dataSchema, observerDegradedSchema]).optional(),
    })
    .parse(value)
}
