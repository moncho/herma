import { describe, expect, test } from 'claude-code/testing'
import { firstLine, parseStatus, statusText, transition, isUsable, type HermaStatus } from './status'

const okJSON = (review: number, backup: object) =>
  JSON.stringify({ bound: true, project_id: 'rec_x', server: 'ok', identity: 'local-agent', role: 'agent', review: { total: review }, backup })

const unboundJSON = (review: number) =>
  JSON.stringify({ bound: false, server: 'ok', identity: 'claude-agent', role: 'agent', review: { total: review }, backup: { enabled: false } })

const run = (stdout: string, exitCode = 0, stderr = '') => ({ exitCode, stdout, stderr })

describe('statusText', () => {
  const cases: Array<[string, HermaStatus, string | undefined]> = [
    ['healthy', parseStatus(run(okJSON(0, { enabled: true, age_seconds: 3 * 3600, stale: false }))), 'herma ✓ · backup 3h'],
    ['review waiting', parseStatus(run(okJSON(2, { enabled: true, age_seconds: 45 * 60, stale: false }))), 'herma ✓ · 2 to review · backup 45m'],
    ['stale', parseStatus(run(okJSON(0, { enabled: true, age_seconds: 2 * 86400, stale: true }))), 'herma ✓ · backup 2d ⚠'],
    ['47 hours stays hours', parseStatus(run(okJSON(0, { enabled: true, age_seconds: 47 * 3600, stale: false }))), 'herma ✓ · backup 47h'],
    ['off', parseStatus(run(okJSON(0, { enabled: false }))), 'herma ✓ · backup off'],
    ['none yet', parseStatus(run(okJSON(0, { enabled: true, stale: false }))), 'herma ✓ · backup none'],
    ['unbound', parseStatus(run('{"bound": false}')), undefined],
    ['failed', parseStatus(run('', 1, 'herma: knowledge base server is not running or reachable at http://127.0.0.1:8765; start herma serve\n')), 'herma ✗ knowledge base server is not running or reachable at http://'],
    ['reviewer', parseStatus(run(JSON.stringify({ bound: true, role: 'reviewer', review: { total: 0 }, backup: { enabled: false } }))), 'herma ✗ reviewer identity refused'],
  ]
  for (const [name, status, want] of cases) {
    test(name, () => {
      expect(statusText(status)).toBe(want)
    })
  }
})

test('firstLine strips control characters before cutting', () => {
  expect(firstLine('\n herma: \x1b[31mdown\x1b[0m\there\x07\n')).toBe('[31mdown[0mhere')
  expect(firstLine('\x1b'.repeat(80) + 'abc', 3)).toBe('abc')
})

test('parseStatus tolerates non-JSON', () => {
  const s = parseStatus(run('Usage: herma ...', 0))
  expect(s.kind).toBe('failed')
  expect(statusText(s)).toBe('herma ✗ unreadable herma status output')
})

test('isUsable needs bound, ok and agent', () => {
  expect(isUsable(parseStatus(run(okJSON(0, { enabled: false }))))).toBe(true)
  expect(isUsable(parseStatus(run('{"bound": false}')))).toBe(false)
  expect(isUsable(parseStatus(run('', 1, 'down')))).toBe(false)
  expect(isUsable(parseStatus(run(JSON.stringify({ bound: true, role: 'reviewer', review: { total: 0 }, backup: { enabled: false } }))))).toBe(false)
})

describe('transition', () => {
  const healthy = parseStatus(run(okJSON(0, { enabled: true, age_seconds: 60, stale: false })))
  const stale = parseStatus(run(okJSON(0, { enabled: true, age_seconds: 86400 * 3, stale: true })))
  const failed = parseStatus(run('', 1, 'herma: not running'))
  test('healthy to failed toasts once', () => {
    expect(transition(healthy, failed)).toBe('herma unreachable: not running')
    expect(transition(failed, failed)).toBe(undefined)
  })
  test('fresh to stale toasts once', () => {
    expect(transition(healthy, stale)).toBe('herma backup is stale')
    expect(transition(stale, stale)).toBe(undefined)
  })
  test('no toast without a previous healthy state', () => {
    expect(transition(undefined, failed)).toBe(undefined)
    expect(transition(undefined, stale)).toBe(undefined)
  })
  test('recovery is silent', () => {
    expect(transition(failed, healthy)).toBe(undefined)
  })
})

test('unbound with a server shows a status line but is not usable for tools', () => {
  const s = parseStatus(run(unboundJSON(1)))
  expect(statusText(s)).toBe('herma ✓ · 1 to review · backup off')
  expect(isUsable(s)).toBe(false)
})

describe('review toast', () => {
  const at = (n: number) => parseStatus(run(okJSON(n, { enabled: false })))
  test('increase toasts with the difference', () => {
    expect(transition(at(0), at(1))).toBe('herma: 1 new proposal to review')
    expect(transition(at(1), at(3))).toBe('herma: 2 new proposals to review')
  })
  test('first reading, steady and decrease are silent', () => {
    expect(transition(undefined, at(2))).toBe(undefined)
    expect(transition(at(2), at(2))).toBe(undefined)
    expect(transition(at(2), at(1))).toBe(undefined)
  })
})
