/**
 * Tests for the native picture overlay: the rectangle, and what covers it.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE TWO FAILURES THESE PREVENT =====================
 *
 * 1. THE WRONG PIXELS. getBoundingClientRect speaks CSS pixels; a native window
 *    is positioned in physical device pixels. On a display at 150% scaling they
 *    differ by half again, and sending the wrong one does not fail — it draws a
 *    picture two thirds of the right size, up and to the left of where it
 *    belongs, with every call reporting success.
 *
 * 2. THE OVERLAY ON TOP OF SOMETHING THAT MATTERS. It is a native child window:
 *    opaque, outside the page's stacking context, and no z-index reaches it.
 *    Settings drawn under it is a configuration form somebody can read two
 *    thirds of. The mixer drawer under it is a LIVE ROUTING MATRIX somebody can
 *    read two thirds of and click all of.
 *
 * createOverlay takes its measuring and its IPC as arguments, so both are tested
 * by driving it rather than by reading source. The wiring — that Settings and
 * the drawer actually call block() — is the one part that has to be read, for
 * the reason mixerwiring.test.js gives: it is an absence-of-call-site property,
 * and app.js needs a DOM and a Wails runtime to run at all.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  toPhysicalRect,
  rectsEqual,
  normaliseDpr,
  describeRect,
  createOverlay,
  BLOCK_SETTINGS,
  BLOCK_MIXER,
  BLOCK_HIDDEN,
} from './overlay.js';

const here = dirname(fileURLToPath(import.meta.url));
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);

function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

/**
 * harness builds an overlay over a fake window, and records every call that
 * would have crossed into Go.
 */
function harness(initial = {}) {
  const state = {
    css: initial.css !== undefined ? initial.css : { x: 10, y: 20, width: 640, height: 360 },
    dpr: initial.dpr !== undefined ? initial.dpr : 1,
    /** the PHYSICAL rectangles, which is what the assertions are about */
    rects: [],
    /** the (cssRect, ratio) pairs actually sent to Go */
    sent: [],
    visibles: [],
    logs: [],
  };
  const overlay = createOverlay({
    measure: () => state.css,
    dpr: () => state.dpr,
    setRect: (css, dpr, physical) => {
      state.sent.push({ css, dpr });
      state.rects.push({ ...physical });
    },
    setVisible: (v) => state.visibles.push(v),
    log: (m) => state.logs.push(m),
  });
  return { state, overlay };
}

// --------------------------------------------------------------------------
// Physical pixels
// --------------------------------------------------------------------------

test('at a device pixel ratio of 1 the rectangle is the CSS box', () => {
  assert.deepEqual(toPhysicalRect({ x: 10, y: 20, width: 640, height: 360 }, 1), {
    x: 10,
    y: 20,
    w: 640,
    h: 360,
  });
});

test('at 150% scaling every coordinate is multiplied, not just the size', () => {
  // The failure this catches is the one that looks nearly right: a correctly
  // scaled picture in the wrong place, because the origin was passed through in
  // CSS pixels while the size was converted.
  assert.deepEqual(toPhysicalRect({ x: 100, y: 40, width: 640, height: 360 }, 1.5), {
    x: 150,
    y: 60,
    w: 960,
    h: 540,
  });
});

test('the EDGES are rounded, so no seam of page background appears', () => {
  // 10.5 -> 11 and 100.5 -> 101 at dpr 1: rounding the origin and the size
  // independently would give x=11, w=90, whose right edge is 101 — but the box
  // beside it starts at 101 too only by luck. Rounding both edges makes
  // adjacent boxes share an edge by construction.
  const r = toPhysicalRect({ x: 10.5, y: 0.5, width: 90, height: 100 }, 1);
  assert.deepEqual(r, { x: 11, y: 1, w: 90, h: 100 });

  // The case that actually bites: a fractional ratio and a fractional origin.
  const s = toPhysicalRect({ x: 10.3, y: 0, width: 100.4, height: 50 }, 1.25);
  assert.equal(s.x, Math.round(10.3 * 1.25));
  assert.equal(s.x + s.w, Math.round((10.3 + 100.4) * 1.25), 'the right edge lands where it should');
});

