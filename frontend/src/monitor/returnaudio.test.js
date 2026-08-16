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
 * The three properties under test are the three that fail silently:
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
 *
 *   3. A setSinkId that WKWebView refuses for want of a user gesture must be
 *      queued and retried, not dropped. Dropped, it is invisible: the operator
 *      picks their headphones, every lamp stays green, and the return plays out
 *      of the laptop speakers sitting in front of a live microphone. See note 4
 *      at the top of audio.js.
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
    /**
     * How many more setSinkId calls must reject with NotAllowedError before one
     * is allowed through. This is WKWebView modelled: it refuses to change the
     * audio output device unless the page has transient activation, and the
     * refusal is a rejection, not a silent no-op. WebView2 has no such rule,
     * which is exactly what leaving this at 0 represents.
     */
    this.sinkRefusals = 0;
    this.sinkIdCalls = 0;
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
    this.sinkIdCalls += 1;
    if (this.sinkRefusals > 0) {
      this.sinkRefusals -= 1;
      // The shape WebKit actually throws. audio.js branches on `name` and
      // nothing else, so `name` is the part that has to be right.
      const err = new Error('The request is not allowed by the user agent');
      err.name = 'NotAllowedError';
      return Promise.reject(err);
    }
    this.sinkId = id;
    return Promise.resolve();
  }
}

/**
 * FakeDocument records the one-shot gesture listeners audio.js arms, so a test
 * can both see that one is armed and fire it. The real thing is
 * document.addEventListener with {once, capture, passive}; only the type and
 * the function matter here.
 */
class FakeDocument {
  constructor() {
    this.created = [];
    /** @type {Map<string, Set<Function>>} */
    this.listeners = new Map();
  }
  createElement() {
    const el = new FakeMediaElement();
    this.created.push(el);
    return el;
  }
  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type).add(fn);
  }
  removeEventListener(type, fn) {
    this.listeners.get(type)?.delete(fn);
  }
  /** armed is true while a gesture retry is waiting for an interaction. */
  get armed() {
    return [...this.listeners.values()].some((s) => s.size > 0);
  }
  /**
   * gesture dispatches one pointerdown and then waits a macrotask, which drains
   * the whole microtask queue — every promise in these fakes is already
   * resolved, so the retry audio.js kicks off has completely finished by the
   * time this returns.
   */
  async gesture() {
    for (const fn of [...(this.listeners.get('pointerdown') ?? [])]) fn({ type: 'pointerdown' });
    await new Promise((r) => setTimeout(r, 0));
  }
}

/** installEnvironment puts the globals audio.js reaches for in place. */
function installEnvironment() {
  globalThis.AudioContext = FakeAudioContext;
  globalThis.MediaStream = class MediaStream {
    constructor(tracks) {
      this.tracks = tracks || [];
    }
  };
  const doc = new FakeDocument();
  globalThis.document = doc;
  return doc;
}

const audioTrack = () => ({ kind: 'audio', id: 'return-track' });

/**
 * buildWithGraph makes a return-audio chain, attaches a track, and hands back
 * the fake nodes it built. Every test starts from a routed, playing path,
 * because that is the only state in which either feature means anything.
 *
 * The chain does not expose its nodes, so the AudioContext is captured by
 * subclassing the constructor for the duration of the build.
 *
 * `sinkRefusals` is not a createReturnAudio option — it is pulled out here and
 * put on the fake element, because it has to be in place before attach() makes
 * the first setSinkId call.
 */
