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
import i18next from 'i18next'
import { useEffect, useState } from 'react'
import { toast } from 'sonner'

import { isHttpUrl } from '@/lib/content-format'

import { getHomePageContent } from '../api'
import type { HomePageContentResult, HomePageStyle } from '../types'

const STORAGE_KEY = 'home_page_content'
const STYLE_STORAGE_KEY = 'home_page_style'

function normalizeHomePageStyle(value: unknown): HomePageStyle {
  return value === 'default' ? 'default' : 'spark'
}

/**
 * Hook to load and manage custom home page content
 * Supports both Markdown/HTML content and iframe URLs
 */
export function useHomePageContent(): HomePageContentResult {
  const [content, setContent] = useState<string>('')
  const [homePageStyle, setHomePageStyle] = useState<HomePageStyle>('spark')
  const [isLoaded, setIsLoaded] = useState(false)

  useEffect(() => {
    let mounted = true

    const loadContent = async () => {
      // Load from localStorage first for immediate display
      const cached = localStorage.getItem(STORAGE_KEY)
      const cachedStyle = localStorage.getItem(STYLE_STORAGE_KEY)
      if (mounted) {
        if (cached) setContent(cached)
        if (cachedStyle) setHomePageStyle(normalizeHomePageStyle(cachedStyle))
      }

      try {
        const response = await getHomePageContent()
        const { success, data, home_page_style } = response

        if (!mounted) return

        if (success) {
          const nextStyle = normalizeHomePageStyle(home_page_style)
          setHomePageStyle(nextStyle)
          localStorage.setItem(STYLE_STORAGE_KEY, nextStyle)
        }

        if (success && data) {
          setContent(data)
          localStorage.setItem(STORAGE_KEY, data)
        } else {
          // Clear content if API returns empty
          setContent('')
          localStorage.removeItem(STORAGE_KEY)
        }
      } catch (error) {
        if (!mounted) return
        // eslint-disable-next-line no-console
        console.error('Failed to load home page content:', error)
        toast.error(i18next.t('Failed to load home page content'))
      } finally {
        if (mounted) {
          setIsLoaded(true)
        }
      }
    }

    loadContent()

    return () => {
      mounted = false
    }
  }, [])

  const isUrl = isHttpUrl(content)

  return { content, homePageStyle, isLoaded, isUrl }
}