test('every physical coordinate is an integer', () => {
  for (const dpr of [1, 1.25, 1.5, 1.75, 2, 2.5, 3]) {
    for (const box of [
      { x: 0, y: 0, width: 1920, height: 1080 },
      { x: 7.3333, y: 111.6667, width: 640.5, height: 360.25 },
      { x: 1.5, y: 2.5, width: 3.5, height: 4.5 },
    ]) {
      const r = toPhysicalRect(box, dpr);
      assert.ok(r, `${dpr} ${JSON.stringify(box)} should be measurable`);
      for (const k of ['x', 'y', 'w', 'h']) {
        assert.ok(Number.isInteger(r[k]), `${k}=${r[k]} must be a whole device pixel at dpr ${dpr}`);
      }
      assert.ok(r.w >= 1 && r.h >= 1, 'and never collapse to nothing');
    }
  }
});

test('a DOMRect with left/top instead of x/y is accepted', () => {
  assert.deepEqual(toPhysicalRect({ left: 4, top: 6, width: 10, height: 12 }, 2), {
    x: 8,
    y: 12,
    w: 20,
    h: 24,
  });
});

test('an unmeasurable box is null, not a rectangle of zero size', () => {
  // Null means "do not report" and leaves the overlay where it was. A zero-size
  // rectangle would move it to a corner and shrink it to nothing — which looks
  // exactly like a picture that never started.
  for (const bad of [
    null,
    undefined,
    {},
    'nope',
    { x: 0, y: 0, width: 0, height: 100 },
    { x: 0, y: 0, width: 100, height: 0 },
    { x: NaN, y: 0, width: 10, height: 10 },
    { x: 0, y: 0, width: -10, height: 10 },
  ]) {
    assert.equal(toPhysicalRect(bad, 1), null, `${JSON.stringify(bad)} is not a rectangle`);
  }
});

test('a nonsense device pixel ratio falls back to 1, never to 0', () => {
  // A ratio of zero would collapse every rectangle to nothing, and an absurd one
  // reaches an int conversion in Go, which is implementation-defined for
  // out-of-range floats. The 16 ceiling mirrors gst.ScaleRect's own.
  for (const bad of [0, -2, NaN, undefined, null, 'two', {}, 17, 1e9, Infinity]) {
    assert.equal(normaliseDpr(bad), 1, `${String(bad)} must fall back to 1`);
  }
  assert.equal(normaliseDpr(3), 3, 'a real ratio is left alone');
  assert.deepEqual(toPhysicalRect({ x: 5, y: 5, width: 10, height: 10 }, 0), {
    x: 5,
    y: 5,
    w: 10,
    h: 10,
  });
});

test('rectsEqual compares by value and tolerates nulls', () => {
  assert.ok(rectsEqual({ x: 1, y: 2, w: 3, h: 4 }, { x: 1, y: 2, w: 3, h: 4 }));
  assert.ok(!rectsEqual({ x: 1, y: 2, w: 3, h: 4 }, { x: 1, y: 2, w: 3, h: 5 }));
  assert.ok(!rectsEqual(null, { x: 1, y: 2, w: 3, h: 4 }));
  assert.ok(rectsEqual(null, null));
});

test('the console line states the ratio as well as the numbers', () => {
  // "The picture is the wrong size" is undiagnosable from a screenshot without
  // knowing which multiplier was applied.
  const line = describeRect({ x: 1, y: 2, w: 3, h: 4 }, 1.5);
  assert.match(line, /3x4/);
  assert.match(line, /\(1,2\)/);
  assert.match(line, /1\.5/);
  assert.match(line, /physical/i);
});

// --------------------------------------------------------------------------
// Reporting
// --------------------------------------------------------------------------

test('sync derives the rectangle in physical pixels', () => {
  const { state, overlay } = harness({ css: { x: 10, y: 20, width: 640, height: 360 }, dpr: 2 });
  overlay.sync();
  assert.deepEqual(state.rects, [{ x: 20, y: 40, w: 1280, h: 720 }]);
});

test('and sends the CSS rectangle and the ratio TOGETHER, in one call', () => {
  // gst.PictureRect is physical, and gst.ScaleRect is what converts — on the Go
  // side, because the ratio Go could read for itself (GetDpiForWindow) is the
  // monitor's scale factor and equals the WebView's only at 100% zoom.
  //
  // They go in ONE call so a rectangle can never be paired with a ratio measured
  // before or after it, which is the whole reason this is not two bindings.
  const { state, overlay } = harness({ css: { x: 10, y: 20, width: 640, height: 360 }, dpr: 1.5 });
  overlay.sync();
  assert.equal(state.sent.length, 1);
  assert.deepEqual(state.sent[0].css, { x: 10, y: 20, width: 640, height: 360 });
  assert.equal(state.sent[0].dpr, 1.5);
});

