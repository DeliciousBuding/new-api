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
import { ClaudeCode, Codex } from '@lobehub/icons'
import { Link } from '@tanstack/react-router'
import { ArrowRight, BookOpen } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { useStatus } from '@/hooks/use-status'

import { SparkCellularHero } from './spark-cellular-hero'

interface SparkHeroProps {
  isAuthenticated?: boolean
  theme: 'light' | 'dark'
}

// Stylized three-dots indicator representing "More"
const MoreIcon = () => (
  <svg
    className='text-muted-foreground/60 group-hover:text-foreground size-6 shrink-0 transition-colors'
    viewBox='0 0 24 24'
    fill='none'
    xmlns='http://www.w3.org/2000/svg'
  >
    <circle cx='6' cy='12' r='2' fill='currentColor' />
    <circle cx='12' cy='12' r='2' fill='currentColor' />
    <circle cx='18' cy='12' r='2' fill='currentColor' />
  </svg>
)

/**
 * Full-bleed hero: the Spark cellular-growth animation covers the whole
 * viewport while the copy sits in the left column. Rendered when the admin
 * sets HomePageStyle=spark; the original landing (Hero/Stats/Features/...)
 * stays intact for HomePageStyle=default.
 */
export function SparkHero(props: SparkHeroProps) {
  const { t } = useTranslation()
  const { status } = useStatus()
  const docsUrl =
    (status?.docs_link as string | undefined) || 'https://docs.newapi.pro'

  const renderDocsButton = () => {
    const isExternal = docsUrl.startsWith('http')
    if (isExternal) {
      return (
        <Button
          variant='outline'
          className='group border-border/50 hover:border-border hover:bg-muted/50 inline-flex h-11 items-center gap-1.5 rounded-lg px-5 text-sm font-medium'
          render={
            <a href={docsUrl} target='_blank' rel='noopener noreferrer' />
          }
        >
          <BookOpen className='text-muted-foreground/80 group-hover:text-foreground size-4 transition-colors duration-200' />
          <span>{t('Docs')}</span>
        </Button>
      )
    }
    return (
      <Button
        variant='outline'
        className='group border-border/50 hover:border-border hover:bg-muted/50 inline-flex h-11 items-center gap-1.5 rounded-lg px-5 text-sm font-medium'
        render={<Link to={docsUrl} />}
      >
        <BookOpen className='text-muted-foreground/80 group-hover:text-foreground size-4 transition-colors duration-200' />
        <span>{t('Docs')}</span>
      </Button>
    )
  }

  return (
    <section className='relative min-h-svh w-full overflow-hidden'>
      <div className='relative z-10 flex min-h-svh flex-col justify-center'>
        <div className='mx-auto grid w-full max-w-7xl grid-cols-1 items-center gap-y-12 px-6 pt-28 pb-16 md:px-12 lg:grid-cols-2 lg:gap-x-24 lg:px-16 xl:gap-x-36 2xl:gap-x-48'>
          {/* Left Column: badge, slogan, description, actions, supported apps */}
          <div className='max-w-xl'>
            <div
              className='landing-animate-fade-up mb-5 inline-flex items-center gap-1.5 rounded-full border border-blue-500/20 bg-blue-500/5 px-3 py-1.5 text-[11px] font-medium text-blue-600 opacity-0 shadow-xs dark:border-blue-400/20 dark:bg-blue-400/5 dark:text-blue-400'
              style={{ animationDelay: '0ms' }}
            >
              <span className='relative flex size-1.5'>
                <span className='absolute inline-flex h-full w-full animate-ping rounded-full bg-blue-400 opacity-75' />
                <span className='relative inline-flex size-1.5 rounded-full bg-blue-500 dark:bg-blue-400' />
              </span>
              <span>{t('AI Application Infrastructure Foundation')}</span>
            </div>

            <h1 className='landing-animate-fade-up text-[clamp(2.5rem,4.8vw,3.75rem)] leading-[1.1] font-bold tracking-tight opacity-0'>
              {t('One gateway.')}
              <br />
              <span className='bg-gradient-to-r from-blue-400 via-violet-400 to-purple-500 bg-clip-text text-transparent'>
                {t('Every model.')}
              </span>
            </h1>

            <p
              className='landing-animate-fade-up text-muted-foreground/80 mt-5 max-w-lg text-base leading-relaxed opacity-0 md:text-[15px]'
              style={{ animationDelay: '120ms' }}
            >
              {t(
                'One standard protocol to every frontier model. Power production AI, own your digital assets, and reach what is next.'
              )}
            </p>

            <div
              className='landing-animate-fade-up mt-8 flex flex-wrap items-center gap-3 opacity-0'
              style={{ animationDelay: '180ms' }}
            >
              {props.isAuthenticated ? (
                <>
                  <Button
                    className='group h-11 rounded-lg px-5 text-sm font-medium'
                    render={<Link to='/dashboard' />}
                  >
                    {t('Go to Dashboard')}
                    <ArrowRight className='ml-1.5 size-4 transition-transform duration-200 group-hover:translate-x-0.5' />
                  </Button>
                  {renderDocsButton()}
                </>
              ) : (
                <>
                  <Button
                    className='group h-11 rounded-lg px-5 text-sm font-medium'
                    render={<Link to='/sign-up' />}
                  >
                    {t('Get Started')}
                    <ArrowRight className='ml-1.5 size-4 transition-transform duration-200 group-hover:translate-x-0.5' />
                  </Button>
                  <Button
                    variant='outline'
                    className='border-border/50 hover:border-border hover:bg-muted/50 h-11 rounded-lg px-5 text-sm font-medium'
                    render={<Link to='/pricing' />}
                  >
                    {t('View Pricing')}
                  </Button>
                  {renderDocsButton()}
                </>
              )}
            </div>

            {/* Supported Applications */}
            <div
              className='landing-animate-fade-up mt-10 w-full opacity-0'
              style={{ animationDelay: '240ms' }}
            >
              <div className='mb-4 flex flex-col gap-1'>
                <span className='text-muted-foreground/50 text-[10px] font-bold tracking-[0.15em] uppercase'>
                  {t('Native integrations')}
                </span>
                <p className='text-muted-foreground/60 text-xs leading-relaxed'>
                  {t(
                    'Claude Code, Codex and the wider AI toolchain connect in one click - no glue code.'
                  )}
                </p>
              </div>
              <div className='flex flex-wrap items-center gap-3'>
                <a
                  href='https://claude.com/claude-code'
                  target='_blank'
                  rel='noopener noreferrer'
                  className='group border-border/40 bg-muted/15 text-foreground/80 hover:border-border hover:bg-muted/30 hover:text-foreground flex items-center gap-3 rounded-full border px-5 py-2.5 text-sm font-medium shadow-[0_1px_2.5px_rgba(0,0,0,0.01)] backdrop-blur-xs transition-all duration-300 hover:scale-[1.02]'
                >
                  <ClaudeCode.Color size={24} className='shrink-0' />
                  <span>Claude Code</span>
                </a>

                <a
                  href='https://openai.com/codex'
                  target='_blank'
                  rel='noopener noreferrer'
                  className='group border-border/40 bg-muted/15 text-foreground/80 hover:border-border hover:bg-muted/30 hover:text-foreground flex items-center gap-3 rounded-full border px-5 py-2.5 text-sm font-medium shadow-[0_1px_2.5px_rgba(0,0,0,0.01)] backdrop-blur-xs transition-all duration-300 hover:scale-[1.02]'
                >
                  <Codex.Color size={24} className='shrink-0' />
                  <span>Codex</span>
                </a>

                <div className='group border-border/40 bg-muted/15 text-foreground/55 hover:border-border hover:bg-muted/30 hover:text-foreground flex cursor-default items-center gap-2.5 rounded-full border px-5 py-2.5 text-sm font-medium shadow-[0_1px_2.5px_rgba(0,0,0,0.01)] backdrop-blur-xs transition-all duration-300 hover:scale-[1.02]'>
                  <MoreIcon />
                  <span>{t('More Apps')}</span>
                </div>
              </div>
            </div>
          </div>

          {/* Growth art column: contained + centred in its own block so the
              copy and the artwork read as one balanced, centred pair */}
          <div className='relative hidden h-[min(62vh,540px)] w-full lg:block'>
            <SparkCellularHero theme={props.theme} />
          </div>
        </div>

        {/* Mobile & tablet: the growth animation sits below the copy as a
            standalone block so it never collides with the buttons or apps */}
        <div className='mx-auto mt-12 h-[46vh] min-h-[300px] w-full max-w-6xl px-6 pb-12 md:px-12 lg:hidden'>
          <SparkCellularHero theme={props.theme} />
        </div>
      </div>
    </section>
  )
}
