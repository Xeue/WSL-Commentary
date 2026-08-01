/**
 * A standalone harness for looking at the mixer drawer without running the app.
 *
 * Owner: WP-M4. DEV ONLY. Nothing that ships imports this file, and demo.html
 * is not an entry point in vite.config — `npm run build` never emits either.
 *
 * See demo.html for how to open it.
 *
 * It stands in for the Go side with an in-memory mixer built from the captured
 * live frame: 54 strips, 7 buses, cam22-1 ("CLAUDE-COMMS") routed to the
 * default ["master","aux1","aux2"] and therefore already in the clean feed.
 * sendCommands applies to that in-memory state after a short delay, so the
 * drawer's "sent, awaiting confirmation" path behaves as it does against the
 * real mixer — and the "deaf mixer" switch makes it not apply at all, which is
 * how the NOT CONFIRMED path is seen.
 */

import { createMixerDrawer } from './drawer.js';
import { LIVE_FIXTURE } from './demo-fixture.js';

const APPLY_DELAY_MS = 900;

/** deep copy without structuredClone, which the WebView2 build may predate. */
const clone = (v) => JSON.parse(JSON.stringify(v));

/** the pretend mixer */
const mixer = {
  state: clone(LIVE_FIXTURE),
  deaf: false,
  golden: null,
};

const log = document.querySelector('#log');
function say(line) {
  const p = document.createElement('div');
  p.textContent = `${new Date().toLocaleTimeString()}  ${line}`;
  log.insertBefore(p, log.firstChild);
}

/** jitter moves the meters so the 1 Hz update path is visible. */
function jitter(snap) {
  const s = clone(snap);
  s.takenAt = new Date().toISOString();
  for (const strip of s.strips) {
    if (!strip.metered) continue;
    const silent = strip.peakHold[0] <= -99;
    if (silent) continue;
    const drift = () => (Math.random() - 0.5) * 6;
    strip.peakHold = [clamp(strip.peakHold[0] + drift()), clamp(strip.peakHold[1] + drift())];
    strip.level = strip.peakHold.slice();
  }
  for (const bus of s.buses) {
    if (!bus.metered) continue;
    const live = s.strips.filter((t) => !t.muted && t.outputs.includes(bus.name) && t.metered);
    const loudest = live.reduce((acc, t) => Math.max(acc, t.peakHold[0]), -100);
    bus.peakHold = [loudest, loudest];
    bus.level = bus.peakHold.slice();
  }
  return s;
}

const clamp = (db) => Math.max(-100, Math.min(-0.5, db));

const drawer = createMixerDrawer({
  mount: document.querySelector('#drawer-mount'),

  getSnapshot: async () => {
    say('getSnapshot()');
    return jitter(mixer.state);
  },

  sendCommands: async (cmds) => {
    say(`sendCommands(${cmds.length}): ${JSON.stringify(cmds)}`);
    if (mixer.deaf) {
      say('  DEAF MIXER: accepted and discarded, exactly as a silently inert command behaves');
      return;
    }
    setTimeout(() => {
      for (const cmd of cmds) {
        if (cmd.kind !== 'setRouting') continue;
        const strip = mixer.state.strips.find((s) => s.name === cmd.args.input);
        if (strip) strip.outputs = cmd.args.outputs.slice();
      }
      say('  the mixer applied it; the next snapshot will show it');
    }, APPLY_DELAY_MS);
  },

  getGolden: async () => mixer.golden,

  setGolden: async (snap) => {
    mixer.golden = clone(snap);
    say('setGolden() - application state only, the mixer was not touched');
  },

  onError: (err, context) => say(`onError: ${context}: ${err.message}`),

  onArmedChange: (armed) => say(`onArmedChange(${armed})`),
});

/* --------------------------------------------------------------- controls */

document.querySelector('#open').addEventListener('click', () => drawer.open());

// THESE TWO CONTROLS CURRENTLY SHOW NOTHING, AND THAT IS EXPECTED.
//
// The drift panel has been withdrawn from the drawer's interface at the
// operator's request, so this build never calls getGolden and never renders a
// diff. Everything behind it is intact — internal/mixer/golden.go, Compare and
// their tests, and compareSnapshots / sortDiffs / diffHeadline in ./model.js
// and theirs — so these buttons still stock the fake correctly and will start
// showing something again the moment the panel is restored in drawer.js. They
// are kept for exactly that reason.

document.querySelector('#golden-clean').addEventListener('click', () => {
  // A golden in which the commentary strip is NOT in the clean feed. The live
  // state is, so the drawer would raise one CRITICAL if the panel were shown.
  const g = clone(mixer.state);
  const comms = g.strips.find((s) => s.name === 'cam22-1');
  if (comms) comms.outputs = ['master'];
  mixer.golden = g;
  say('loaded a golden in which CLAUDE-COMMS is not in the clean feed');
});

document.querySelector('#golden-clear').addEventListener('click', () => {
  mixer.golden = null;
  say('cleared the golden state');
});

document.querySelector('#reset-defaults').addEventListener('click', () => {
  // What a device reset, a re-added input or a restored profile does: every
  // strip back to the default routing, i.e. every strip into the clean feed.
  for (const s of mixer.state.strips) s.outputs = ['master', 'aux1', 'aux2'];
  say('every strip reset to the default ["master","aux1","aux2"] - all of them are now in the clean feed');
});

document.querySelector('#unmute-comms').addEventListener('click', () => {
  const comms = mixer.state.strips.find((s) => s.name === 'cam22-1');
  if (comms) comms.muted = !comms.muted;
  say(`CLAUDE-COMMS is now ${comms && comms.muted ? 'muted' : 'UNMUTED - and it is in the clean feed'}`);
});

const deafBtn = document.querySelector('#deaf');
deafBtn.addEventListener('click', () => {
  mixer.deaf = !mixer.deaf;
  deafBtn.setAttribute('aria-pressed', String(mixer.deaf));
  deafBtn.textContent = mixer.deaf ? 'Deaf mixer: ON (writes are discarded)' : 'Deaf mixer: off';
  say(`deaf mixer ${mixer.deaf ? 'ON' : 'off'}`);
});

const feedBtn = document.querySelector('#feed');
let feeding = true;
let timer = setInterval(tick, 1000);
function tick() {
  drawer.update(jitter(mixer.state));
}
feedBtn.addEventListener('click', () => {
  feeding = !feeding;
  if (feeding) {
    timer = setInterval(tick, 1000);
  } else {
    clearInterval(timer);
  }
  feedBtn.setAttribute('aria-pressed', String(!feeding));
  feedBtn.textContent = feeding ? 'Stop the 1 Hz feed' : 'Start the 1 Hz feed';
  say(feeding ? 'update() resumed' : 'update() stopped - the drawer should say STALE within a few seconds');
});

say('ready. Press "Open the drawer". Nothing has been written.');
