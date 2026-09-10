import test from 'node:test';
import assert from 'node:assert/strict';

import { readPref, writePref, PREF_RETURN_LEVEL, PREF_MONITOR_RAIL_COLLAPSED } from './prefs.js';

function fakeStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    map: m,
  };
}

test('a preference round-trips as JSON', () => {
  const s = fakeStorage();
  assert.equal(writePref(PREF_RETURN_LEVEL, 0.1, s), true);
  assert.equal(readPref(PREF_RETURN_LEVEL, s), 0.1);
  assert.equal(writePref(PREF_MONITOR_RAIL_COLLAPSED, true, s), true);
  assert.equal(readPref(PREF_MONITOR_RAIL_COLLAPSED, s), true);
});

test('nothing stored, no storage, or a broken store all read as null and write as false', () => {
  assert.equal(readPref(PREF_RETURN_LEVEL, fakeStorage()), null);
  assert.equal(readPref(PREF_RETURN_LEVEL, null), null);
  assert.equal(writePref(PREF_RETURN_LEVEL, 1, null), false);
  const broken = {
    getItem: () => {
      throw new Error('SecurityError');
    },
    setItem: () => {
      throw new Error('QuotaExceededError');
    },
  };
  assert.equal(readPref(PREF_RETURN_LEVEL, broken), null);
  assert.equal(writePref(PREF_RETURN_LEVEL, 1, broken), false);
  const corrupt = fakeStorage();
  corrupt.setItem(PREF_RETURN_LEVEL, '{not json');
  assert.equal(readPref(PREF_RETURN_LEVEL, corrupt), null);
});

test('the keys are namespaced and distinct', () => {
  assert.ok(PREF_RETURN_LEVEL.startsWith('wslcomms.'));
  assert.ok(PREF_MONITOR_RAIL_COLLAPSED.startsWith('wslcomms.'));
  assert.notEqual(PREF_RETURN_LEVEL, PREF_MONITOR_RAIL_COLLAPSED);
});
