/**
 * Tests for the return-audio graph: the channel selector and the mute.
 *
 * Run with:  node --test "src/monitor/*.test.js"
 *
 * Owner: WP-5a.
 *
 * Node has no Web Audio, so these use hand-written fakes in the same spirit as
 * peer.test.js: enough to record WHAT WAS WIRED TO WHAT, which is the whole of
 * what these two features are. Whether Chromium then produces sound belongs to
 * harness.html.
 *
 * The two properties under test are the two that fail silently:
 *
 *   1. "Left only" must route the LEFT SOURCE channel to BOTH outputs. The
 *      wrong implementation — silencing one output — looks identical on a
 *      meter and leaves a commentator on one ear. channels.test.js pins the
 *      table; this pins the graph that is built from it.
 *
 *   2. Selecting the SRT return must make this path genuinely silent. Not
 *      quiet: severed. A gain of zero is one ramp away from being a second copy
 *      of the programme, at a different offset, in the ears of somebody talking
 *      over it.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createReturnAudio } from './audio.js';
import { CHANNEL_STEREO, CHANNEL_LEFT, CHANNEL_RIGHT } from './channels.js';

// --------------------------------------------------------------------------
// Fakes
// --------------------------------------------------------------------------

/** FakeNode records its outgoing edges, including the channel indices. */
class FakeNode {
  constructor(kind) {
    this.kind = kind;
    /** @type {Array<{target: FakeNode, from: number|undefined, to: number|undefined}>} */
    this.edges = [];
  }
  connect(target, from, to) {
    this.edges.push({ target, from, to });
    return target;
  }
  disconnect(target) {
    if (target === undefined) this.edges = [];
    else this.edges = this.edges.filter((e) => e.target !== target);
  }
  /** edgesTo returns the {from,to} pairs reaching a given node. */
  edgesTo(target) {
    return this.edges.filter((e) => e.target === target).map(({ from, to }) => ({ from, to }));
  }
  connectsTo(target) {
    return this.edges.some((e) => e.target === target);
  }
}

class FakeGainNode extends FakeNode {
  constructor() {
    super('gain');
    this.gain = {
      value: 1,
      setTargetAtTime(v) {
        this.value = v;
      },
    };
  }
}

let contextsCreated = 0;

class FakeAudioContext {
  constructor(opts) {
    contextsCreated += 1;
    this.options = opts;
    this.state = 'running';
    this.currentTime = 0;
    this.sampleRate = 48000;
    this.closed = false;
    /** @type {FakeNode[]} */
    this.nodes = [];
  }
  #track(node) {
    this.nodes.push(node);
    return node;
  }
  createChannelSplitter(n) {
    const node = this.#track(new FakeNode('splitter'));
    node.numberOfOutputs = n;
    return node;
  }
  createChannelMerger(n) {
    const node = this.#track(new FakeNode('merger'));
    node.numberOfInputs = n;
    return node;
  }
  createGain() {
    return this.#track(new FakeGainNode());
  }
  createMediaStreamDestination() {
    const node = this.#track(new FakeNode('destination'));
    node.stream = { id: 'dest-stream' };
    return node;
  }
  createMediaStreamSource(stream) {
    const node = this.#track(new FakeNode('source'));
    node.stream = stream;
    return node;
  }
  async resume() {
    this.state = 'running';
  }
  async close() {
    this.closed = true;
  }
  find(kind) {
    return this.nodes.find((n) => n.kind === kind);
  }
}

/** FakeMediaElement is the surface audio.js touches on an <audio>. */
class FakeMediaElement {
  constructor() {
    this.srcObject = null;
    this.autoplay = false;
    this.muted = false;
    this.defaultMuted = false;
    this.volume = 1;
    this.playCalls = 0;
    this.pauseCalls = 0;
    this.playing = false;
    this.sinkId = null;
    this.attributes = {};
  }
  setAttribute(k, v) {
    this.attributes[k] = v;
  }
  play() {
    this.playCalls += 1;
    this.playing = true;
    return Promise.resolve();
  }
  pause() {
    this.pauseCalls += 1;
    this.playing = false;
  }
  setSinkId(id) {
    this.sinkId = id;
    return Promise.resolve();
  }
}

/** installEnvironment puts the globals audio.js reaches for in place. */
function installEnvironment() {
  const created = [];
  globalThis.AudioContext = FakeAudioContext;
  globalThis.MediaStream = class MediaStream {
    constructor(tracks) {
      this.tracks = tracks || [];
    }
  };
  globalThis.document = {
    createElement() {
      const el = new FakeMediaElement();
      created.push(el);
      return el;
    },
    addEventListener() {},
    removeEventListener() {},
  };
  return created;
}

const audioTrack = () => ({ kind: 'audio', id: 'return-track' });

/**
 * buildWithGraph makes a return-audio chain, attaches a track, and hands back
 * the fake nodes it built. Every test starts from a routed, playing path,
 * because that is the only state in which either feature means anything.
 *
 * The chain does not expose its nodes, so the AudioContext is captured by
 * subclassing the constructor for the duration of the build.
 */