test('the JS and Go conversions are the same arithmetic', () => {
  // Two implementations of one calculation that differ anywhere are two
  // implementations that have to be argued about rather than compared. The
  // console line here has to be the rectangle Go lands on, or a support bundle
  // is worse than useless.
  const go = read(join(here, '..', '..', '..'), 'internal', 'gst', 'picture.go');
  const fn = go.slice(go.indexOf('func ScaleRect('), go.indexOf('func roundHalfAwayFromZero('));
  assert.match(fn, /left := roundHalfAwayFromZero\(x \* ratio\)/, 'Go rounds the left edge');
  assert.match(fn, /right := roundHalfAwayFromZero\(\(x \+ w\) \* ratio\)/, 'and the right edge');
  assert.match(fn, /W: right - left/, 'and subtracts, rather than rounding the width');
  assert.match(fn, /ratio > 16/, 'and treats an absurd ratio as 1');

  const js = ui('overlay.js');
  assert.match(js, /roundHalfAwayFromZero\(x \* k\)/);
  assert.match(js, /roundHalfAwayFromZero\(\(x \+ w\) \* k\)/);
  assert.match(js, /DPR_CEILING = 16/);
});

test('a DPI change re-reports even though the CSS box has not moved', () => {
  // A window dragged to a differently-scaled monitor does not move the CSS box
  // by a single pixel while every physical coordinate in it changes. The ratio
  // is read fresh on every sync precisely so this cannot be missed.
  const { state, overlay } = harness({ css: { x: 0, y: 0, width: 100, height: 50 }, dpr: 1 });
  overlay.sync();
  state.dpr = 1.5;
  overlay.sync();
  assert.deepEqual(state.rects, [
    { x: 0, y: 0, w: 100, h: 50 },
    { x: 0, y: 0, w: 150, h: 75 },
  ]);
});

test('a sync that changes nothing does not cross into Go', () => {
  // sync() is driven from a ResizeObserver and a resize listener. Reporting
  // unconditionally would put a queue of identical IPC calls behind every
  // window drag.
  const { state, overlay } = harness();
  overlay.sync();
  overlay.sync();
  overlay.sync();
  assert.equal(state.rects.length, 1);
});

test('an unmeasurable box does not throw the last rectangle away', () => {
  // The usual reason for one is the home view being hidden. Clearing it would
  // mean a fresh rectangle — and a frame at the wrong place — every time
  // somebody came back from Settings.
  const { state, overlay } = harness({ css: { x: 0, y: 0, width: 100, height: 50 } });
  overlay.sync();
  state.css = null;
  overlay.sync();
  assert.equal(state.rects.length, 1);
  assert.deepEqual(overlay.rect, { x: 0, y: 0, w: 100, h: 50 });
});

test('a failing binding is logged, not thrown, so a resize cannot break the page', () => {
  const logs = [];
  const overlay = createOverlay({
    measure: () => ({ x: 0, y: 0, width: 10, height: 10 }),
    dpr: () => 1,
    setRect: () => {
      throw new Error('no such binding');
    },
    setVisible: () => {
      throw new Error('no such binding');
    },
    log: (m) => logs.push(m),
  });
  assert.doesNotThrow(() => overlay.sync());
  overlay.setWanted(true);
  assert.ok(
    logs.some((l) => l.includes('no such binding')),
    'the failure must be said somewhere',
  );
});

// --------------------------------------------------------------------------
// What covers it
// --------------------------------------------------------------------------

test('the overlay is born hidden and blocked', () => {
  // An overlay that has not yet been told where to be, painted wherever a native
  // default puts it, is an opaque box over an application somebody is trying to
  // read. Nothing shows until a rectangle has been measured and nothing is
  // blocking.
  const { state, overlay } = harness();
  assert.equal(overlay.visible, false);
  assert.deepEqual(overlay.blockers, [BLOCK_HIDDEN]);
  overlay.sync();
  assert.equal(overlay.visible, false, 'a rectangle alone is not enough');
  assert.deepEqual(state.visibles, [], 'and nothing is told to show');
});

