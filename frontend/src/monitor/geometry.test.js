/**
 * Tests for the mosaic geometry.
 *
 * Owner: WP-5a. Run with Node's built-in test runner — there is no test
 * framework in frontend/package.json and package.json is frozen (CONTRACT.md
 * rule 1), so these use `node:test` and `node:assert`, which need no dependency:
 *
 *   cd frontend && node --test src/monitor/
 *
 * Only the pure modules are covered. video.js, audio.js, signalling.js,
 * peer.js and monitor.js need a DOM, Web Audio, WebRTC and a live KVS channel
 * respectively; harness.html is how those are exercised.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  normaliseTile,
  clampScale,
  fitScale,
  cropStyles,
  layoutSize,
  DEFAULT_TILE,
  MOSAIC_WIDTH,
  MOSAIC_HEIGHT,
  MIN_SCALE,
  MAX_SCALE,
} from './geometry.js';

test('normaliseTile', async (t) => {
  const cases = [
    {
      name: 'the measured PGM tile passes through unchanged',
      in: { x: 0, y: 360, w: 640, h: 360 },
      want: { x: 0, y: 360, w: 640, h: 360 },
    },
    {
      name: 'the measured PVW tile passes through unchanged',
      in: { x: 0, y: 0, w: 640, h: 360 },
      want: { x: 0, y: 0, w: 640, h: 360 },
    },
    {
      name: 'a source thumbnail on the 320x180 grid passes through',
      in: { x: 1280, y: 362, w: 320, h: 180 },
      want: { x: 1280, y: 362, w: 320, h: 180 },
    },
    {
      name: 'undefined falls back to the PGM default',
      in: undefined,
      want: { ...DEFAULT_TILE },
    },
    {
      name: 'null falls back to the PGM default',
      in: null,
      want: { ...DEFAULT_TILE },
    },
    {
      name: 'a non-object falls back to the PGM default',
      in: 'nope',
      want: { ...DEFAULT_TILE },
    },
    {
      name: 'missing fields fall back per-field, not wholesale',
      in: { x: 640 },
      want: { x: 640, y: DEFAULT_TILE.y, w: DEFAULT_TILE.w, h: DEFAULT_TILE.h },
    },
    {
      name: 'numeric strings from a hand-edited config.json are accepted',
      in: { x: '640', y: '181', w: '320', h: '180' },
      want: { x: 640, y: 181, w: 320, h: 180 },
    },
    {
      name: 'NaN fields fall back',
      in: { x: NaN, y: NaN, w: NaN, h: NaN },
      want: { ...DEFAULT_TILE },
    },
    {
      name: 'Infinity fields fall back',
      in: { x: Infinity, y: 0, w: Infinity, h: 100 },
      want: { x: DEFAULT_TILE.x, y: 0, w: DEFAULT_TILE.w, h: 100 },
    },
    {
      name: 'a negative origin is a typo, not a value to clamp: it falls back per-field',
      in: { x: -50, y: -50, w: 640, h: 360 },
      want: { x: DEFAULT_TILE.x, y: DEFAULT_TILE.y, w: 640, h: 360 },
    },
    {
      name: 'a negative width falls back rather than inverting the crop',
      in: { x: 0, y: 0, w: -640, h: 360 },
      want: { x: 0, y: 0, w: DEFAULT_TILE.w, h: 360 },
    },
    {
      name: 'zero width falls back rather than collapsing the picture',
      in: { x: 0, y: 360, w: 0, h: 360 },
      want: { x: 0, y: 360, w: DEFAULT_TILE.w, h: 360 },
    },
    {
      name: 'a tile wider than the mosaic is clipped to the mosaic',
      in: { x: 2000, y: 1400, w: 640, h: 360 },
      want: { x: 2000, y: 1400, w: MOSAIC_WIDTH - 2000, h: MOSAIC_HEIGHT - 1400 },
    },
    {
      name: 'an origin past the mosaic clamps inside it',
      in: { x: 99999, y: 99999, w: 640, h: 360 },
      want: { x: MOSAIC_WIDTH - 1, y: MOSAIC_HEIGHT - 1, w: 1, h: 1 },
    },
    {
      name: 'fractional values round',
      in: { x: 0.4, y: 359.6, w: 640.2, h: 359.5 },
      want: { x: 0, y: 360, w: 640, h: 360 },
    },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      assert.deepEqual(normaliseTile(c.in), c.want);
    });
  }
});

test('normaliseTile with a non-default mosaic size', async (t) => {
  // If Sony changes the multiviewer resolution the tile must be re-clamped to
  // the real intrinsic size, which is what video.js does on loadedmetadata.
  const cases = [
    {
      name: 'a tile inside a smaller mosaic is untouched',
      tile: { x: 0, y: 180, w: 320, h: 180 },
      mosaic: { width: 1120, height: 720 },
      want: { x: 0, y: 180, w: 320, h: 180 },
    },
    {
      name: 'the PGM tile is clipped into a half-size mosaic rather than lost',
      tile: { x: 0, y: 360, w: 640, h: 360 },
      mosaic: { width: 1120, height: 720 },
      want: { x: 0, y: 360, w: 640, h: 360 },
    },
    {
      name: 'a tile past the bottom of a small mosaic clamps to one row',
      tile: { x: 0, y: 700, w: 640, h: 360 },
      mosaic: { width: 1120, height: 720 },
      want: { x: 0, y: 700, w: 640, h: 20 },
    },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      assert.deepEqual(normaliseTile(c.tile, c.mosaic), c.want);
    });
  }
});

test('clampScale', async (t) => {
  const cases = [
    { name: 'unity', in: 1, want: 1 },
    { name: 'a half', in: 0.5, want: 0.5 },
    { name: 'numeric string', in: '2', want: 2 },
    { name: 'zero resolves to unity, not to an invisible picture', in: 0, want: 1 },
    { name: 'negative resolves to unity', in: -3, want: 1 },
    { name: 'NaN resolves to unity', in: NaN, want: 1 },
    { name: 'undefined resolves to unity', in: undefined, want: 1 },
    { name: 'Infinity is not finite and resolves to unity', in: Infinity, want: 1 },
    { name: 'above the ceiling clamps down', in: 99, want: MAX_SCALE },
    { name: 'below the floor clamps up', in: 0.0001, want: MIN_SCALE },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      assert.equal(clampScale(c.in), c.want);
    });
  }
});

test('fitScale', async (t) => {
  const pgm = { x: 0, y: 360, w: 640, h: 360 };
  const cases = [
    { name: 'exact width is unity', tile: pgm, w: 640, h: undefined, want: 1 },
    { name: 'double width is 2x', tile: pgm, w: 1280, h: undefined, want: 2 },
    { name: 'half width is 0.5x', tile: pgm, w: 320, h: undefined, want: 0.5 },
    {
      name: 'width and height: the smaller ratio wins so nothing overflows',
      tile: pgm,
      w: 1280,
      h: 360,
      want: 1,
    },
    {
      name: 'a tall box still fits the width, per spec §10',
      tile: pgm,
      w: 1280,
      h: 4000,
      want: 2,
    },
    { name: 'height only fits the height', tile: pgm, w: 0, h: 720, want: 2 },
    { name: 'no box at all is unity', tile: pgm, w: 0, h: 0, want: 1 },
    { name: 'a NaN width is unity', tile: pgm, w: NaN, h: undefined, want: 1 },
    { name: 'an absurd width clamps to MAX_SCALE', tile: pgm, w: 100000, h: undefined, want: MAX_SCALE },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      assert.equal(fitScale(c.tile, c.w, c.h), c.want);
    });
  }
});

test('cropStyles implements the spec §7 recipe', async (t) => {
  await t.test('the PGM tile at unity', () => {
    const s = cropStyles({ x: 0, y: 360, w: 640, h: 360 }, 1);
    assert.equal(s.crop.width, '640px');
    assert.equal(s.crop.height, '360px');
    assert.equal(s.crop.overflow, 'hidden');
    assert.equal(s.crop.transform, 'scale(1)');
    assert.equal(s.crop.transformOrigin, '0 0');
    // "the <video> inside at natural size with left:0; top:-360px"
    assert.equal(s.video.left, '0px');
    assert.equal(s.video.top, '-360px');
    assert.equal(s.video.width, `${MOSAIC_WIDTH}px`);
    assert.equal(s.video.height, `${MOSAIC_HEIGHT}px`);
    // The stage carries the scaled size, because transform does not lay out.
    assert.equal(s.stage.width, '640px');
    assert.equal(s.stage.height, '360px');
  });

  await t.test('the PGM tile at 1.5x', () => {
    const s = cropStyles({ x: 0, y: 360, w: 640, h: 360 }, 1.5);
    assert.equal(s.crop.width, '640px', 'the crop box does not change size, the transform scales it');
    assert.equal(s.crop.transform, 'scale(1.5)');
    assert.equal(s.stage.width, '960px');
    assert.equal(s.stage.height, '540px');
  });

  await t.test('a thumbnail tile offsets in both axes', () => {
    const s = cropStyles({ x: 1280, y: 362, w: 320, h: 180 }, 1);
    assert.equal(s.video.left, '-1280px');
    assert.equal(s.video.top, '-362px');
    assert.equal(s.crop.width, '320px');
    assert.equal(s.crop.height, '180px');
  });

  await t.test('a non-default mosaic size sizes the video element to match', () => {
    const s = cropStyles({ x: 0, y: 180, w: 320, h: 180 }, 1, { width: 1120, height: 720 });
    assert.equal(s.video.width, '1120px');
    assert.equal(s.video.height, '720px');
  });

  await t.test('a bad scale never removes the transform', () => {
    const s = cropStyles({ x: 0, y: 360, w: 640, h: 360 }, NaN);
    assert.equal(s.crop.transform, 'scale(1)');
    assert.equal(s.crop.overflow, 'hidden', 'without overflow:hidden the whole mosaic shows');
  });
});

test('layoutSize', async (t) => {
  const cases = [
    { name: 'unity', tile: { x: 0, y: 360, w: 640, h: 360 }, k: 1, want: { width: 640, height: 360 } },
    { name: 'double', tile: { x: 0, y: 360, w: 640, h: 360 }, k: 2, want: { width: 1280, height: 720 } },
    { name: 'a bad scale falls back to unity', tile: { x: 0, y: 360, w: 640, h: 360 }, k: 0, want: { width: 640, height: 360 } },
  ];
  for (const c of cases) {
    await t.test(c.name, () => {
      assert.deepEqual(layoutSize(c.tile, c.k), c.want);
    });
  }
});