async function buildWithGraph(opts = {}) {
  const createdElements = installEnvironment();
  /** @type {FakeAudioContext[]} */
  const contexts = [];
  globalThis.AudioContext = class extends FakeAudioContext {
    constructor(o) {
      super(o);
      contexts.push(this);
    }
  };
  const audioEl = new FakeMediaElement();
  const errors = [];
  const ra = createReturnAudio({ audioEl, gainDb: 18, onError: (e) => errors.push(e), ...opts });
  await ra.attach(audioTrack());
  const ctx = contexts[0];
  return {
    ra,
    audioEl,
    errors,
    contexts,
    createdElements,
    ctx,
    splitter: ctx.find('splitter'),
    merger: ctx.find('merger'),
    gain: ctx.find('gain'),
    dest: ctx.find('destination'),
    source: ctx.find('source'),
  };
}

// --------------------------------------------------------------------------
// The channel selector, in the graph
// --------------------------------------------------------------------------

test('the track goes into the SPLITTER, not straight into the gain', () => {
  // If the source were wired to the gain the channel control would have nothing
  // to act on, and every mode would sound identical.
  return buildWithGraph().then(({ source, splitter, merger, gain, dest }) => {
    assert.ok(source.connectsTo(splitter), 'source -> splitter');
    assert.ok(!source.connectsTo(gain), 'source must not bypass the splitter');
    assert.ok(merger.connectsTo(gain), 'merger -> gain');
    assert.ok(gain.connectsTo(dest), 'gain -> destination');
  });
});

test('stereo wires source L to the left output and source R to the right', async () => {
  const { splitter, merger } = await buildWithGraph({ channelMode: CHANNEL_STEREO });
  assert.deepEqual(splitter.edgesTo(merger), [
    { from: 0, to: 0 },
    { from: 1, to: 1 },
  ]);
});

test('left only wires source L to BOTH outputs', async () => {
  const { splitter, merger } = await buildWithGraph({ channelMode: CHANNEL_LEFT });
  assert.deepEqual(splitter.edgesTo(merger), [
    { from: 0, to: 0 },
    { from: 0, to: 1 },
  ]);
  // Stated the other way round as well: the right SOURCE channel reaches
  // nothing, and both OUTPUTS are fed. That pair of facts is what distinguishes
  // this from silencing an ear.
  const pairs = splitter.edgesTo(merger);
  assert.ok(!pairs.some((p) => p.from === 1), 'the right source channel is not routed');
  assert.deepEqual(pairs.map((p) => p.to).sort(), [0, 1], 'both output channels are fed');
});

test('right only wires source R to BOTH outputs', async () => {
  const { splitter, merger } = await buildWithGraph({ channelMode: CHANNEL_RIGHT });
  const pairs = splitter.edgesTo(merger);
  assert.deepEqual(pairs, [
    { from: 1, to: 0 },
    { from: 1, to: 1 },
  ]);
  assert.ok(!pairs.some((p) => p.from === 0), 'the left source channel is not routed');
});

test('switching channel rewires the splitter and rebuilds nothing else', async () => {
  const g = await buildWithGraph({ channelMode: CHANNEL_STEREO });
  const sourceBefore = g.source;
  const streamBefore = g.audioEl.srcObject;

  g.ra.setChannelMode(CHANNEL_RIGHT);

  assert.deepEqual(g.splitter.edgesTo(g.merger), [
    { from: 1, to: 0 },
    { from: 1, to: 1 },
  ]);
  // The expensive things are untouched. This is what makes the control usable
  // mid-match: no AudioContext is created, the peer connection's track is still
  // the same node, and the element keeps the same stream.
  assert.equal(g.contexts.length, 1, 'no second AudioContext');
  assert.equal(g.ctx.find('source'), sourceBefore, 'the source node is not rebuilt');
  assert.equal(g.audioEl.srcObject, streamBefore, 'the element keeps its stream');
  assert.equal(g.ctx.closed, false, 'the context is not closed');
});

test('switching channel leaves no stale edges from the previous mode', async () => {
  const g = await buildWithGraph({ channelMode: CHANNEL_STEREO });
  g.ra.setChannelMode(CHANNEL_LEFT);
  g.ra.setChannelMode(CHANNEL_RIGHT);
  g.ra.setChannelMode(CHANNEL_STEREO);
  // Three switches, still exactly two connections. An accumulating graph would
  // sum the same source into an output repeatedly and get louder every switch.
  assert.equal(g.splitter.edges.length, 2);
  assert.deepEqual(g.splitter.edgesTo(g.merger), [
    { from: 0, to: 0 },
    { from: 1, to: 1 },
  ]);
});

test('an unroutable channel mode never leaves the splitter unwired', async () => {
  const g = await buildWithGraph();
  g.ra.setChannelMode('centre');
  assert.equal(g.splitter.edges.length, 2, 'nonsense falls back to a routable mode');
});

