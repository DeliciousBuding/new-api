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
import type { SVGProps } from 'react'

import { cn } from '@/lib/utils'

export function IconTokenDance({
  className,
  ...props
}: SVGProps<SVGSVGElement>) {
  return (
    <svg
      role='img'
      viewBox='0 0 160 160'
      xmlns='http://www.w3.org/2000/svg'
      className={cn(className)}
      {...props}
    >
      <title>TokenDance</title>
      <rect width='160' height='160' rx='32' fill='#0071BC' />
      <rect x='39' y='20' width='20' height='80' rx='10' fill='#FFFFFF' />
      <rect x='70' y='40' width='20' height='80' rx='10' fill='#FFFFFF' />
      <rect x='101' y='60' width='20' height='80' rx='10' fill='#FFFFFF' />
    </svg>
  )
}
