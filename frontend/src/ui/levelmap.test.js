import test from 'node:test';
import assert from 'node:assert/strict';

import {
  sliderToLevel,
  levelToSlider,
  describeLevelPosition,
  positionToDb,
  DEFAULT_LEVEL_POSITION,
  LEVEL_RANGE_DB,
} from './levelmap.js';

const close = (a, b, eps = 1e-9) => Math.abs(a - b) < eps;

test('the top of the slider is the full make-up gain, exactly as 100 always was', () => {
  assert.equal(sliderToLevel(100), 1);
  assert.equal(positionToDb(100), 0);
  assert.equal(describeLevelPosition(100), '0 dB');
});

test('the bottom is silence, and it says so', () => {
  assert.equal(sliderToLevel(0), 0);
  assert.equal(positionToDb(0), -Infinity);
  assert.equal(describeLevelPosition(0), 'Mute');
});

test('the taper is decibels: 50 is -20 dB, 75 is -10 dB, and the default is 75', () => {
  assert.ok(close(sliderToLevel(50), 0.1), `50 -> ${sliderToLevel(50)}`);
  assert.equal(describeLevelPosition(50), '-20.0 dB');
  assert.ok(close(sliderToLevel(75), Math.pow(10, -0.5)));
  assert.equal(describeLevelPosition(75), '-10.0 dB');
  assert.equal(DEFAULT_LEVEL_POSITION, 75);
  assert.equal(LEVEL_RANGE_DB, 40);
  // The last position above mute is not a cliff.
  assert.equal(describeLevelPosition(1), '-39.6 dB');
});

test('the taper is monotonic with no dead zone', () => {
  let prev = -1;
  for (let pos = 0; pos <= 100; pos++) {
    const level = sliderToLevel(pos);
    assert.ok(level > prev, `position ${pos} (${level}) must be louder than ${pos - 1} (${prev})`);
    prev = level;
  }
});

test('levelToSlider is the inverse on every position', () => {
  for (let pos = 0; pos <= 100; pos++) {
    assert.equal(levelToSlider(sliderToLevel(pos)), pos, `round trip of ${pos}`);
  }
});

test('a level the slider cannot show still lands somewhere sane', () => {
  assert.equal(levelToSlider(0.0001), 0, 'under -40 dB reads as mute');
  assert.equal(levelToSlider(2), 100, 'over unity clamps to the top');
  assert.equal(levelToSlider(NaN), 0);
  assert.equal(levelToSlider('0.1'), 50, 'a string number is a number');
  assert.equal(sliderToLevel('abc'), 0, 'garbage is silence, never full gain (gain.js clampLevel says why)');
  assert.equal(sliderToLevel(250), 1, 'over the top clamps to the top');
});
