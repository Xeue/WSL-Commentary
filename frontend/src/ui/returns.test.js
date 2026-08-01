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

  assert.equal(RETURN_BUSES.length, 7, 'there are seven audio tracks');
  RETURN_BUSES.forEach((bus, i) => {
    assert.equal(bus.mid, want[i].mid, `entry ${i} is mid ${want[i].mid}`);
    assert.equal(bus.name, want[i].name, `mid ${want[i].mid} is ${want[i].name}`);
    assert.ok(bus.label.includes(bus.name), `mid ${bus.mid} label names ${bus.name}`);
    assert.ok(
      !/^mid\s/i.test(bus.label),
      `mid ${bus.mid} label "${bus.label}" leads with its mid number; the labels are name-only`,
    );
  });
});

test('no label carries the mixer bus names that used to be here', () => {
  // These are the MIXER's bus vocabulary. They are correct names for buses and
  // wrong names for the monitor's audio tracks, and offering them here sent a
  // commentator hunting for a clean return through five options named after
  // something else.
  //
  // mid 1 and mid 2 keep their bus names in parentheses on purpose: "PGM
  // (master)" and "CLN (aux1)" are the two the operator has to match against
  // the mixer surface, and those two mappings are not in doubt.
  for (const bus of RETURN_BUSES) {
    if (bus.mid <= 2) continue;
    for (const wrong of ['aux1', 'aux2', 'mon1', 'mon2', 'mon3', 'mon4']) {
      assert.ok(
        !bus.label.includes(wrong),
        `mid ${bus.mid} is labelled "${bus.label}", which names the mixer bus ${wrong}`,
      );
    }
  }
});

test('mids 4 to 6 say they are mix-minus feeds, and name their MIC input', () => {
  for (const mid of [4, 5, 6]) {
    const label = returnLabel(mid);
    assert.match(label, /mix-minus/i, `mid ${mid} says it is a mix-minus feed`);
    assert.match(label, new RegExp(`MIC${mid - 3}`), `mid ${mid} names the MIC input it is derived from`);
  }
});

test('mid 2 is still the default', () => {
  assert.equal(DEFAULT_RETURN_MID, 2);
  assert.equal(RETURN_BUSES.find((b) => b.mid === DEFAULT_RETURN_MID).name, 'CLN');
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
