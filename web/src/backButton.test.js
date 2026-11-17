import { test } from 'node:test'
import assert from 'node:assert/strict'
import { backStep } from './backButton.js'

test('nothing to undo leaves Back to the app', () => {
  assert.equal(backStep({}), null)
  assert.equal(backStep(), null)
})

test('a dialog closes before anything else', () => {
  assert.equal(backStep({ dialogOpen: true, drawerOpen: true, canBack: true, canUp: true }), 'dialog')
})

test('the drawer closes before the folder changes', () => {
  assert.equal(backStep({ drawerOpen: true, canBack: true, canUp: true }), 'drawer')
})

test('stepping back is preferred to going up', () => {
  assert.equal(backStep({ canBack: true, canUp: true }), 'back')
  assert.equal(backStep({ canBack: true }), 'back')
})

test('up when nothing is behind but the top is not reached', () => {
  assert.equal(backStep({ canUp: true }), 'up')
})
