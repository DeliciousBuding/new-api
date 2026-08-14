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
*/
import assert from 'node:assert/strict'
import { after, describe, test } from 'node:test'

import { Window } from 'happy-dom'

const domWindow = new Window()
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'CustomEvent',
  'MutationObserver',
  'requestAnimationFrame',
  'cancelAnimationFrame',
  'getComputedStyle',
  'matchMedia',
  'customElements',
] as const

for (const key of domGlobals) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

const { act } = await import('react')
const { createRoot } = await import('react-dom/client')

const { ClientProfileBadge } = await import('../client-profile-badge')

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

function renderBadge(profile: string) {
  const host = document.createElement('div')
  document.body.appendChild(host)
  const root = createRoot(host)
  act(() => {
    root.render(<ClientProfileBadge profile={profile as never} />)
  })
  return { host, root }
}

after(() => {
  document.body.innerHTML = ''
})

describe('ClientProfileBadge', () => {
  for (const profile of [
    'codex_cli',
    'codex_desktop',
    'codex_app',
    'claude_cli',
    'claude_desktop',
    'claude_desktop_3p',
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
  ]) {
    test(`renders ${profile} without crashing`, () => {
      const { host, root } = renderBadge(profile)
      const text = host.textContent ?? ''
      assert.ok(text.trim().length > 0, 'badge should render a label')
      root.unmount()
    })
  }

  test('brand profiles render a brand icon (codex -> Codex brand)', () => {
    const { host, root } = renderBadge('codex_cli')
    assert.ok(host.querySelector('svg'), 'brand icon should render as svg')
    root.unmount()
  })

  test('fallback profiles render a lucide glyph (chat)', () => {
    const { host, root } = renderBadge('chat')
    assert.ok(host.querySelector('svg'), 'fallback icon should render as svg')
    root.unmount()
  })

  test('chip carries the machine profile id as title', () => {
    const { host, root } = renderBadge('claude_sdk')
    const chip = host.querySelector('[title]')
    assert.ok(chip, 'chip should carry a title')
    assert.equal(chip?.getAttribute('title'), 'client_profile: claude_sdk')
    root.unmount()
  })
})