test('it shows only when it has a rectangle, is unblocked, and is wanted', () => {
  const { state, overlay } = harness();
  overlay.unblock(BLOCK_HIDDEN);
  overlay.setWanted(true);
  assert.equal(overlay.visible, false, 'still no rectangle');
  overlay.sync();
  assert.equal(overlay.visible, true);
  assert.deepEqual(state.visibles, [true]);
});

test('Settings hides it, and closing Settings brings it back', () => {
  const { state, overlay } = harness();
  overlay.unblock(BLOCK_HIDDEN);
  overlay.setWanted(true);
  overlay.sync();
  assert.equal(overlay.visible, true);

  overlay.block(BLOCK_SETTINGS);
  assert.equal(overlay.visible, false);
  overlay.unblock(BLOCK_SETTINGS);
  assert.equal(overlay.visible, true);
  assert.deepEqual(state.visibles, [true, false, true]);
});

test('the mixer drawer hides it, and closing the drawer brings it back', () => {
  const { overlay } = harness();
  overlay.unblock(BLOCK_HIDDEN);
  overlay.setWanted(true);
  overlay.sync();

  overlay.block(BLOCK_MIXER);
  assert.equal(overlay.visible, false, 'a live routing matrix must never be under it');
  overlay.unblock(BLOCK_MIXER);
  assert.equal(overlay.visible, true);
});

test('TWO BLOCKERS AT ONCE: releasing one does not uncover the other', () => {
  // This is the whole reason blocking is a SET and not a boolean. Settings is
  // opened from behind the mixer drawer — showSettings closes the drawer, which
  // releases the mixer block, and with a boolean the picture would come straight
  // back over the top of the Settings form.
  const { overlay } = harness();
  overlay.unblock(BLOCK_HIDDEN);
  overlay.setWanted(true);
  overlay.sync();

  overlay.block(BLOCK_MIXER);
  overlay.block(BLOCK_SETTINGS);
  assert.deepEqual(overlay.blockers, [BLOCK_MIXER, BLOCK_SETTINGS]);

  overlay.unblock(BLOCK_MIXER);
  assert.equal(overlay.visible, false, 'Settings is still on screen');

  overlay.unblock(BLOCK_SETTINGS);
  assert.equal(overlay.visible, true);
});

test('blocking and unblocking are idempotent', () => {
  const { state, overlay } = harness();
  overlay.unblock(BLOCK_HIDDEN);
  overlay.setWanted(true);
  overlay.sync();
  overlay.block(BLOCK_SETTINGS);
  overlay.block(BLOCK_SETTINGS);
  overlay.unblock(BLOCK_MIXER); // never blocked
  assert.equal(overlay.visible, false);
  overlay.unblock(BLOCK_SETTINGS);
  assert.equal(overlay.visible, true);
  assert.deepEqual(state.visibles, [true, false, true], 'no repeated IPC for a state that did not change');
});

test('a sync while blocked still reports geometry but does not show', () => {
  // The rectangle has to stay current while Settings is up, or coming back finds
  // the overlay at yesterday's geometry.
  const { state, overlay } = harness();
  overlay.unblock(BLOCK_HIDDEN);
  overlay.setWanted(true);
  overlay.sync();
  overlay.block(BLOCK_SETTINGS);

  state.css = { x: 0, y: 0, width: 1000, height: 562 };
  overlay.sync();
  assert.deepEqual(state.rects[state.rects.length - 1], { x: 0, y: 0, w: 1000, h: 562 });
  assert.equal(overlay.visible, false);
});

test('losing the SRT picture hides the overlay without touching the blockers', () => {
  // This is the fallback: the receiver stops SHOWING, the overlay goes, and the
  // mosaic that was underneath it all along is what the commentator sees.
  const { overlay } = harness();
  overlay.unblock(BLOCK_HIDDEN);
  overlay.setWanted(true);
  overlay.sync();
  assert.equal(overlay.visible, true);

  overlay.setWanted(false);
  assert.equal(overlay.visible, false);
  assert.deepEqual(overlay.blockers, [], 'nothing is covering it; there is simply nothing to paint');
});

// --------------------------------------------------------------------------
// The wiring
// --------------------------------------------------------------------------

