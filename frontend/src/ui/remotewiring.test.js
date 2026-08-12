/**
 * Tests for the LAN remote-access wiring: the transport SHIM proven against a
 * fake WebSocket, and the source-text guards that keep the transport behind it.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= WHY THE SHIM IS DRIVEN HERE ========================
 *
 * internal/remote/shim.js is the ENTIRE frontend side of the remote bridge, and
 * the claim the whole design rests on is that it needs ZERO changes to
 * backend.js: it installs window.go.main.App and window.runtime by string name,
 * queues calls made before the socket opens, and PRUNES the method list on the
 * hello frame so host-only bindings simply do not exist on a remote client. That
 * claim is proven by driving the shim, not asserted in a comment.
 *
 * The shim is a classic (non-module) script and is DOM-optional by construction
 * — every document access is guarded — so it runs here against nothing but a
 * fake WebSocket, a fake window and a location. node runs each test FILE in its
 * own process, so the globals installed below do not leak into settings.test.js
 * or the other suites, which assume no window at all (the fake backend).
 *
 * ======================= WHY THE REST READS SOURCE =========================
 *
 * The two guards below assert the ABSENCE of a call site, which is this
 * codebase's own idiom for it (mixerwiring.test.js, picturesource.test.js): the
 * transport must stay behind the shim, so backend.js must never grow a `fetch(`
 * or a `WebSocket`, and app.js must gate the destructive beforeunload stops on
 * isRemoteClient() so a closing remote tab cannot kill the commentator's audio.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');
const ui = (name) => readFileSync(join(here, name), 'utf8');

// ---------------------------------------------------------------------------
// A fake WebSocket and a fake window, so the shim can be driven headless.
// ---------------------------------------------------------------------------

class FakeWebSocket {
  constructor(url) {
    this.url = url;
    this.readyState = 0; // CONNECTING; 1 === OPEN, which the shim checks for
    this.sent = [];
    FakeWebSocket.instances.push(this);
  }
  send(data) {
    this.sent.push(data);
  }
  close() {
    this.readyState = 3;
    if (this.onclose) this.onclose({});
  }
  // --- test drivers ---
  open() {
    this.readyState = 1;
    if (this.onopen) this.onopen({});
  }
  deliver(obj) {
    if (this.onmessage) this.onmessage({ data: JSON.stringify(obj) });
  }
}
FakeWebSocket.instances = [];

// Install the globals BEFORE the shim (and before backend.js is dynamically
// imported): the shim reads window.location/window.WebSocket, and backend.js
// reads window.go/window.runtime at module scope.
globalThis.window = {
  location: { protocol: 'https:', host: 'commentary.local:8443' },
  WebSocket: FakeWebSocket,
};
globalThis.WebSocket = FakeWebSocket;

// Run the shim exactly as the browser would: a classic script, evaluated in the
// global scope, whose IIFE installs window.go/window.runtime/__wslcommsRemote
// and opens the (fake) socket.
const shimSrc = readFileSync(join(repoRoot, 'internal', 'remote', 'shim.js'), 'utf8');
(0, eval)(shimSrc);

/** The socket the shim's connect() created on load. */
function currentSocket() {
  return FakeWebSocket.instances[FakeWebSocket.instances.length - 1];
}

// The hello frame the server would send this client. Its shape is
// {t, client, methods, events} — no caps, because the listener is unauthenticated
// and there are no capability tiers (see docs/remote-access.md). `methods` is a
// subset that deliberately OMITS every picture and return method; that omission
// is the whole degradation mechanism, so it is what the availability assertions
// below hang on.
const HELLO_METHODS = ['GetConfig', 'ListInputDevices', 'GetKVSCredentials'];
function sendHello(socket) {
  socket.deliver({ t: 'hello', client: 'client-xyz', methods: HELLO_METHODS, events: [] });
}

// ---------------------------------------------------------------------------
// The transport
// ---------------------------------------------------------------------------

