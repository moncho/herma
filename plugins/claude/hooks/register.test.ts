import { expect, mock, test } from 'claude-code/testing'

const options = { herma: '/herma/bin/herma', credentials: '/herma/.herma/credentials.json', identity: 'local-agent', url: 'http://127.0.0.1:9999' }
const base = ['/herma/bin/herma', '--url', 'http://127.0.0.1:9999', '--credentials', '/herma/.herma/credentials.json', '--identity', 'local-agent']
const cleared = { HERMA_TOKEN: '', HERMA_IDENTITY: '', HERMA_CREDENTIALS: '', HERMA_SOCKET: '', HERMA_URL: '' }
const SUBCOMMANDS = ['status', 'recall', 'get']

// sub names the herma subcommand an argv runs.
const sub = (argv: readonly string[]) => argv.find(a => SUBCOMMANDS.includes(a)) ?? ''

type Call = { argv: string[]; env?: Record<string, string>; timeoutMs?: number }
const bound = JSON.stringify({ bound: true, role: 'agent', review: { total: 1 }, backup: { enabled: false } })
const start = { cwd: '/work', surface: null, isInteractive: false }

// Answers what the engine does beneath the plugin besides herma: the session's
// start, user settings (holding `user` as the plugin's options), the status
// line, toasts, tool registration and the clock.
type Seen = { statuses?: Array<string | undefined>; registered?: string[]; failing?: 'tool.register' | 'ui' }

function engine(on: any, seen: Seen = {}, user: object | null = options) {
  on('session.start', (_$: unknown, e: { cwd: string }) => ({ cwd: e.cwd }))
  on('settings.read', (_$: unknown, e: { source?: string }) =>
    ({ value: e.source === 'user' && user ? { pluginConfigs: { herma: { options: user } } } : {} }))
  on('ui.status', (_$: unknown, e: { text: string | undefined }) => {
    seen.statuses?.push(e.text)
    if (seen.failing === 'ui') throw new Error('no status line')
    return { value: undefined }
  })
  on('ui.toast', () => {
    if (seen.failing === 'ui') throw new Error('no toasts')
    return { value: undefined }
  })
  on('tool.register', (_$: unknown, e: { name: string }) => {
    if (seen.failing === 'tool.register') throw new Error('registration refused')
    seen.registered?.push(e.name)
    return { value: { tool: `mcp__herma__${e.name}` } }
  })
  return mock.clock(on)
}

