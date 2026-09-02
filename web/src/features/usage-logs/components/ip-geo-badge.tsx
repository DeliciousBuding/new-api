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

// IPv6 addresses can reach 39 characters (e.g. 2001:0db8:85a3:0000:0000:8a2e:0370:7334)
// and would stretch the log table's IP column. The chip renders a compact,
// group-aware abbreviation (network prefix + ellipsis + host suffix); the full
// address stays available through the chip's copy action and the hover tooltip.
const IP_ABBREVIATION_MAX = 18

function abbreviateIp(ip: string): string {
  if (ip.length <= IP_ABBREVIATION_MAX) return ip
  if (ip.includes(':')) {
    const groups = ip.split(':')
    if (groups.length > 4) {
      return `${groups.slice(0, 2).join(':')}…${groups.slice(-2).join(':')}`
    }
  }
  const keep = Math.floor((IP_ABBREVIATION_MAX - 1) / 2)
  return `${ip.slice(0, keep)}…${ip.slice(-keep)}`
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
// Locality display parts: "Province·City" first (ip2region overlay), then
// ISP, country and ASN. Empty parts are dropped.
function localityParts(geo?: GeoInfo): string[] {
  if (!geo) return []
  const locality = [geo.province, geo.city].filter(Boolean).join('·')
  return [locality, geo.isp, geo.country, geo.asn ? `AS${geo.asn}` : ''].filter(
    Boolean
  )
}

export function IpGeoBadge({ ip, geo, className, compact }: IpGeoBadgeProps) {
  const flag = flagClass(geo?.country_code)
  const parts = localityParts(geo)
  const hasGeoDetails = parts.length > 0

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
        {parts.map((part) => (
          <span key={part} className='text-muted-foreground'>
            {part}
          </span>
        ))}
      </span>
    )
  }

  const displayIp = abbreviateIp(ip)
  const isAbbreviated = displayIp !== ip

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
      <span className='whitespace-nowrap'>{displayIp}</span>
    </StatusBadge>
  )

  if (!hasGeoDetails) {
    return chip
  }

  // Hover: quick locality glance (city first, then country/ASN). When the chip
  // abbreviates a long address, lead with the full IP so the audit value stays
  // one hover away.
  const localityLine = parts.join(' · ')

  const tooltipContent = isAbbreviated ? (
    <div className='flex flex-col gap-0.5'>
      <span className='font-mono'>{ip}</span>
      {localityLine && (
        <span className='text-muted-foreground'>{localityLine}</span>
      )}
    </div>
  ) : (
    localityLine
  )

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
        <TooltipContent>{tooltipContent}</TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}
