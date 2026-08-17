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
import { render } from '@testing-library/react'
import i18next from 'i18next'
import { beforeAll, describe, expect, test } from 'vitest'

import { IpGeoBadge } from '../ip-geo-badge'

beforeAll(() => {
  i18next.addResourceBundle('en', 'translation', {
    'Country:': 'Country:',
    'City:': 'City:',
    'ASN:': 'ASN:',
  })
})

function renderBadge(props: {
  ip: string
  geo?: {
    country_code?: string
    country?: string
    city?: string
    asn?: number
    asn_org?: string
  }
}) {
  return render(<IpGeoBadge {...props} />)
}

describe('IpGeoBadge', () => {
  test('shows the raw IP address (no masking for admins)', () => {
    const { container } = renderBadge({ ip: '1.2.3.4' })
    expect(container.textContent).toContain('1.2.3.4')
  })

  test('renders a country flag when country_code is present', () => {
    const { container } = renderBadge({
      ip: '1.2.3.4',
      geo: { country_code: 'CN', country: 'China' },
    })
    expect(container.querySelector('.fi-cn')).not.toBeNull()
  })

  test('falls back to a globe icon without country_code', () => {
    const { container } = renderBadge({ ip: '1.2.3.4' })
    expect(container.querySelector('svg')).not.toBeNull()
  })

  test('ignores malformed country codes', () => {
    const { container } = renderBadge({
      ip: '1.2.3.4',
      geo: { country_code: 'cn', country: 'China' },
    })
    expect(container.querySelector('[class*="fi-"]')).toBeNull()
  })

  test('exposes locality details via popover trigger when geo is resolved', () => {
    const { container } = renderBadge({
      ip: '1.2.3.4',
      geo: {
        country_code: 'CN',
        country: 'China',
        city: 'Shenzhen',
        asn: 4134,
        asn_org: 'Chinanet',
      },
    })
    expect(container.querySelector('button')).not.toBeNull()
  })

  test('stays a plain chip without geo details', () => {
    const { container } = renderBadge({ ip: '1.2.3.4' })
    expect(container.querySelector('button')).toBeNull()
  })

  test('abbreviates long IPv6 addresses to keep the chip compact', () => {
    const longIpv6 = '2001:0db8:85a3:0000:0000:8a2e:0370:7334'
    const { container } = renderBadge({ ip: longIpv6 })
    const text = container.textContent ?? ''
    // The chip keeps the network prefix and host suffix, joined by an ellipsis.
    expect(text).toContain('…')
    expect(text).toContain('2001:0db8')
    expect(text).toContain('0370:7334')
    // The dropped middle groups are not rendered inline.
    expect(text).not.toContain('85a3:0000:0000:8a2e')
  })

  test('leaves short IPv4 addresses untouched', () => {
    const { container } = renderBadge({ ip: '192.168.1.1' })
    const text = container.textContent ?? ''
    expect(text).toContain('192.168.1.1')
    expect(text).not.toContain('…')
  })

  test('keeps the geo hover trigger when the IP is abbreviated', () => {
    const { container } = renderBadge({
      ip: '2001:0db8:85a3:0000:0000:8a2e:0370:7334',
      geo: { country_code: 'CN', country: 'China', city: 'Shenzhen', asn: 4134 },
    })
    expect(container.querySelector('button')).not.toBeNull()
    expect(container.textContent ?? '').toContain('…')
  })
})
