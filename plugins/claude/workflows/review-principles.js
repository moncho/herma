export const meta = {
  name: 'review-principles',
  description: 'Review a diff against the accepted herma principles, then try to refute each finding',
  whenToUse: 'Before opening or merging a change, to check it against the principles herma holds for this user and project',
  phases: [
    { title: 'Scope', detail: 'changed files and the principles a diff can break' },
    { title: 'Review', detail: 'one reviewer per changed file' },
    { title: 'Verify', detail: 'one skeptic per finding' },
  ],
}

// args: an optional git range or PR reference, such as "main...HEAD" or "#42".
// Without it, the change is the current branch against its merge base with the
// default branch, plus uncommitted work.
const target = typeof args === 'string' && args.trim() ? args.trim() : null

const SCOPE = {
  type: 'object',
  required: ['range', 'files', 'principles'],
  properties: {
    range: { type: 'string', description: 'the exact command or range that produces the diff' },
    files: { type: 'array', items: { type: 'string' } },
    principles: {
      type: 'array',
      items: {
        type: 'object',
        required: ['id', 'title', 'rule'],
        properties: { id: { type: 'string' }, title: { type: 'string' }, rule: { type: 'string' } },
      },
    },
  },
}

const FINDINGS = {
  type: 'object',
  required: ['findings'],
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        required: ['principle_id', 'line', 'claim', 'evidence'],
        properties: {
          principle_id: { type: 'string' },
          line: { type: 'integer' },
          claim: { type: 'string', description: 'what the change does that breaks the principle' },
          evidence: { type: 'string', description: 'the code, quoted, that shows it' },
        },
      },
    },
  },
}

const VERDICT = {
  type: 'object',
  required: ['refuted', 'reason'],
  properties: { refuted: { type: 'boolean' }, reason: { type: 'string' } },
}

phase('Scope')
const scope = await agent(
  `Find what to review and what to review it against. Return data only.

1. The change: ${target
    ? `the git range or PR "${target}".`
    : 'the current branch against its merge base with the default branch, plus uncommitted work.'}
   Set "range" to the exact command that prints this diff (for example "git diff main...HEAD").
   List every changed file in "files", skipping deleted, generated and vendored files.
2. The principles: the accepted herma principles in your instructions, plus the project's
   principles from \`herma context\` if this checkout is bound. Keep only principles a code
   diff can break (error handling, API shape, design simplicity, committed artifacts);
   drop ones about conversation, PR prose or session process. Give each its herma record
   ID, title and a one-sentence rule.`,
  { label: 'scope', schema: SCOPE, effort: 'low' },
)

if (!scope || !scope.files.length || !scope.principles.length) {
  log('Nothing to review: no changed files or no applicable principles.')
  return { range: scope ? scope.range : null, principles: scope ? scope.principles : [], confirmed: [], refuted: [], unverified: [] }
}
log(`Reviewing ${scope.files.length} file(s) against ${scope.principles.length} principle(s).`)

const rules = scope.principles.map(p => `- ${p.id} · ${p.title}: ${p.rule}`).join('\n')

const perFile = await pipeline(
  scope.files,
  file => agent(
    `Review only the changes to ${file} in \`${scope.range}\` against these principles:

${rules}

Report a finding only where a changed line breaks one of them, and quote that line. Code
outside the diff is context, not a finding. An empty list is a good answer.`,
    { label: `review:${file}`, phase: 'Review', schema: FINDINGS },
  ),
  (review, file) => {
    if (!review) throw new Error(`review of ${file} failed`) // drops the file to null, reported below
    return parallel(review.findings.map(f => () =>
    agent(
      `Try to refute this finding about ${file} in \`${scope.range}\`.

Principle ${f.principle_id}: ${(scope.principles.find(p => p.id === f.principle_id) || {}).rule || 'unknown principle'}
Line ${f.line}: ${f.claim}
Evidence: ${f.evidence}

Read the diff and the surrounding code. It is refuted if the quoted code is not in the
change, the principle does not cover it, or the code already meets the principle.
If you cannot settle it from the code, set refuted to true.`,
      { label: `verify:${file}:${f.line}`, phase: 'Verify', schema: VERDICT },
    ).then(v => ({ file, ...f, verdict: v })),
    ))
  },
)

const all = perFile.filter(Boolean).flat().filter(Boolean)
const failedFiles = scope.files.filter((_, i) => !perFile[i])
if (failedFiles.length) log(`Not reviewed (agent failed): ${failedFiles.join(', ')}`)

return {
  range: scope.range,
  principles: scope.principles,
  confirmed: all.filter(f => f.verdict && !f.verdict.refuted),
  refuted: all.filter(f => f.verdict && f.verdict.refuted).map(f => ({ file: f.file, line: f.line, principle_id: f.principle_id, reason: f.verdict.reason })),
  unverified: all.filter(f => !f.verdict),
  not_reviewed: failedFiles,
}
