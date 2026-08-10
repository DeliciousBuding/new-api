import 'flag-icons/css/flag-icons.min.css'
import { Globe } from 'lucide-react'

import { StatusBadge } from '@/components/status-badge'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'

import type { GeoInfo } from '../types'

function flagClass(countryCode?: string): string | null {
  if (!countryCode || !/^[A-Z]{2}$/.test(countryCode)) return null
  return `fi fi-${countryCode.toLowerCase()}`
}

interface IpGeoBadgeProps {
  ip: string
  geo?: GeoInfo
  className?: string
  /** Compact text row for the details dialog: flag + IP + locality, no chip. */
  compact?: boolean
}

// Renders the client IP as a compact chip: a country flag (when the backend
// resolved one) plus the raw address. Hovering a chip with locality data
// shows city/country and ASN details. The IP is an audit element and stays
// visible to admins — it does not join the sensitive-value masking that
// applies to usernames, channel names, and token names.
export function IpGeoBadge({ ip, geo, className, compact }: IpGeoBadgeProps) {
  const flag = flagClass(geo?.country_code)
  const hasGeoDetails = Boolean(geo && (geo.city || geo.country || geo.asn))

  const flagEl = flag && (
    <span
      className={cn(
        'h-[14px] w-[18px] shrink-0 rounded-[2px] text-[14px]',
        flag
      )}
      aria-hidden='true'
    />
  )

  if (compact) {
    return (
      <span
        className={cn('inline-flex items-center gap-1.5 text-xs', className)}
      >
        {flagEl}
        {!flag && (
          <Globe
            className='text-muted-foreground size-3 shrink-0'
            aria-hidden='true'
          />
        )}
        <span className='font-mono whitespace-nowrap'>{ip}</span>
        {geo?.city && <span className='text-muted-foreground'>{geo.city}</span>}
        {geo?.country && (
          <span className='text-muted-foreground'>{geo.country}</span>
        )}
        {geo?.asn && <span className='text-muted-foreground'>AS{geo.asn}</span>}
      </span>
    )
  }

  const chip = (
    <StatusBadge
      size='sm'
      copyText={ip}
      className={cn(
        'border-border/60 bg-muted/30 h-6 max-w-none gap-1.5 rounded-md border px-2 font-mono [font-family:var(--font-body)]',
        className
      )}
    >
      {flagEl}
      {!flag && (
        <Globe
          className='text-muted-foreground size-3 shrink-0'
          aria-hidden='true'
        />
      )}
      <span className='whitespace-nowrap'>{ip}</span>
    </StatusBadge>
  )

  if (!hasGeoDetails) {
    return chip
  }

  // Hover: quick locality glance (city first, then country/ASN) — mirrors the
  // Sub2API usage-card pattern of a compact summary with details one level
  // deeper. Click still opens the full popover below.
  const tooltipLine = [
    geo?.city,
    geo?.country,
    geo?.asn ? `AS${geo.asn}` : null,
  ]
    .filter(Boolean)
    .join(' · ')

  return (
    <TooltipProvider delay={300}>
      <Tooltip>
        <TooltipTrigger
          render={
            <button type='button' className='inline-flex items-center gap-1' />
          }
        >
          {chip}
        </TooltipTrigger>
        <TooltipContent>{tooltipLine}</TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}