test('switching bus does not disturb the channel choice', async () => {
  const g = await buildWithGraph({ channelMode: CHANNEL_LEFT });
  // A bus switch is another attach() with a different track.
  await g.ra.attach({ kind: 'audio', id: 'another-bus' });
  assert.deepEqual(g.splitter.edgesTo(g.merger), [
    { from: 0, to: 0 },
    { from: 0, to: 1 },
  ]);
  assert.equal(g.contexts.length, 1, 'a bus switch does not rebuild the context');
});

// --------------------------------------------------------------------------
// The mute: only one return path may be audible
// --------------------------------------------------------------------------

test('muting SEVERS the path to the output rather than turning it down', async () => {
  const g = await buildWithGraph();
  assert.ok(g.gain.connectsTo(g.dest), 'audible to begin with');
  const gainBefore = g.gain.gain.value;

  g.ra.setMuted(true);

  assert.ok(!g.gain.connectsTo(g.dest), 'the gain -> destination edge is gone');
  assert.equal(
    g.gain.gain.value,
    gainBefore,
    'and the gain value is untouched — this is not a volume control',
  );
  assert.ok(g.ra.getState().muted, 'the state says so');
  assert.equal(g.ra.getState().outputConnected, false, 'and agrees about the graph');
});

test('muting pauses the element, so the output device is released', async () => {
  const g = await buildWithGraph();
  const pausesBefore = g.audioEl.pauseCalls;
  g.ra.setMuted(true);
  assert.ok(g.audioEl.pauseCalls > pausesBefore, 'the element is paused');
  assert.equal(g.audioEl.playing, false);
});

test('un-muting reconnects and starts playing again', async () => {
  const g = await buildWithGraph();
  g.ra.setMuted(true);
  const playsBefore = g.audioEl.playCalls;

  g.ra.setMuted(false);
  await Promise.resolve();
  await Promise.resolve();

  assert.ok(g.gain.connectsTo(g.dest), 'the edge is back');
  assert.ok(g.audioEl.playCalls > playsBefore, 'a paused element does not resume by itself');
  assert.equal(g.ra.getState().muted, false);
});

test('un-muting twice does not double the connection', async () => {
  const g = await buildWithGraph();
  g.ra.setMuted(true);
  g.ra.setMuted(false);
  g.ra.setMuted(false);
  assert.equal(
    g.gain.edges.filter((e) => e.target === g.dest).length,
    1,
    'summing the same signal into the destination twice is +6 dB nobody asked for',
  );
});

test('a chain built muted is never audible for even one tick', async () => {
  // The page starts with SRT selected: the WebRTC graph must be built silent,
  // not built audible and muted a moment later.
  const g = await buildWithGraph({ muted: true });
  assert.ok(!g.gain.connectsTo(g.dest), 'never connected');
  assert.equal(g.audioEl.playCalls, 0, 'and never played');
  assert.equal(g.ra.getState().muted, true);
});

test('attaching a track while muted stays silent', async () => {
  const g = await buildWithGraph({ muted: true });
  await g.ra.attach({ kind: 'audio', id: 'a-different-bus' });
  assert.ok(!g.gain.connectsTo(g.dest), 'a bus switch is not a way back to audible');
  assert.equal(g.audioEl.playCalls, 0);
});

test('resume() does not un-mute — that is the one way a silenced path could speak', async () => {
  // resume() is called on any user gesture after an autoplay block. If it
  // started playback on a muted path, clicking anywhere in the window while
  // monitoring on SRT would add a second, offset copy of the programme.
  const g = await buildWithGraph({ muted: true });
  await g.ra.resume();
  assert.equal(g.audioEl.playCalls, 0, 'resume must not start a muted path');
  assert.ok(!g.gain.connectsTo(g.dest));
});

test('the channel choice survives a mute and un-mute', async () => {
  const g = await buildWithGraph({ channelMode: CHANNEL_LEFT });
  g.ra.setMuted(true);
  g.ra.setMuted(false);
  assert.deepEqual(g.splitter.edgesTo(g.merger), [
    { from: 0, to: 0 },
    { from: 0, to: 1 },
  ]);
});

test('getState reports the channel routing in words, for a support log', async () => {
  const g = await buildWithGraph({ channelMode: CHANNEL_RIGHT });
  const state = g.ra.getState();
  assert.equal(state.channelMode, CHANNEL_RIGHT);
  assert.match(state.channelRouting, /source R -> left ear, source R -> right ear/);
});

test('close() disconnects the splitter and merger as well as the gain', async () => {
  const g = await buildWithGraph();
  await g.ra.close();
  assert.equal(g.splitter.edges.length, 0);
  assert.equal(g.merger.edges.length, 0);
  assert.equal(g.gain.edges.length, 0);
  assert.equal(g.ctx.closed, true, 'the AudioContext is closed — Chromium caps them at six');
});
