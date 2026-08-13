/**
 * Tests for the Return dropdown's seven audio tracks.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * The bug these exist to prevent has already happened once. The table was
 * written out twice — home.js and settings.js — and both copies named mids 3 to
 * 7 after the MIXER's buses ("aux2", "mon1", "mon2", "mon3", "mon4 (PFL)")
 * rather than the monitor's audio tracks. The two copies agreed with each
 * other, which is exactly why nobody noticed they were both wrong.
 *
 * So there are two kinds of test here: the contents of the table, and the fact
 * that there is only one of it.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  RETURN_BUSES,
  DEFAULT_RETURN_MID,
  isValidReturnMid,
  returnLabel,
} from './returns.js';

const here = dirname(fileURLToPath(import.meta.url));
const read = (name) => readFileSync(join(here, name), 'utf8');

test('the seven tracks are the M2L-X bundle enum, in mid order', () => {
  const want = [
    { mid: 1, name: 'PGM' },
    { mid: 2, name: 'CLN' },
    { mid: 3, name: 'MON' },
    { mid: 4, name: 'MIC1' },
    { mid: 5, name: 'MIC2' },
    { mid: 6, name: 'MIC3' },
    { mid: 7, name: 'PFL' },
  ];

  // `name` stays the M2L-X enum (PGM/CLN/MON/MIC1..3/PFL); `label` is the
  // facility's chosen display text and no longer echoes the enum.
  const wantLabels = ['Program Dirty', 'Program Clean', 'Aux', 'Monitor 1', 'Monitor 2', 'Monitor 3', 'PFL'];
  assert.equal(RETURN_BUSES.length, 7, 'there are seven audio tracks');
  RETURN_BUSES.forEach((bus, i) => {
    assert.equal(bus.mid, want[i].mid, `entry ${i} is mid ${want[i].mid}`);
    assert.equal(bus.name, want[i].name, `mid ${want[i].mid} is ${want[i].name}`);
    assert.equal(bus.label, wantLabels[i], `mid ${bus.mid} is labelled ${wantLabels[i]}`);
  });
});

test('the seven labels are the facility names the operator set, in mid order', () => {
  // The display labels are the operator's own vocabulary, deliberately NOT the
  // M2L-X enum: PGM shows as "Program Dirty", CLN as "Program Clean", MON as
  // "Aux", and the three MIC mix-minus feeds as "Monitor 1/2/3".
  assert.deepEqual(
    RETURN_BUSES.map((b) => b.label),
    ['Program Dirty', 'Program Clean', 'Aux', 'Monitor 1', 'Monitor 2', 'Monitor 3', 'PFL'],
  );
});

test('mids 4 to 6 keep their MIC enum names even though the labels read "Monitor"', () => {
  // The labels say "Monitor 1/2/3", but these are still the MIC mix-minus (N-1)
  // feeds — the enum name records that, and the file comment explains why it
  // matters (silence on an unused MIC input is the feed not existing).
  for (const mid of [4, 5, 6]) {
    const bus = RETURN_BUSES.find((b) => b.mid === mid);
    assert.equal(bus.name, `MIC${mid - 3}`, `mid ${mid} is MIC${mid - 3}`);
    assert.equal(bus.label, `Monitor ${mid - 3}`, `mid ${mid} is labelled Monitor ${mid - 3}`);
    assert.match(returnLabel(mid), /^Monitor [123]$/);
  }
});

test('the default return is mid 4, MIC1 / "Monitor 1"', () => {
  assert.equal(DEFAULT_RETURN_MID, 4);
  const def = RETURN_BUSES.find((b) => b.mid === DEFAULT_RETURN_MID);
  assert.equal(def.name, 'MIC1');
  assert.equal(def.label, 'Monitor 1');
});

test('the valid range is 1..7, unchanged — config.ReturnMid agrees', () => {
  for (const mid of [1, 2, 3, 4, 5, 6, 7]) {
    assert.equal(isValidReturnMid(mid), true, `mid ${mid} is valid`);
  }
  for (const mid of [0, 8, -1, 2.5, NaN, null, undefined, 'two']) {
    assert.equal(isValidReturnMid(mid), false, `${String(mid)} is not a valid mid`);
  }
});

test('returnLabel never returns a blank, which would read as "no return"', () => {
  for (const mid of [0, 8, -1, NaN]) {
    const label = returnLabel(mid);
    assert.notEqual(label, '');
    assert.match(label, /not one of the seven/);
  }
});

test('both screens import the one table instead of restating it', () => {
  // This is the actual regression. Two hand-written copies is how mids 3 to 7
  // came to be wrong in two places at once, and a test on the table alone would
  // not have caught it.
  for (const file of ['home.js', 'settings.js']) {
    const src = read(file);
    assert.match(src, /from '\.\/returns\.js'/, `${file} imports the shared table`);
    assert.ok(
      !/mid 3 —|mid 4 —|mid 5 —|mid 6 —|mid 7 —/.test(src),
      `${file} still spells out its own dropdown labels; there must be exactly one table`,
    );
  }
});
