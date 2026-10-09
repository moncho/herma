// Reading `herma status` and turning it into the status line and its toasts.

export type Backup = { enabled: false } | { enabled: true; ageSeconds?: number; stale: boolean }

export type HermaStatus =
  | { kind: 'unbound' }
  | { kind: 'failed'; reason: string }
  | { kind: 'ok'; bound: boolean; role: string; review: number; backup: Backup }

type Run = { exitCode: number; stdout: string; stderr: string }

// firstLine returns the first nonblank line, without control characters or
// herma's "herma: " prefix.
export function firstLine(text: string, max = 60): string {
  const line = text.split('\n').map(l => l.replace(/\p{Cc}/gu, '')).find(l => l.trim() !== '') ?? ''
  return line.trim().replace(/^herma: /, '').slice(0, max)
}

export function parseStatus(r: Run): HermaStatus {
  if (r.exitCode !== 0) {
    return { kind: 'failed', reason: firstLine(r.stderr) || `herma status exited ${r.exitCode}` }
  }
  let doc: Record<string, unknown>
  try {
    doc = JSON.parse(r.stdout)
  } catch {
    return { kind: 'failed', reason: 'unreadable herma status output' }
  }
  if (typeof doc !== 'object' || doc === null) return { kind: 'failed', reason: 'unreadable herma status output' }
  // A bare {"bound": false} comes from older herma, which says nothing more.
  if (doc.bound === false && typeof doc.role !== 'string') return { kind: 'unbound' }
  if (typeof doc.bound !== 'boolean') return { kind: 'failed', reason: 'unreadable herma status output' }
  const review = doc.review as { total?: unknown } | undefined
  const b = (doc.backup ?? {}) as { enabled?: unknown; age_seconds?: unknown; stale?: unknown }
  const backup: Backup = b.enabled === true
    ? { enabled: true, ageSeconds: typeof b.age_seconds === 'number' ? b.age_seconds : undefined, stale: b.stale === true }
    : { enabled: false }
  return {
    kind: 'ok',
    bound: doc.bound === true,
    role: typeof doc.role === 'string' ? doc.role : '',
    review: typeof review?.total === 'number' ? review.total : 0,
    backup,
  }
}

export function isUsable(s: HermaStatus): boolean {
  return s.kind === 'ok' && s.bound && s.role === 'agent'
}

function age(seconds: number): string {
  if (seconds < 3600) return `${Math.max(0, Math.floor(seconds / 60))}m`
  if (seconds < 2 * 86400) return `${Math.floor(seconds / 3600)}h`
  return `${Math.floor(seconds / 86400)}d`
}

export function statusText(s: HermaStatus): string | undefined {
  if (s.kind === 'unbound') return undefined
  if (s.kind === 'failed') return `herma ✗ ${s.reason}`
  if (s.role !== 'agent') return 'herma ✗ reviewer identity refused'
  const parts = ['herma ✓']
  if (s.review > 0) parts.push(`${s.review} to review`)
  const b = s.backup
  if (!b.enabled) parts.push('backup off')
  else if (b.ageSeconds === undefined) parts.push('backup none')
  else parts.push(`backup ${age(b.ageSeconds)}${b.stale ? ' ⚠' : ''}`)
  return parts.join(' · ')
}

function healthy(s: HermaStatus | undefined): boolean {
  return s?.kind === 'ok'
}

function stale(s: HermaStatus | undefined): boolean {
  return s?.kind === 'ok' && s.backup.enabled && s.backup.stale
}

// transition names a toast for a change into a bad state: healthy to failed,
// fresh to stale, or more proposals to review. A first reading never toasts.
export function transition(prev: HermaStatus | undefined, next: HermaStatus): string | undefined {
  if (next.kind === 'failed' && healthy(prev)) return `herma unreachable: ${next.reason}`
  if (stale(next) && healthy(prev) && !stale(prev)) return 'herma backup is stale'
  if (next.kind === 'ok' && prev?.kind === 'ok' && next.review > prev.review) {
    const n = next.review - prev.review
    return `herma: ${n} new proposal${n === 1 ? '' : 's'} to review`
  }
  return undefined
}
