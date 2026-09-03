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
import { describe, expect, test, vi } from 'vitest'

import { ClientProfileBadge } from '../client-profile-badge'

// @lobehub/icons (behind the fork's lobe-icon loader) uses ESM directory
// imports that Vite's resolver cannot follow. These tests cover badge
// structure and labels, not upstream SVG internals, so stub the loader.
// Vitest hoists vi.mock, so the stub applies to the imports above.
vi.mock('@/lib/lobe-icon', async () => {
  const React = await import('react')
  return {
    getLobeIcon: () =>
      React.createElement('svg', { 'data-mock-lobe-icon': 'true' }),
  }
})

function renderBadge(profile: string) {
  return render(<ClientProfileBadge profile={profile as never} />)
}

describe('ClientProfileBadge', () => {
  for (const profile of [
    'codex_cli',
    'codex_desktop',
    'codex_app',
    'claude_cli',
    'claude_desktop',
    'claude_desktop_3p',
    'claude_vscode',
    'claude_plugin',
    'claude_app',
    'claude_sdk',
    'openai_sdk',
    'gohttp',
    'cliproxyapi',
    'chat',
    'hermes_agent',
    'openclaw',
    'cherry_studio',
    'windsurf',
    'cline',
    'roo_code',
    'cursor',
    'trae',
    'perplexity',
    'http_client',
    'codex_vscode',
    'codex_browser',
    'gemini_cli',
    'gemini_sdk',
    'qoder',
    'gemini_code_assist',
    'openai_agents',
    'semantic_kernel',
    'langchain',
    'llama_index',
    'grok',
    'mcp_sdk',
    'n8n',
    'zapier',
    'make',
    'mistral_sdk',
    'litellm',
    'cohere_sdk',
    'ai_sdk',
    'sub2api',
    'deepseek_harness',
    'deepseek',
    'omp',
  ]) {
    test(`renders ${profile} without crashing`, () => {
      const { container } = renderBadge(profile)
      const text = container.textContent ?? ''
      expect(text.trim().length).toBeGreaterThan(0)
    })
  }

  test('brand profiles render a brand icon (codex -> Codex brand)', () => {
    const { container } = renderBadge('codex_cli')
    expect(container.querySelector('svg')).not.toBeNull()
  })

  test('fallback profiles render a lucide glyph (chat)', () => {
    const { container } = renderBadge('chat')
    expect(container.querySelector('svg')).not.toBeNull()
  })

  test('chip carries the machine profile id as title', () => {
    const { container } = renderBadge('claude_sdk')
    const chip = container.querySelector('[title]')
    expect(chip).not.toBeNull()
    expect(chip?.getAttribute('title')).toBe('client_profile: claude_sdk')
  })
})