// Each test answers process.run from beneath the plugin, as the engine would;
// an op's hook answers with { value }.
function fakeHerma(on: any, answers: Record<string, { exitCode: number; stdout: string; stderr?: string }>, calls: string[][], runs: Call[] = []) {
  on('process.run', (_$: unknown, e: { argv: readonly string[]; init?: { env?: Record<string, string>; timeoutMs?: number } }) => {
    calls.push([...e.argv])
    runs.push({ argv: [...e.argv], env: e.init?.env, timeoutMs: e.init?.timeoutMs })
    const answer = answers[sub(e.argv)] ?? { exitCode: 1, stdout: '', stderr: 'herma: unexpected command' }
    return { value: { exitCode: answer.exitCode, stdout: answer.stdout, stderr: answer.stderr ?? '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
}

test('bound session shows status and serves recall', { options }, async ($: any, on: any) => {
  const calls: string[][] = []
  const runs: Call[] = []
  const statuses: Array<string | undefined> = []
  fakeHerma(on, { status: { exitCode: 0, stdout: bound }, recall: { exitCode: 0, stdout: '{"results":[]}' } }, calls, runs)
  engine(on, { statuses })
  await $.session.start(start)
  expect(statuses.at(-1)).toBe('herma ✓ · 1 to review · backup off')
  const result = await $.tool.call({ tool: 'mcp__herma__recall', query: 'pruning' })
  expect(result.result).toBe('{"results":[]}')
  expect(calls.find(c => sub(c) === 'recall')).toEqual([...base, 'recall', 'pruning', '--limit', '8'])
  expect(calls.find(c => sub(c) === 'status')).toEqual([...base, 'status'])
  // herma runs on these options alone, never on HERMA_* variables it inherits.
  for (const run of runs) expect(run.env).toEqual(cleared)
})

test('unbound session with a server registers no tools but shows the status', { options }, async ($: any, on: any) => {
  const calls: string[][] = []
  const registered: string[] = []
  const statuses: Array<string | undefined> = []
  fakeHerma(on, { status: { exitCode: 0, stdout: JSON.stringify({ bound: false, server: 'ok', identity: 'claude-agent', role: 'agent', review: { total: 1 }, backup: { enabled: false } }) } }, calls)
  engine(on, { statuses, registered })
  await $.session.start(start)
  expect(calls.map(c => sub(c))).toContain('status')
  expect(registered).toEqual([])
  expect(statuses.at(-1)).toBe('herma ✓ · 1 to review · backup off')
})

test('a bare unbound status from older herma registers no tools and shows nothing', { options }, async ($: any, on: any) => {
  const calls: string[][] = []
  const registered: string[] = []
  const statuses: Array<string | undefined> = []
  fakeHerma(on, { status: { exitCode: 0, stdout: '{"bound": false}' } }, calls)
  engine(on, { statuses, registered })
  await $.session.start(start)
  expect(registered).toEqual([])
  expect(statuses.at(-1)).toBe(undefined)
})

test('tool errors carry the CLI message and bad input runs nothing', { options }, async ($: any, on: any) => {
  const calls: string[][] = []
  fakeHerma(on, { status: { exitCode: 0, stdout: bound }, get: { exitCode: 1, stdout: '', stderr: 'herma: record not found\n' } }, calls)
  engine(on)
  await $.session.start(start)
  const missing = await $.tool.call({ tool: 'mcp__herma__get', id: 'rec_' + '0'.repeat(32) })
  expect(missing.isError).toBe(true)
  expect(String(missing.result)).toBe('record not found')
  const before = calls.length
  const bad = await $.tool.call({ tool: 'mcp__herma__get', id: 'nope' })
  expect(bad.isError).toBe(true)
  expect(calls.filter(c => sub(c) === 'get').length).toBe(calls.slice(0, before).filter(c => sub(c) === 'get').length)
})

test('a herma that cannot run answers as a tool error and refreshes the status', { options }, async ($: any, on: any) => {
  const calls: string[][] = []
  on('process.run', (_$: unknown, e: { argv: readonly string[] }) => {
    calls.push([...e.argv])
    if (sub(e.argv) === 'get') throw new Error('spawn /herma/bin/herma ENOENT')
    return { value: { exitCode: 0, stdout: bound, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  const clock = engine(on)
  await $.session.start(start)
  const failed = await $.tool.call({ tool: 'mcp__herma__get', id: 'rec_' + '0'.repeat(32) })
  await clock.settle()
  expect(failed.isError).toBe(true)
  expect(String(failed.result).length).toBeGreaterThan(0)
  const subs = calls.map(c => sub(c))
  expect(subs.lastIndexOf('status')).toBeGreaterThan(subs.indexOf('get'))
})

const REFUSED = 'herma ✗ herma options must come from user settings'
const refusals: Array<[string, object, object | null]> = [
  ['options that differ from user settings', options, { ...options, credentials: '/elsewhere/credentials.json' }],
  ['options absent from user settings', options, null],
  ['a relative herma path', { ...options, herma: 'bin/herma' }, { ...options, herma: 'bin/herma' }],
  ['a relative credentials path', { ...options, credentials: '.herma/credentials.json' }, { ...options, credentials: '.herma/credentials.json' }],
]
for (const [name, given, user] of refusals) {
  test(`${name} run no herma and register no tools`, { options: given }, async ($: any, on: any) => {
    const calls: string[][] = []
    const registered: string[] = []
    const statuses: Array<string | undefined> = []
    fakeHerma(on, { status: { exitCode: 0, stdout: bound } }, calls)
    const clock = engine(on, { statuses, registered }, user)
    await $.session.start(start)
    await clock.advance(120_000)
    expect(statuses.at(-1)).toBe(REFUSED)
    expect(registered).toEqual([])
    expect(calls).toEqual([])
  })
}

test('identity left at its default in user settings is accepted', { options }, async ($: any, on: any) => {
  const registered: string[] = []
  fakeHerma(on, { status: { exitCode: 0, stdout: bound } }, [])
  const { identity: _, ...user } = options
  engine(on, { registered }, user)
  await $.session.start(start)
  expect(registered).toEqual(['recall', 'get'])
})

test('a reviewer identity registers no tools', { options }, async ($: any, on: any) => {
  const registered: string[] = []
  const statuses: Array<string | undefined> = []
  const reviewer = JSON.stringify({ bound: true, role: 'reviewer', review: { total: 3 }, backup: { enabled: false } })
  fakeHerma(on, { status: { exitCode: 0, stdout: reviewer } }, [])
  engine(on, { statuses, registered })
  await $.session.start(start)
  expect(registered).toEqual([])
  expect(statuses.at(-1)).toBe('herma ✗ reviewer identity refused')
})

const statusRuns = (runs: Call[]) => runs.filter(r => sub(r.argv) === 'status')

test('an unbound session starts no refresh timer', { options }, async ($: any, on: any) => {
  const runs: Call[] = []
  fakeHerma(on, { status: { exitCode: 0, stdout: '{"bound": false}' } }, [], runs)
  const clock = engine(on)
  await $.session.start(start)
  await clock.advance(180_000)
  expect(statusRuns(runs).length).toBe(1)
})

test('the first reading waits 5 s and each refresh 10 s', { options }, async ($: any, on: any) => {
  const runs: Call[] = []
  fakeHerma(on, { status: { exitCode: 0, stdout: bound } }, [], runs)
  const clock = engine(on)
  await $.session.start(start)
  await clock.advance(120_000)
  expect(statusRuns(runs).map(r => r.timeoutMs)).toEqual([5_000, 10_000, 10_000])
})

test('the refresh timer runs even when tool registration fails', { options }, async ($: any, on: any) => {
  const runs: Call[] = []
  fakeHerma(on, { status: { exitCode: 0, stdout: bound } }, [], runs)
  const clock = engine(on, { failing: 'tool.register' })
  await $.session.start(start).catch(() => {})
  await clock.advance(60_000)
  expect(statusRuns(runs).length).toBe(2)
})

test('after a reload a tool call restarts the refresh loop', { options }, async ($: any, on: any) => {
  const runs: Call[] = []
  fakeHerma(on, { status: { exitCode: 0, stdout: bound }, recall: { exitCode: 0, stdout: '{"results":[]}' } }, [], runs)
  const clock = engine(on)
  // No session.start: the module was reloaded mid-session.
  const result = await $.tool.call({ tool: 'mcp__herma__recall', query: 'pruning' })
  expect(result.result).toBe('{"results":[]}')
  await clock.settle()
  const before = statusRuns(runs).length
  await clock.advance(60_000)
  expect(statusRuns(runs).length).toBe(before + 1)
  await $.tool.call({ tool: 'mcp__herma__recall', query: 'again' })
  await clock.advance(60_000)
  // One loop, not one per call.
  expect(statusRuns(runs).length).toBe(before + 3)
})

test('after a reload a tool call still refuses options outside user settings', { options }, async ($: any, on: any) => {
  const calls: string[][] = []
  const statuses: Array<string | undefined> = []
  fakeHerma(on, { recall: { exitCode: 0, stdout: '{"results":[]}' } }, calls)
  const clock = engine(on, { statuses }, { ...options, herma: '/other/herma' })
  const result = await $.tool.call({ tool: 'mcp__herma__recall', query: 'pruning' })
  await clock.advance(60_000)
  expect(result.isError).toBe(true)
  expect(String(result.result)).toBe('herma options must come from user settings')
  expect(statuses.at(-1)).toBe(REFUSED)
  expect(calls).toEqual([])
})

test('a failing status line or toast does not break the session', { options }, async ($: any, on: any) => {
  const registered: string[] = []
  let healthy = true
  on('process.run', (_$: unknown, e: { argv: readonly string[] }) => {
    const ok = sub(e.argv) !== 'status' || healthy
    const stdout = sub(e.argv) === 'recall' ? '{"results":[]}' : bound
    return { value: { exitCode: ok ? 0 : 1, stdout: ok ? stdout : '', stderr: ok ? '' : 'herma: down', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  const clock = engine(on, { registered, failing: 'ui' })
  await $.session.start(start)
  expect(registered).toEqual(['recall', 'get'])
  healthy = false
  const result = await $.tool.call({ tool: 'mcp__herma__recall', query: 'pruning' })
  await clock.advance(60_000)
  expect(result.result).toBe('{"results":[]}')
})
