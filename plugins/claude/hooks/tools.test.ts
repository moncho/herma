import { expect, test } from 'claude-code/testing'
import { getArgs, recallArgs, RECALL_TOOL, GET_TOOL } from './tools'

test('recall builds argv with defaults', () => {
  expect(recallArgs({ query: 'snapshot pruning' })).toEqual({ argv: ['recall', 'snapshot pruning', '--limit', '8'] })
  expect(recallArgs({ query: 'x y', limit: 3, include_proposed: true })).toEqual({ argv: ['recall', 'x y', '--limit', '3', '--include-proposed'] })
})

test('recall refuses bad input', () => {
  expect(recallArgs({})).toEqual({ error: 'query is required' })
  expect(recallArgs({ query: '   ' })).toEqual({ error: 'query is required' })
  expect(recallArgs({ query: 'a'.repeat(501) })).toEqual({ error: 'query must be at most 500 characters' })
  expect(recallArgs({ query: 'x', limit: 0 })).toEqual({ error: 'limit must be an integer from 1 to 20' })
  expect(recallArgs({ query: 'x', limit: 2.5 })).toEqual({ error: 'limit must be an integer from 1 to 20' })
  expect(recallArgs({ query: 'x', include_proposed: 'yes' })).toEqual({ error: 'include_proposed must be true or false' })
})

test('recall refuses a leading dash', () => {
  expect(recallArgs({ query: '-v' })).toEqual({ error: 'query must not start with "-"' })
})

test('get validates the record ID', () => {
  const id = 'rec_' + '0123456789abcdef'.repeat(2)
  expect(getArgs({ id })).toEqual({ argv: ['get', id] })
  expect(getArgs({ id: 'rec_123' })).toEqual({ error: 'id must be rec_ followed by 32 lowercase hex characters' })
  expect(getArgs({})).toEqual({ error: 'id must be rec_ followed by 32 lowercase hex characters' })
})

test('tool specs name recall and get', () => {
  expect(RECALL_TOOL.name).toBe('recall')
  expect(GET_TOOL.name).toBe('get')
})