test('a call made before the socket opens resolves after it opens', async () => {
  const socket = currentSocket();
  assert.equal(socket.readyState, 0, 'the shim opens the socket on load');

  // Called through the pre-hello Proxy, before open: it must be queued, not lost.
  const pending = window.go.main.App.GetConfig();
  assert.equal(window.__wslremoteShim._state().queued, 1, 'the early call must be queued');
  assert.equal(socket.sent.length, 0, 'and nothing sent on a socket that is not open');

  // Open, then hello: only on hello does the shim flush the queue, so a call is
  // never sent to a method that is about to be pruned away.
  socket.open();
  assert.equal(socket.sent.length, 0, 'open alone must not flush — the method list is not known yet');
  sendHello(socket);
  assert.equal(socket.sent.length, 1, 'the queued call flushes once hello has installed the method list');

  // Answer it, and the original promise settles with the value.
  const frame = JSON.parse(socket.sent[0]);
  assert.equal(frame.method, 'GetConfig');
  socket.deliver({ t: 'result', id: frame.id, ok: true, value: { ok: 1 } });
  assert.deepEqual(await pending, { ok: 1 }, 'the pre-open call resolves with the server result');
});

test('EventsOn delivers events and returns a working unsubscribe', () => {
  const socket = currentSocket();
  let seen = 0;
  const off = window.runtime.EventsOn('status', () => {
    seen += 1;
  });
  assert.equal(typeof off, 'function', 'EventsOn must return an unsubscribe function');

  socket.deliver({ t: 'event', name: 'status', data: {} });
  assert.equal(seen, 1, 'the handler receives the event');

  off();
  socket.deliver({ t: 'event', name: 'status', data: {} });
  assert.equal(seen, 1, 'after unsubscribe, no further events reach the handler');
});

test('a hello that omits the picture and return methods prunes them everywhere', async () => {
  const socket = currentSocket();
  sendHello(socket); // idempotent: re-install the pruned method list

  // window.go.main.App carries exactly the hello methods, and NONE of the
  // host-only picture/return surface.
  for (const method of HELLO_METHODS) {
    assert.equal(typeof window.go.main.App[method], 'function', `${method} must be installed`);
  }
  for (const absent of [
    'StartPicture',
    'StopPicture',
    'SetPictureRect',
    'SetPictureVisible',
    'GetPictureState',
    'StartReturn',
    'StopReturn',
    'GetReturnState',
    'IsSRTReturnSelected',
    'ListOutputDevices',
  ]) {
    assert.equal(window.go.main.App[absent], undefined, `${absent} must be pruned on a remote client`);
  }

  // And backend.js reads that as "no native picture / no SRT return": its
  // availability checks are ALL-or-nothing over the same method names, so a
  // pruned surface degrades to the mosaic with a message rather than to a button
  // that throws. Imported dynamically, AFTER the real window.go is installed, so
  // backend computes usingFakeBackend === false.
  const backend = await import('./backend.js');
  assert.equal(backend.usingFakeBackend, false, 'with window.go installed this is not the fake backend');
  assert.equal(backend.isRemoteClient(), true, 'the shim published window.__wslcommsRemote');
  assert.equal(backend.pictureAvailable(), false, 'the pruned picture surface is unavailable');
  assert.equal(backend.srtReturnAvailable(), false, 'the pruned return surface is unavailable');
});

// ---------------------------------------------------------------------------
// The source-text guards: the transport stays behind the shim
// ---------------------------------------------------------------------------

