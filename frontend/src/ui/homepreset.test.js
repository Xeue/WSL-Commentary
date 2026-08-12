/**
 * Tests for the home-header active-preset indicator: the setters home.js
 * exposes, the SENDING gate on the selector, and the wiring in app.js that
 * pushes presets to the header and reuses the ONE apply path.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= WHY THIS READS SOURCE ==============================
 *
 * home.js and app.js build against the real DOM, and package.json is frozen —
 * no jsdom. So the properties that matter are asserted on SOURCE TEXT, the same
 * class of evidence mixerwiring.test.js, presets.test.js and remotewiring.test.js
 * already use. The comment-stripper keeps prose ABOUT a call from satisfying a
 * guard about the call itself.
 *
 * The properties pinned here are the ones a refactor could silently break:
 *
 *   - the header selector is DISABLED while SENDING, because ApplyPreset is
 *     refused server-side mid-send and a control that only ever fails must say
 *     why, not throw when pressed (the same rule the Settings Apply button
 *     follows);
 *   - app.js pushes both the list AND the active id to the header, on startup,
 *     after an apply, and on another seat's save;
 *   - there is exactly ONE call site for backend.applyPreset — the header
 *     switch reuses applyPresetAndRefresh, it does not open a second apply path.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const ui = (name) => readFileSync(join(here, name), 'utf8');

/** Strips comments, so a note discussing a call cannot satisfy a guard about it. */
function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

// ---------------------------------------------------------------------------
// home.js: the indicator, its setters, and the SENDING gate
// ---------------------------------------------------------------------------

test('home.js exposes setPresets and setActivePreset', () => {
  const js = ui('home.js');
  assert.match(js, /function setPresets\(list\)/, 'home.js must define setPresets');
  assert.match(js, /function setActivePreset\(id\)/, 'home.js must define setActivePreset');
  // And hand both out on the view object, or app.js cannot drive the indicator.
  const ret = js.slice(js.indexOf('return {'), js.length);
  assert.match(ret, /\bsetPresets,/, 'setPresets must be on the returned view');
  assert.match(ret, /\bsetActivePreset,/, 'setActivePreset must be on the returned view');
});

test('the header preset selector is gated on the running state, with a reason', () => {
  const js = ui('home.js');
  // setRunning is the one place the SENDING state arrives, and it must feed the
  // indicator's gate — the same derivation the START/STOP button uses.
  const setRunning = js.slice(js.indexOf('function setRunning(running)'), js.indexOf('function setBusy'));
  assert.ok(setRunning.length > 0, 'home.js must still define setRunning');
  assert.match(setRunning, /presetSendingNow = running === true/, 'setRunning must record the sending state');
  assert.match(setRunning, /renderPresetIndicator\(\)/, 'and re-render the indicator so the gate takes effect');

  // The render disables the select on that state, and puts the reason on it.
  const render = js.slice(js.indexOf('function renderPresetIndicator()'), js.indexOf('function setPresets'));
  assert.match(render, /presetIndicatorSelect\.disabled = presetSendingNow/, 'the select must be disabled while sending');
  assert.match(render, /Disabled while SENDING/, 'the reason must be on the control, not only in a log');
});

test('the indicator is quiet: hidden until there is a saved instance', () => {
  const js = ui('home.js');
  const render = js.slice(js.indexOf('function renderPresetIndicator()'), js.indexOf('function setPresets'));
  assert.match(
    render,
    /if \(presetList\.length === 0\) \{\s*presetIndicator\.hidden = true;/,
    'no saved instances must hide the whole indicator — it costs no attention until it has one',
  );
});

test('the selector change raises onPresetChange with the chosen id', () => {
  const js = codeOnly(ui('home.js'));
  assert.match(
    js,
    /presetIndicatorSelect\.addEventListener\('change'[\s\S]*?handlers\.onPresetChange\(presetIndicatorSelect\.value\)/,
    'switching in the header must call the onPresetChange handler',
  );
});

// ---------------------------------------------------------------------------
// app.js: the wiring, and the single apply path
// ---------------------------------------------------------------------------

test('app.js pushes both the preset list and the active id to the header', () => {
  const js = ui('app.js');
  const refresh = js.slice(js.indexOf('async function refreshHomePresets()'), js.indexOf('// --- monitor'));
  assert.ok(refresh.length > 0, 'app.js must have refreshHomePresets');
  assert.match(refresh, /backend\.listPresets\(\)/, 'it must read the saved instances');
  assert.match(refresh, /backend\.getActivePreset\(\)/, 'and which one is active');
  assert.match(refresh, /home\.setPresets\(/, 'and push the list to the header');
  assert.match(refresh, /home\.setActivePreset\(/, 'and mark the active one');
});

test('app.js refreshes the indicator on startup, after an apply, and on a remote save', () => {
  const js = codeOnly(ui('app.js'));
  // Three call sites: init, applyPresetAndRefresh, the onConfig handler. Counting
  // is enough to catch any one of them being dropped.
  const calls = (js.match(/refreshHomePresets\(\)/g) || []).length;
  assert.ok(calls >= 4, `expected refreshHomePresets to be called on startup, after apply, on ` +
    `remote save (and defined once); found ${calls} references`);

  // The config event refresh, specifically — a remote or Settings apply changes
  // the active instance and the event does not carry it.
  const onConfig = js.slice(js.indexOf('backend.onConfig('), js.indexOf('backend.onRemote('));
  assert.match(onConfig, /refreshHomePresets\(\)/, 'the config event must refresh the header indicator');
});

test('the header switch reuses the ONE apply path — no second applyPreset call site', () => {
  const js = codeOnly(ui('app.js'));

  // Exactly one place calls backend.applyPreset: applyPresetAndRefresh. The
  // header handler calls applyPresetAndRefresh, not backend.applyPreset again.
  const applyCalls = (js.match(/backend\.applyPreset\(/g) || []).length;
  assert.equal(applyCalls, 1, 'backend.applyPreset must have exactly one call site (applyPresetAndRefresh)');

  const handler = js.slice(js.indexOf('async function onHomePresetChange(id)'), js.indexOf('async function refreshHomePresets()'));
  assert.ok(handler.length > 0, 'app.js must define onHomePresetChange');
  assert.match(
    handler,
    /applyPresetAndRefresh\(id\)/,
    'the header switch must go through the shared apply sequence, not a copy of it',
  );
  assert.ok(
    !/backend\.applyPreset\(/.test(handler),
    'onHomePresetChange must NOT open a second apply path',
  );
});

test('home is given the onPresetChange handler', () => {
  const js = ui('app.js');
  const home = js.slice(js.indexOf('const home = createHomeView('), js.indexOf('});', js.indexOf('const home = createHomeView(')));
  assert.match(home, /onPresetChange:\s*onHomePresetChange/, 'app.js must wire the header switch handler');
});
