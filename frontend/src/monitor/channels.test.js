/**
 * Tests for the return channel selector's routing table.
 *
 * Run with:  node --test "src/monitor/*.test.js"
 *
 * Owner: WP-5a.
 *
 * ======================= WHAT THESE ARE FOR =================================
 *
 * There is one way to get this wrong that produces a control which looks
 * perfect, meters correctly, and leaves a commentator monitoring on one ear:
 * implementing "Left only" as "silence the right OUTPUT" instead of "route the
 * left SOURCE to both outputs".
 *
 * Both make the level meter show signal on the left. Both are described by the
 * same three words in the UI. One of them is a commentator with a dead ear in a
 * pair of single-ear cans, thirty seconds before kick-off, with no way to tell
 * whether the fault is the button or the headphones.
 *
 * So every assertion below is about WHICH SOURCE CHANNEL REACHES WHICH OUTPUT.
 * None is about a gain value, because a gain value cannot tell the two apart.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  CHANNEL_STEREO,
  CHANNEL_LEFT,
  CHANNEL_RIGHT,
  CHANNEL_MODES,
  DEFAULT_CHANNEL_MODE,
  isValidChannelMode,
  normaliseChannelMode,
  sourceChannelsForOutputs,
  channelRouting,
  describeChannelMode,
  channelModeLabel,
} from './channels.js';

test('stereo passes both source channels straight through', () => {
  assert.deepEqual(sourceChannelsForOutputs(CHANNEL_STEREO), [0, 1]);
  assert.deepEqual(channelRouting(CHANNEL_STEREO).map((r) => ({ ...r })), [
    { from: 0, to: 0 },
    { from: 1, to: 1 },
  ]);
});

test('left only puts the LEFT SOURCE channel in BOTH ears', () => {
  // Not "silence the right ear". Source 0 reaches output 0 AND output 1.
  assert.deepEqual(sourceChannelsForOutputs(CHANNEL_LEFT), [0, 0]);
  assert.deepEqual(channelRouting(CHANNEL_LEFT).map((r) => ({ ...r })), [
    { from: 0, to: 0 },
    { from: 0, to: 1 },
  ]);
});

test('right only puts the RIGHT SOURCE channel in BOTH ears', () => {
  assert.deepEqual(sourceChannelsForOutputs(CHANNEL_RIGHT), [1, 1]);
  assert.deepEqual(channelRouting(CHANNEL_RIGHT).map((r) => ({ ...r })), [
    { from: 1, to: 0 },
    { from: 1, to: 1 },
  ]);
});

test('no mode ever leaves an output channel unfed', () => {
  // This is the one-dead-ear failure stated as an invariant rather than as
  // three separate cases, so a fourth mode added later cannot reintroduce it.
  for (const mode of CHANNEL_MODES) {
    const routing = channelRouting(mode.value);
    const fedOutputs = new Set(routing.map((r) => r.to));
    assert.ok(fedOutputs.has(0), `${mode.value} feeds the left output`);
    assert.ok(fedOutputs.has(1), `${mode.value} feeds the right output`);
    assert.equal(routing.length, 2, `${mode.value} makes exactly one connection per output`);
  }
});

test('a single-channel mode uses ONE source channel for both outputs', () => {
  for (const mode of [CHANNEL_LEFT, CHANNEL_RIGHT]) {
    const sources = new Set(sourceChannelsForOutputs(mode));
    assert.equal(sources.size, 1, `${mode} draws both ears from one source channel`);
  }
  assert.equal(new Set(sourceChannelsForOutputs(CHANNEL_STEREO)).size, 2, 'stereo draws from both');
});

test('every source channel referenced exists on a two-channel splitter', () => {
  for (const mode of CHANNEL_MODES) {
    for (const { from, to } of channelRouting(mode.value)) {
      assert.ok(from === 0 || from === 1, `${mode.value} reads source channel ${from}`);
      assert.ok(to === 0 || to === 1, `${mode.value} writes output channel ${to}`);
    }
  }
});

test('stereo is the default, so nobody who ignores the control is changed', () => {
  assert.equal(DEFAULT_CHANNEL_MODE, CHANNEL_STEREO);
  assert.deepEqual(sourceChannelsForOutputs(DEFAULT_CHANNEL_MODE), [0, 1]);
});

test('the three modes are the whole control, in drawing order', () => {
  assert.deepEqual(
    CHANNEL_MODES.map((m) => m.value),
    [CHANNEL_STEREO, CHANNEL_LEFT, CHANNEL_RIGHT],
  );
  for (const mode of CHANNEL_MODES) {
    assert.ok(mode.label.length > 0, `${mode.value} has a label`);
    assert.ok(mode.hint.length > 0, `${mode.value} has a hint`);
  }
});

test('the hints say "both ears", because that is the thing that is not obvious', () => {
  const left = CHANNEL_MODES.find((m) => m.value === CHANNEL_LEFT);
  const right = CHANNEL_MODES.find((m) => m.value === CHANNEL_RIGHT);
  assert.match(left.hint, /both ears/i, 'the Left hint must say both ears');
  assert.match(right.hint, /both ears/i, 'the Right hint must say both ears');
});

test('the Right hint warns that a mono bus is silence', () => {
  // A ChannelSplitter up-mixes discretely: a mono track has digital silence on
  // channel 1. There is no API that reports a remote track's true channel
  // count, so this cannot be detected and turned into a warning at runtime —
  // saying it up front is the only honest option available.
  const right = CHANNEL_MODES.find((m) => m.value === CHANNEL_RIGHT);
  assert.match(right.hint, /mono/i);
  assert.match(right.hint, /silence/i);
});

test('anything unroutable normalises to a mode that exists', () => {
  // A mode with no routing entry wires the splitter to nothing, and the symptom
  // of that is silence — which is the symptom of every other fault too.
  for (const bad of [undefined, null, '', 'centre', 'LEFT', 0, 1, {}, []]) {
    const got = normaliseChannelMode(bad);
    assert.ok(isValidChannelMode(got), `${JSON.stringify(bad)} -> ${got}, which is routable`);
    assert.equal(channelRouting(got).length, 2);
  }
  assert.equal(normaliseChannelMode('nonsense', CHANNEL_RIGHT), CHANNEL_RIGHT, 'the fallback is honoured');
  assert.equal(normaliseChannelMode('nonsense', 'also nonsense'), DEFAULT_CHANNEL_MODE, 'a bad fallback is not');
});

test('isValidChannelMode accepts exactly the three', () => {
  for (const good of [CHANNEL_STEREO, CHANNEL_LEFT, CHANNEL_RIGHT]) {
    assert.equal(isValidChannelMode(good), true);
  }
  for (const bad of ['both', 'l', 'r', 'mono', undefined, 2]) {
    assert.equal(isValidChannelMode(bad), false, `${String(bad)} is not a channel mode`);
  }
});

test('describeChannelMode states the routing, not the label', () => {
  // It goes in support logs. "left" tells nobody anything; "source L -> left
  // ear, source L -> right ear" is the thing that was actually wired.
  assert.match(describeChannelMode(CHANNEL_LEFT), /source L -> left ear, source L -> right ear/);
  assert.match(describeChannelMode(CHANNEL_RIGHT), /source R -> left ear, source R -> right ear/);
  assert.match(describeChannelMode(CHANNEL_STEREO), /source L -> left ear, source R -> right ear/);
});

test('channelModeLabel never returns a blank segment', () => {
  for (const mode of CHANNEL_MODES) {
    assert.equal(channelModeLabel(mode.value), mode.label);
  }
  assert.notEqual(channelModeLabel('centre'), '');
  assert.match(channelModeLabel('centre'), /not one of the three/);
});

test('the table and the routing are frozen', () => {
  // CHANNEL_MODES is iterated by two screens and the routing is read on every
  // switch; a mutation from either would be invisible until somebody's ears
  // noticed.
  assert.ok(Object.isFrozen(CHANNEL_MODES));
  for (const mode of CHANNEL_MODES) assert.ok(Object.isFrozen(mode));
  assert.ok(Object.isFrozen(channelRouting(CHANNEL_LEFT)));
});
