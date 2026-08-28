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
import type { TFunction } from 'i18next'
import { z } from 'zod'

import {
  INVITATION_VALIDATION,
  getInvitationFormErrorMessages,
} from '../constants'
import type { InvitationCodeFormData, InvitationCode } from '../types'

// ============================================================================
// Form Schema (use getInvitationFormSchema(t) in components for i18n messages)
// ============================================================================

export function getInvitationFormSchema(t: TFunction) {
  const msg = getInvitationFormErrorMessages(t)
  return z.object({
    name: z
      .string()
      .min(INVITATION_VALIDATION.NAME_MIN_LENGTH, msg.NAME_LENGTH_INVALID)
      .max(INVITATION_VALIDATION.NAME_MAX_LENGTH, msg.NAME_LENGTH_INVALID),
    max_uses: z
      .number()
      .int(msg.MAX_USES_INVALID)
      .min(INVITATION_VALIDATION.MAX_USES_MIN, msg.MAX_USES_INVALID),
    expired_time: z.date().optional(),
    count: z
      .number()
      .min(INVITATION_VALIDATION.COUNT_MIN, msg.COUNT_INVALID)
      .max(INVITATION_VALIDATION.COUNT_MAX, msg.COUNT_INVALID)
      .optional(),
  })
}

export type InvitationFormValues = {
  name: string
  max_uses: number
  expired_time?: Date
  count?: number
}

// ============================================================================
// Form Defaults
// ============================================================================

export const INVITATION_FORM_DEFAULT_VALUES: InvitationFormValues = {
  name: '',
  max_uses: 1,
  expired_time: undefined,
  count: 1,
}

// ============================================================================
// Form Data Transformation
// ============================================================================

/**
 * Transform form data to API payload
 */
export function transformFormDataToPayload(
  data: InvitationFormValues
): InvitationCodeFormData {
  return {
    name: data.name,
    max_uses: data.max_uses,
    expired_time: data.expired_time
      ? Math.floor(data.expired_time.getTime() / 1000)
      : 0,
    count: data.count || 1,
  }
}

/**
 * Transform invitation code data to form defaults
 */
export function transformInvitationCodeToFormDefaults(
  invitationCode: InvitationCode
): InvitationFormValues {
  return {
    name: invitationCode.name,
    max_uses: invitationCode.max_uses,
    expired_time:
      invitationCode.expired_time > 0
        ? new Date(invitationCode.expired_time * 1000)
        : undefined,
    count: 1,
  }
}
