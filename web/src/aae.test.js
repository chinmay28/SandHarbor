import { test } from 'node:test'
import assert from 'node:assert/strict'
import { candidateStems, isSidecar, photoFor } from './aae.js'

const entry = (name) => ({ file: { id: name, name }, hasThumb: false })
const names = (...list) => list.map(entry)

test('only .aae files are sidecars, whatever their case', () => {
  assert.equal(isSidecar('IMG_1234.AAE'), true)
  assert.equal(isSidecar('img_1234.aae'), true)
  assert.equal(isSidecar('IMG_1234.HEIC'), false)
  assert.equal(isSidecar('aae'), false)
  assert.equal(isSidecar(), false)
})

test('a sidecar finds the picture of the same name', () => {
  const gallery = names('IMG_1233.HEIC', 'IMG_1234.HEIC', 'IMG_1235.JPG')
  assert.equal(photoFor('IMG_1234.AAE', gallery).file.name, 'IMG_1234.HEIC')
})

test('the edited rendering is preferred to the original when both are there', () => {
  const gallery = names('IMG_1234.HEIC', 'IMG_E1234.HEIC')
  assert.equal(photoFor('IMG_O1234.AAE', gallery).file.name, 'IMG_E1234.HEIC')
  assert.equal(photoFor('IMG_1234.AAE', gallery).file.name, 'IMG_E1234.HEIC')
})

test('an _O sidecar falls back to the original when there is no rendering', () => {
  assert.equal(photoFor('IMG_O1234.AAE', names('IMG_1234.JPG')).file.name, 'IMG_1234.JPG')
})

test('names are matched without regard to case', () => {
  assert.equal(photoFor('img_o1234.aae', names('IMG_1234.HEIC')).file.name, 'IMG_1234.HEIC')
})

test('a sidecar with no picture beside it, or a file that is not one, finds nothing', () => {
  assert.equal(photoFor('IMG_9999.AAE', names('IMG_1234.HEIC')), null)
  assert.equal(photoFor('IMG_1234.HEIC', names('IMG_1234.HEIC')), null)
  assert.equal(photoFor('IMG_1234.AAE', []), null)
  assert.equal(photoFor('IMG_1234.AAE'), null)
})

test('a number is not matched on its tail alone', () => {
  assert.equal(photoFor('IMG_1234.AAE', names('IMG_11234.HEIC', 'IMG_234.HEIC')), null)
})

test('a stem outside the IMG_ shape is only itself', () => {
  assert.deepEqual(candidateStems('Holiday'), ['holiday'])
  assert.equal(photoFor('Holiday.aae', names('holiday.jpg')).file.name, 'holiday.jpg')
  assert.deepEqual(candidateStems('IMG_O7'), ['img_e7', 'img_7', 'img_o7'])
})