test('app.js blocks the overlay for Settings, on both reasons', () => {
  const src = codeOnly(ui('app.js'));
  const show = src.slice(src.indexOf('function showSettings()'), src.indexOf('function showHome()'));
  assert.ok(show.includes('overlay.block(BLOCK_SETTINGS)'), 'Settings must hide the overlay');
  assert.ok(
    show.includes('overlay.block(BLOCK_HIDDEN)'),
    'and so must the home view going away, which is a separate fact with a separate release',
  );

  const home = src.slice(src.indexOf('function showHome()'), src.indexOf('function toggleMixer()'));
  assert.ok(home.includes('overlay.unblock(BLOCK_SETTINGS)'));
  assert.ok(home.includes('overlay.unblock(BLOCK_HIDDEN)'));
  assert.ok(home.includes('overlay.sync()'), 'and re-measure, in case the window moved while it was up');
});

test('app.js blocks the overlay while the mixer drawer is open', () => {
  const src = codeOnly(ui('app.js'));
  const at = src.indexOf('onOpenChange:');
  assert.ok(at > 0, 'app.js must listen for the drawer opening');
  const handler = src.slice(at, src.indexOf('onStatus:', at));
  assert.ok(handler.includes('overlay.block(BLOCK_MIXER)'));
  assert.ok(handler.includes('overlay.unblock(BLOCK_MIXER)'));
});

test('the mixer host announces both edges, including a drawer that closes itself', () => {
  // The drawer closes from its own Close button, the scrim and Escape, and app.js
  // is told about none of them directly. Without this the picture would stay
  // hidden for the rest of the match after one Escape.
  const src = codeOnly(ui('mixerhost.js'));
  assert.match(src, /onOpenChange/, 'the host must accept the callback');

  const open = src.slice(src.indexOf('function openMixer()'));
  const openBody = open.slice(0, open.indexOf('\n  }'));
  assert.ok(openBody.includes('announceOpen(true)'), 'opening must be announced');
  assert.ok(
    openBody.indexOf('announceOpen(true)') < openBody.indexOf('drawer.open()'),
    'and announced BEFORE the drawer paints, because that is the dangerous direction',
  );

  const close = src.slice(src.indexOf('function closeMixer()'));
  assert.ok(close.slice(0, close.indexOf('\n  }')).includes('announceOpen(false)'));

  const poll = src.slice(src.indexOf('async function pollMixer()'));
  assert.ok(
    poll.slice(0, poll.indexOf('\n  }')).includes('announceOpen(false)'),
    'the poll is the only thing that notices a drawer closed from inside itself',
  );
});

test('the overlay rectangle is re-reported on resize, on a box change and on a DPI change', () => {
  const src = codeOnly(ui('app.js'));
  const fn = src.slice(src.indexOf('function watchPictureRect()'));
  const body = fn.slice(0, fn.indexOf('\n  }'));
  assert.match(body, /window\.addEventListener\('resize'/, 'the window changing size');
  assert.match(body, /new ResizeObserver\(sync\)\.observe\(home\.pictureEl\)/, 'the box itself changing size');
  assert.match(body, /watchDevicePixelRatio\(sync\)/, 'and the display scaling changing');

  // The DPI watch has to RE-ARM: a media query pinned to the current ratio stops
  // matching the moment the ratio moves, so a single listener fires once and
  // then never again.
  const dpi = src.slice(src.indexOf('function watchDevicePixelRatio(onChange)'));
  const dpiBody = dpi.slice(0, dpi.indexOf('\n  }'));
  assert.match(dpiBody, /dppx/, 'pinned to the current ratio');
  assert.match(dpiBody, /arm\(\);/, 'and re-armed against the new one');
});

test('the conversion to physical pixels happens in exactly one place', () => {
  // Two conversions is two chances to use the wrong one, and the wrong one is
  // invisible: a picture in the wrong place with everything reporting success.
  for (const file of ['app.js', 'home.js']) {
    const src = codeOnly(ui(file));
    assert.ok(
      !/devicePixelRatio\s*\*/.test(src) && !/\*\s*devicePixelRatio/.test(src),
      `${file} must not multiply by devicePixelRatio itself — overlay.js owns that`,
    );
  }
  // home.js measures and does not convert: it hands app.js raw CSS pixels.
  const home = codeOnly(ui('home.js'));
  assert.match(home, /function measurePictureRect\(\)/);
  assert.ok(!home.includes('devicePixelRatio'), 'home.js must have no opinion about device pixels');
});
