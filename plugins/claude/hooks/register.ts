import type { EngineInterface, Register, Timer, ToolCallResult } from 'claude-code'
import { firstLine, isUsable, parseStatus, statusText, transition, type HermaStatus } from './status'
import { GET_TOOL, RECALL_TOOL, getArgs, recallArgs } from './tools'

type Options = { herma: string; credentials: string; identity: string; url: string }

// herma reads these before its flags' defaults; cleared, it runs on the options alone.
const CLEARED_ENV = { HERMA_TOKEN: '', HERMA_IDENTITY: '', HERMA_CREDENTIALS: '', HERMA_SOCKET: '', HERMA_URL: '' }

// What the hooks share: the herma argv prefix, the last status shown and the
// refresh timer running in this module environment.
type State = { base: string[]; last: HermaStatus | undefined; timer?: Timer }

const REFRESH_MS = 60_000
const FIRST_TIMEOUT_MS = 5_000
const REFRESH_TIMEOUT_MS = 10_000

const REFUSED = 'herma options must come from user settings'
const DEFAULTS: Partial<Options> = { identity: 'local-agent' }
const KEYS = ['herma', 'credentials', 'identity', 'url'] as const

// fromUserSettings says whether the options the module received are the ones
// user settings hold, with absolute herma and credentials paths: project and local
// settings must not choose the binary herma runs or the credentials it reads.
async function fromUserSettings($: EngineInterface, o: Options): Promise<boolean> {
  if (!o.herma?.startsWith('/') || !o.credentials?.startsWith('/')) return false
  let user: Record<string, unknown>
  try {
    const settings = (await $.settings.read({ source: 'user' })) as Record<string, any>
    user = settings?.pluginConfigs?.herma?.options ?? {}
  } catch {
    return false
  }
  return KEYS.every(k => (user[k] ?? DEFAULTS[k]) === o[k])
}

// The status line and toasts are best effort: a failure there must not fail
// the hook that refreshed.
function show($: EngineInterface, text: string | undefined): void {
  try { $.ui.status(text) } catch { /* nothing to show it on */ }
}

function notify($: EngineInterface, text: string): void {
  try { $.ui.toast(text) } catch { /* nothing to show it on */ }
}

async function refresh($: EngineInterface, state: State, timeoutMs = REFRESH_TIMEOUT_MS): Promise<HermaStatus> {
  let next: HermaStatus
  try {
    next = parseStatus(await $.process.run([...state.base, 'status'], { env: CLEARED_ENV, timeoutMs }))
  } catch (err) {
    next = { kind: 'failed', reason: firstLine(String(err)) || 'herma could not run' }
  }
  const toast = transition(state.last, next)
  if (toast) notify($, toast)
  show($, statusText(next))
  state.last = next
  return next
}

// keepFresh starts the refresh loop unless one already runs in this module
// environment; a reload cancels the old environment's timer.
function keepFresh($: EngineInterface, state: State): void {
  if (state.timer) return
  state.timer = $.clock.every(REFRESH_MS, () => { refresh($, state).catch(() => {}) })
}

function stopRefreshing(state: State): void {
  state.timer?.cancel()
  state.timer = undefined
}

async function answer($: EngineInterface, state: State, args: { argv: string[] } | { error: string }): Promise<ToolCallResult> {
  if ('error' in args) return { result: args.error, isError: true }
  let ran
  try {
    ran = await $.process.run([...state.base, ...args.argv], { env: CLEARED_ENV, timeoutMs: 15_000 })
  } catch (err) {
    // herma could not start or overran the timeout.
    refresh($, state).catch(() => {})
    return { result: firstLine(String(err), 500) || 'herma could not run', isError: true }
  }
  refresh($, state).catch(() => {})
  return ran.exitCode === 0
    ? { result: ran.stdout }
    : { result: firstLine(ran.stderr, 500) || `herma exited ${ran.exitCode}`, isError: true }
}

// serve answers a tool call. A reloaded module gets no session.start, so its
// first tool call checks the options and restarts the refresh loop.
async function serve($: EngineInterface, state: State, o: Options, args: { argv: string[] } | { error: string }): Promise<ToolCallResult> {
  if (!state.timer) {
    if (!(await fromUserSettings($, o))) {
      show($, `herma ✗ ${REFUSED}`)
      return { result: REFUSED, isError: true }
    }
    keepFresh($, state)
  }
  return answer($, state, args)
}

export const register: Register = (on, options) => {
  const o = options as unknown as Options
  const state: State = { base: [o.herma, '--url', o.url, '--credentials', o.credentials, '--identity', o.identity], last: undefined }

  on('session.start', async ($, e, next) => {
    const started = await next(e)
    if (!(await fromUserSettings($, o))) {
      stopRefreshing(state)
      show($, `herma ✗ ${REFUSED}`)
      return started
    }
    const status = await refresh($, state, FIRST_TIMEOUT_MS)
    // Outside a bound checkout the plugin stays silent for the session.
    if (status.kind !== 'unbound') keepFresh($, state)
    if (isUsable(status)) {
      await $.tool.register(RECALL_TOOL)
      await $.tool.register(GET_TOOL)
    }
    return started
  })

  on('tool.call', { tool: 'mcp__herma__recall' }, ($, e) => serve($, state, o, recallArgs(e as unknown as Record<string, unknown>)))
  on('tool.call', { tool: 'mcp__herma__get' }, ($, e) => serve($, state, o, getArgs(e as unknown as Record<string, unknown>)))
}
