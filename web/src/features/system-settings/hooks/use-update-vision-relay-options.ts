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
import { useMutation, useQueryClient } from '@tanstack/react-query'
import i18next from 'i18next'
import { toast } from 'sonner'

import { updateVisionRelayOptions } from '../api'
import type { UpdateVisionRelayOptionsRequest } from '../types'

export function useUpdateVisionRelayOptions() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (request: UpdateVisionRelayOptionsRequest) =>
      updateVisionRelayOptions(request),
    onSuccess: (data) => {
      if (data.success) {
        queryClient.invalidateQueries({ queryKey: ['system-options'] })
        toast.success(i18next.t('Settings saved successfully'))
      } else {
        toast.error(data.message || i18next.t('Failed to save settings'))
      }
    },
    onError: (error: Error) => {
      toast.error(error.message || i18next.t('Failed to save settings'))
    },
  })
}
