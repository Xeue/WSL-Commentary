import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const ui = (name) => readFileSync(join(here, name), 'utf8');

// The PGM monitor window's controls column folds away like the application's,
// and the return level is remembered and re-applied. These read the source
// the way homelayout.test.js does: the wiring is the fact under test.

test('the monitor rail collapses with the same classes the application uses, so the same CSS applies', () => {
  const src = ui('monitorview.js');
  for (const cls of ['rail-header', 'rail-collapse', 'rail-strip', 'rail-expand', 'rail-strip-label', 'home-rail-collapsed']) {
    assert.ok(src.includes(cls), `monitorview.js must use ${cls}, which is what main.css styles for the application's rail`);
  }
  assert.ok(src.includes("el.className = 'view view-home view-monitor'"), 'the view must carry view-home: the collapse rules are .view-home.home-rail-collapsed');
  assert.ok(src.includes("rail.setAttribute('aria-expanded'"), 'the fold is announced');
});

test('the fold is remembered per machine and read back on the next launch', () => {
  const src = ui('monitorview.js');
  assert.ok(src.includes('PREF_MONITOR_RAIL_COLLAPSED'), 'the preference key is used');
  assert.ok(/readPref\(PREF_MONITOR_RAIL_COLLAPSED\)\s*===\s*true/.test(src), 'the initial state comes from the preference');
  assert.ok(src.includes('writePref(PREF_MONITOR_RAIL_COLLAPSED, true)'), 'folding is written');
  assert.ok(src.includes('writePref(PREF_MONITOR_RAIL_COLLAPSED, false)'), 'unfolding is written');
});

test('an alert count survives the fold on the strip', () => {
  const src = ui('monitorview.js');
  assert.ok(src.includes('rail-strip-attention'), 'the strip carries the count');
  assert.ok(src.includes('railStripCount.textContent = String(entries.length)'), 'and it is the number of entries');
});

test('the return level is remembered and every monitor is built at it', () => {
  const app = ui('app.js');
  assert.ok(app.includes('let currentLevel = restoreLevel();'), 'the page holds the level');
  assert.ok(app.includes('writePref(PREF_RETURN_LEVEL, fraction)'), 'a change is remembered');
  assert.ok(app.includes('level: currentLevel,'), 'createMonitor is given the level');
  assert.ok(app.includes('safeMonitorCall((m) => m.setLevel(currentLevel))'), 'and the setter re-applies it after construction');
  assert.ok(!app.includes('home.setLevel(1);'), 'the slider no longer starts at full on every launch');
  assert.ok(app.includes('home.setLevel(currentLevel);'), 'it starts where it was left');
  assert.ok(app.includes('return sliderToLevel(DEFAULT_LEVEL_POSITION);'), 'a never-set machine starts at the taper default, not full');
});

test('the panel slider is the decibel taper with a readout', () => {
  const panel = ui('pgmpanel.js');
  assert.ok(panel.includes("from './levelmap.js'"), 'the taper is levelmap.js');
  assert.ok(panel.includes('handlers.onLevelChange(sliderToLevel(Number(levelSlider.value)))'), 'what leaves is the linear multiplier');
  assert.ok(panel.includes('levelSlider.value = String(levelToSlider(fraction));'), 'setLevel maps back through the taper');
  assert.ok(panel.includes("levelReadout.className = 'level-readout'"), 'the readout exists');
  assert.ok(!panel.includes("levelSlider.value = '100';"), 'no hard-coded full start');
});
