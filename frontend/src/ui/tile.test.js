/**
 * Tests for the PGM crop arithmetic.
 *
 * Owner: WP-5b. Run with Node's built-in test runner — package.json is frozen
 * (CONTRACT.md rule 1), so these use `node:test` and `node:assert`, which need
 * no dependency:
 *
 *   cd frontend && node --test "src/ui/*.test.js"
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  REFERENCE_MOSAIC,
  DEFAULT_TILE,
  normaliseMosaic,
  normaliseTile,
  rescaleTile,
  effectiveCrop,
  describeCrop,
} from './tile.js';

test('normaliseMosaic accepts both {w,h} and {width,height}', () => {
  assert.deepEqual(normaliseMosaic({ w: 1920, h: 1080 }), { w: 1920, h: 1080 });
  assert.deepEqual(normaliseMosaic({ width: 1920, height: 1080 }), { w: 1920, h: 1080 });
});

test('normaliseMosaic rejects the 0x0 a <video> reports before metadata', () => {
  // This is the case that matters: a <video> with no track yet answers 0x0, and
  // dividing by it would produce Infinity and a crop of nothing.
  assert.equal(normaliseMosaic({ width: 0, height: 0 }), null);
  assert.equal(normaliseMosaic({ width: 1920, height: 0 }), null);
  assert.equal(normaliseMosaic(null), null);
  assert.equal(normaliseMosaic(undefined), null);
  assert.equal(normaliseMosaic('1920x1080'), null);
  assert.equal(normaliseMosaic({ width: NaN, height: 1080 }), null);
  assert.equal(normaliseMosaic({ width: -1920, height: 1080 }), null);
});

test('normaliseTile passes a good tile through unchanged', () => {
  assert.deepEqual(normaliseTile({ x: 0, y: 360, w: 640, h: 360 }), { x: 0, y: 360, w: 640, h: 360 });
});

test('normaliseTile falls back per field, not per tile', () => {
  // A botched Settings save must show the documented PGM region, not a sliver.
  assert.deepEqual(normaliseTile({ x: 100, w: -5 }), {
    x: 100,
    y: DEFAULT_TILE.y,
    w: DEFAULT_TILE.w,
    h: DEFAULT_TILE.h,
  });
  assert.deepEqual(normaliseTile(null), { ...DEFAULT_TILE });
  assert.deepEqual(normaliseTile('nonsense'), { ...DEFAULT_TILE });
});

test('normaliseTile accepts numeric strings, because config.json is hand-edited', () => {
  assert.deepEqual(normaliseTile({ x: '0', y: '360', w: '640', h: '360' }), {
    x: 0,
    y: 360,
    w: 640,
    h: 360,
  });
});

test('normaliseTile clips a tile that hangs off the mosaic', () => {
  const t = normaliseTile({ x: 1000, y: 600, w: 640, h: 360 }, { w: 1120, h: 720 });
  assert.equal(t.x + t.w <= 1120, true, 'tile must not extend past the right edge');
  assert.equal(t.y + t.h <= 720, true, 'tile must not extend past the bottom edge');
});

test('rescaleTile is the identity when the mosaic is the size it was measured against', () => {
  const tile = { x: 0, y: 360, w: 640, h: 360 };
  assert.deepEqual(rescaleTile(tile, REFERENCE_MOSAIC, { w: 2240, h: 1440 }), tile);
});

test('rescaleTile halves the tile when the mosaic arrives at half size', () => {
  // The whole point: the same REGION of the multiviewer, expressed in the
  // pixels that actually arrived.
  assert.deepEqual(rescaleTile({ x: 0, y: 360, w: 640, h: 360 }, { w: 2240, h: 1440 }, { w: 1120, h: 720 }), {
    x: 0,
    y: 180,
    w: 320,
    h: 180,
  });
});

test('rescaleTile scales the axes independently', () => {
  // 2240x1440 is 14:9. A live mosaic at 16:9 stretches one axis more than the
  // other, and using one factor for both would move the tile off its row.
  const got = rescaleTile({ x: 640, y: 360, w: 640, h: 360 }, { w: 2240, h: 1440 }, { w: 1920, h: 1080 });
  assert.deepEqual(got, {
    x: Math.round(640 * (1920 / 2240)),
    y: Math.round(360 * (1080 / 1440)),
    w: Math.round(640 * (1920 / 2240)),
    h: Math.round(360 * (1080 / 1440)),
  });
});

test('rescaleTile keeps the rescaled tile inside the live mosaic', () => {
  // The bottom-right tile of the grid, rounded up, must not spill over the edge
  // — a tile that hangs off the picture renders as black margin, which is the
  // symptom this whole module exists to remove.
  const got = rescaleTile({ x: 1600, y: 1080, w: 640, h: 360 }, { w: 2240, h: 1440 }, { w: 1919, h: 1079 });
  assert.equal(got.x + got.w <= 1919, true, `${JSON.stringify(got)} spills past the right edge`);
  assert.equal(got.y + got.h <= 1079, true, `${JSON.stringify(got)} spills past the bottom edge`);
});

test('rescaleTile leaves the tile alone when the live mosaic is unknown', () => {
  const tile = { x: 0, y: 360, w: 640, h: 360 };
  assert.deepEqual(rescaleTile(tile, REFERENCE_MOSAIC, null), tile);
  assert.deepEqual(rescaleTile(tile, REFERENCE_MOSAIC, { w: 0, h: 0 }), tile);
});

test('rescaleTile survives a nonsense reference mosaic by using the measured one', () => {
  assert.deepEqual(rescaleTile({ x: 0, y: 360, w: 640, h: 360 }, { w: 0, h: 0 }, { w: 1120, h: 720 }), {
    x: 0,
    y: 180,
    w: 320,
    h: 180,
  });
});

test('effectiveCrop reports the live mosaic and that it rescaled', () => {
  const crop = effectiveCrop({ x: 0, y: 360, w: 640, h: 360 }, { width: 1120, height: 720 });
  assert.equal(crop.known, true);
  assert.equal(crop.rescaled, true);
  assert.deepEqual(crop.mosaic, { w: 1120, h: 720 });
  assert.deepEqual(crop.tile, { x: 0, y: 180, w: 320, h: 180 });
  assert.equal(crop.aspect, 320 / 180);
});

test('effectiveCrop before metadata uses the reference mosaic and says so', () => {
  const crop = effectiveCrop({ x: 0, y: 360, w: 640, h: 360 }, { width: 0, height: 0 });
  assert.equal(crop.known, false);
  assert.equal(crop.rescaled, false);
  assert.deepEqual(crop.mosaic, { w: REFERENCE_MOSAIC.w, h: REFERENCE_MOSAIC.h });
  assert.deepEqual(crop.tile, { x: 0, y: 360, w: 640, h: 360 });
});

test('effectiveCrop does not claim a rescale when the sizes match', () => {
  const crop = effectiveCrop({ x: 0, y: 360, w: 640, h: 360 }, { width: 2240, height: 1440 });
  assert.equal(crop.known, true);
  assert.equal(crop.rescaled, false);
});

test('effectiveCrop preserves the tile aspect ratio through a rescale', () => {
  // The container is sized from crop.aspect. If the rescale distorted it the
  // picture would be stretched, which in a commentary position is a bug report.
  const crop = effectiveCrop({ x: 0, y: 360, w: 640, h: 360 }, { width: 1120, height: 720 });
  assert.equal(Math.abs(crop.aspect - 640 / 360) < 0.001, true, `aspect ${crop.aspect}`);
});

test('describeCrop names both mosaics when it rescales', () => {
  const tile = { x: 0, y: 360, w: 640, h: 360 };
  const line = describeCrop(effectiveCrop(tile, { width: 1120, height: 720 }), tile);
  assert.match(line, /2240x1440/);
  assert.match(line, /1120x720/);
  assert.match(line, /320x180/);
});

test('describeCrop says when there is no metadata yet', () => {
  const tile = { x: 0, y: 360, w: 640, h: 360 };
  const line = describeCrop(effectiveCrop(tile, null), tile);
  assert.match(line, /no video track metadata yet/);
});