async function buildWithGraph({ sinkRefusals = 0, ...opts } = {}) {
  const doc = installEnvironment();
  /** @type {FakeAudioContext[]} */
  const contexts = [];
  globalThis.AudioContext = class extends FakeAudioContext {
    constructor(o) {
      super(o);
      contexts.push(this);
    }
  };
  const audioEl = new FakeMediaElement();
  audioEl.sinkRefusals = sinkRefusals;
  const errors = [];
  const ra = createReturnAudio({ audioEl, gainDb: 18, onError: (e) => errors.push(e), ...opts });
  await ra.attach(audioTrack());
  const ctx = contexts[0];
  return {
    ra,
    audioEl,
    errors,
    contexts,
    doc,
    createdElements: doc.created,
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

// --------------------------------------------------------------------------
// setSinkId and the user gesture (WKWebView)
//
// The refused-then-retried path is macOS-only in practice, but nothing in
// audio.js tests the platform: it branches on the NotAllowedError WebKit
// throws. That is what the last test here pins — with no refusal scripted, the
// element is set once, nothing is armed, and nothing is reported, which is the
// WebView2 behaviour this port is not allowed to change.
// --------------------------------------------------------------------------

test('a setSinkId refused for want of a gesture is queued, not lost', async () => {
  // The config-apply paths in ui/app.js have no gesture behind them, so on
  // WKWebView the operator's chosen headphones are refused the moment the
  // configuration loads. Losing it there means the return plays on the system
  // default — on a commentary laptop, the built-in speakers, next to a live
  // microphone.
  const g = await buildWithGraph({ sinkId: 'headphones-1', sinkRefusals: 1 });

  assert.equal(g.audioEl.sinkId, null, 'the element was refused');
  assert.equal(g.ra.getState().appliedSinkId, null);
  assert.equal(g.ra.getState().sinkAwaitingGesture, true, 'and the retry is armed');
  assert.ok(g.doc.armed, 'a gesture listener is waiting');

  // AND NOTHING IS SAID. This assertion is inverted from what it used to be, at
  // the operator's request: he screenshotted the sentence this branch used to
  // report — "the browser will not change the audio output device until someone
  // interacts with the window" — as a thing that alarmed him.
  //
  // Every word of it was true. It was also an explanation of a state that is
  // about to end: the listener armed above fires on the next interaction
  // ANYWHERE in the document and re-applies, so the operator was reading an
  // alarm about something that fixed itself while they read it. An alert that
  // fires when everything is fine trains people to ignore the surface it appears
  // on, and that cost is paid by the next message on that surface, which is
  // real.
  //
  // What is NOT silenced is the retry failing — that is the test below, and it
  // is the reason silence here is safe rather than merely quieter.
  assert.deepEqual(
    g.errors.map((e) => e.code),
    [],
    'a deferral that is about to fix itself must say nothing at all',
  );

  await g.doc.gesture();

  assert.equal(g.audioEl.sinkId, 'headphones-1', 'the next click applies it');
  assert.equal(g.ra.getState().appliedSinkId, 'headphones-1');
  assert.equal(g.ra.getState().sinkAwaitingGesture, false);
  assert.ok(!g.doc.armed, 'and the one-shot listener is gone');
});

test('the queued sink is retried BEFORE playback, not after', async () => {
  // Transient activation is a budget, not a flag: ctx.resume() and play() are
  // awaited round trips, and spending the activation window on them first is
  // how the headphone choice ends up needing a second click.
  const g = await buildWithGraph({ sinkId: 'headphones-1', sinkRefusals: 1 });
  const playsBefore = g.audioEl.playCalls;
  g.audioEl.play = function play() {
    this.playCalls += 1;
    this.playing = true;
    assert.equal(this.sinkId, 'headphones-1', 'the sink was applied before this play()');
    return Promise.resolve();
  };
  await g.doc.gesture();
  assert.ok(g.audioEl.playCalls > playsBefore, 'playback was still retried');
});

test('a device refused a second time from inside a gesture is reported as a failure', async () => {
  // One retry, then the truth. A device that is never going to be permitted
  // must not promise "on the next click" on every click for the rest of the
  // match — that is the same silent-failure shape this whole change removes,
  // wearing a friendlier message.
  const g = await buildWithGraph({ sinkId: 'headphones-1', sinkRefusals: 2 });
  assert.equal(g.errors.length, 0, 'the first refusal is silent — it is about to be retried');

  await g.doc.gesture();

  assert.equal(g.audioEl.sinkId, null, 'still refused');
  assert.equal(
    g.errors.filter((e) => e.code === 'SINK_ID_DEFERRED').length,
    0,
    'and still no promise of a click: one silent retry, then the truth',
  );
  const failures = g.errors.filter((e) => e.code === 'SINK_ID_FAILED');
  assert.equal(failures.length, 1, 'the refusal is reported for what it is');
  assert.match(
    failures[0].message,
    /system default output/i,
    'and it describes the CONSEQUENCE the commentator will experience, not the call that failed',
  );
  assert.equal(g.ra.getState().sinkAwaitingGesture, false);
});

test('choosing a different device gets a fresh retry', async () => {
  // The one-shot budget stops one un-grantable device nagging forever. It must
  // not stop the NEXT choice the commentator makes from being queued.
  const g = await buildWithGraph({ sinkId: 'headphones-1', sinkRefusals: 2 });
  await g.doc.gesture(); // spends the budget on headphones-1 and reports failure

  g.audioEl.sinkRefusals = 1;
  await g.ra.setSinkId('headphones-2');
  assert.equal(g.ra.getState().sinkAwaitingGesture, true, 'the new device is queued');

  await g.doc.gesture();
  assert.equal(g.audioEl.sinkId, 'headphones-2');
});

test('with no gesture requirement nothing is armed and nothing is said', async () => {
  // WebView2. This application is on air on Windows and this test is the reason
  // the retry is keyed on the error rather than on the platform: if the browser
  // never refuses, the whole mechanism is inert.
  const g = await buildWithGraph({ sinkId: 'headphones-1' });
  assert.equal(g.audioEl.sinkId, 'headphones-1');
  assert.equal(g.audioEl.sinkIdCalls, 1, 'applied once, on attach');
  assert.equal(g.ra.getState().sinkAwaitingGesture, false);
  assert.ok(!g.doc.armed, 'no gesture listener exists to fire');
  assert.deepEqual(g.errors, [], 'and no error was reported');
});