test('backend.js contains no transport of its own', () => {
  // The whole point of the shim is that backend.js is untouched by the remote
  // work except for event names and isRemoteClient(). A fetch or a WebSocket
  // here would be a second, un-audited path to the app.
  const js = ui('backend.js');
  assert.equal(/\bfetch\s*\(/.test(js), false, 'backend.js must not fetch — the transport is the shim');
  assert.equal(/\bWebSocket\b/.test(js), false, 'backend.js must not open a WebSocket — the transport is the shim');
});

test('app.js subscribes to the config event', () => {
  const app = ui('app.js');
  assert.match(
    app,
    /backend\.onConfig\(/,
    'app.js must subscribe to the config event, or a second controller clobbers this page indefinitely',
  );
});

test('app.js gates the destructive beforeunload stops on isRemoteClient()', () => {
  // The single highest-consequence detail in the whole plan: a closing REMOTE
  // tab must not call stopReturn()/stopPicture(), which reach the HOST's audio
  // and picture. The shim already neutralises it by omission, but the guard
  // makes it explicit so a change to the shim's method list cannot re-arm it.
  const app = ui('app.js');
  const start = app.indexOf("addEventListener('beforeunload'");
  const firstStop = app.indexOf('backend.stopReturn()', start);
  assert.ok(start > 0 && firstStop > start, 'the beforeunload handler must still call stopReturn');
  const guardRegion = app.slice(start, firstStop);
  assert.match(
    guardRegion,
    /backend\.isRemoteClient\(\)/,
    'the beforeunload stopReturn()/stopPicture() must be gated on isRemoteClient() before they run',
  );
});

// ---------------------------------------------------------------------------
// The unauthenticated posture: no client accounts, no capability tiers, and
// nothing in the APP UI but the bound-port status.
// ---------------------------------------------------------------------------

test('backend.js and settings.js drop the removed client-management bindings', () => {
  // The listener is unauthenticated by the owner's decision — no login, no
  // client accounts, no capability tiers. The three per-client admin methods are
  // GONE from the Go bound surface, so their JS wrappers and every call site must
  // go too, or the Settings screen calls a binding that no longer exists.
  for (const file of ['backend.js', 'settings.js']) {
    const js = ui(file);
    for (const gone of [
      'AddRemoteClient',
      'SetRemoteClientPassword',
      'DeleteRemoteClient',
      'addRemoteClient',
      'setRemoteClientPassword',
      'deleteRemoteClient',
    ]) {
      assert.equal(
        js.includes(gone),
        false,
        `${file} still references ${gone}; there are no client accounts on an unauthenticated listener`,
      );
    }
  }
});

test('the Settings remote-access group is status-only', () => {
  // It shows the bound-port status from GetRemoteState and nothing else: no
  // client list, no capability checkboxes, no add/set-password/delete controls.
  const js = ui('settings.js');
  for (const gone of [
    'remote-cap',
    'REMOTE_CAP',
    'remote-client',
    'remote-clients',
    'remote-add',
    'remote-empty',
    'remoteCapBoxes',
    'remoteClientList',
    'Add client',
    'Set password',
  ]) {
    assert.equal(js.includes(gone), false, `settings.js still carries the client-management artefact "${gone}"`);
  }
  // And it DOES render the bound-port status: enabled, the HTTP/HTTPS ports or
  // URLs, and the certificate fingerprint it derives "running" from.
  assert.match(js, /backend\.getRemoteState\(\)/, 'the group must read GetRemoteState');
  assert.match(js, /certFingerprint/, 'running is derived from a non-empty certFingerprint, not a running field');
  assert.match(js, /httpURL|httpPort/, 'the readout must show the HTTP port/URL');
  assert.match(js, /httpsURL|httpsPort/, 'the readout must show the HTTPS port/URL');
});

test('the app shows no unauthenticated-listener warning or secure-context note', () => {
  // The owner's decision: the risk write-up lives in docs/remote-access.md and
  // the app UI shows ONLY the bound-port status. A prior (reverted) attempt added
  // a secure-context / plain-HTTP note to Settings; guard that it stays out of
  // what the app DISPLAYS. These are phrases such a note would use and that this
  // file's WHY-comments deliberately do not (the comment uses the hyphenated
  // "secure-context", never the spaced "secure context").
  const js = ui('settings.js').toLowerCase();
  for (const phrase of ['secure context', 'in the clear', 'attack surface', 'preferable', '⚠']) {
    assert.equal(
      js.includes(phrase),
      false,
      `settings.js shows "${phrase}"; the risk write-up belongs in docs/remote-access.md, not the app UI`,
    );
  }
});
