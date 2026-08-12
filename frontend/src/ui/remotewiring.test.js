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

// The hello frame the server would send this client: view-tier methods only,
// deliberately OMITTING every picture and return method. That omission is the
// whole degradation mechanism, so it is what the availability assertions below
// hang on.
const HELLO_METHODS = ['GetConfig', 'ListInputDevices', 'GetKVSCredentials'];
function sendHello(socket) {
  socket.deliver({ t: 'hello', client: 'client-xyz', caps: ['view'], methods: HELLO_METHODS });
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
