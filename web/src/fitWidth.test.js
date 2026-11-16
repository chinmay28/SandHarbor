import { test } from 'node:test'
import assert from 'node:assert/strict'
import { GROW_SLOP, SHRINK_SLOP, nextFitWidth } from './fitWidth.js'

test('the first measurement is taken as it is', () => {
  assert.equal(nextFitWidth(0, 360), 360)
})

test('nothing measured keeps the last width', () => {
  assert.equal(nextFitWidth(360, 0), 360)
  assert.equal(nextFitWidth(0, 0), 0)
})

test('narrower wins beyond the noise', () => {
  assert.equal(nextFitWidth(360, 360 - SHRINK_SLOP + 1), 360)
  assert.equal(nextFitWidth(360, 350), 350)
})

test('wider has to be more than a scrollbar', () => {
  assert.equal(nextFitWidth(350, 360), 350)
  assert.equal(nextFitWidth(350, 350 + GROW_SLOP - 1), 350)
  assert.equal(nextFitWidth(350, 350 + GROW_SLOP), 350 + GROW_SLOP)
})

test('a scrollbar coming and going settles instead of looping', () => {
  // The dialog's scrollbar toggles the viewer between these two widths for
  // as long as each redraw keeps flipping whether it is needed.
  let width = 0
  const seen = []
  for (const measured of [360, 350, 360, 350, 360, 350]) {
    width = nextFitWidth(width, measured)
    seen.push(width)
  }
  assert.deepEqual(seen, [360, 350, 350, 350, 350, 350])
})

test('a real resize goes through in both directions', () => {
  assert.equal(nextFitWidth(360, 740), 740)
  assert.equal(nextFitWidth(740, 360), 360)
})
