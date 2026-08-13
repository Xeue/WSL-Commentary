/**
 * Tests for the live-operation URL parser.
 *
 * Owner: WP-5b. `cd frontend && node --test "src/ui/*.test.js"`
 *
 * The real URL from the live instance is used throughout, because that is the
 * string this exists to accept:
 *
 *   https://m2lx-wslstudios-matcht.etapsiota.com/live-operation/dl9-5p5ah0bd-empd
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { parseLiveOperationURL, formatLiveOperationURL, bareHost } from './liveurl.js';

const HOST = 'm2lx-wslstudios-matcht.etapsiota.com';
const EVENT = 'dl9-5p5ah0bd-empd';
const FULL = `https://${HOST}/live-operation/${EVENT}`;

test('parses the live instance URL', () => {
  assert.deepEqual(parseLiveOperationURL(FULL), { ok: true, host: HOST, eventId: EVENT });
});

test('accepts it without a scheme', () => {
  assert.deepEqual(parseLiveOperationURL(`${HOST}/live-operation/${EVENT}`), {
    ok: true,
    host: HOST,
    eventId: EVENT,
  });
});

test('accepts http as well as https', () => {
  assert.deepEqual(parseLiveOperationURL(`http://${HOST}/live-operation/${EVENT}`), {
    ok: true,
    host: HOST,
    eventId: EVENT,
  });
});

test('accepts a trailing slash', () => {
  assert.deepEqual(parseLiveOperationURL(`${FULL}/`), { ok: true, host: HOST, eventId: EVENT });
});

test('accepts surrounding whitespace, which a paste often carries', () => {
  assert.deepEqual(parseLiveOperationURL(`  ${FULL}  \n`), { ok: true, host: HOST, eventId: EVENT });
});

test('ignores a query string and a fragment', () => {
  assert.deepEqual(parseLiveOperationURL(`${FULL}?tab=audio#mix`), {
    ok: true,
    host: HOST,
    eventId: EVENT,
  });
});

test('keeps the port, which is part of the host', () => {
  const got = parseLiveOperationURL(`https://${HOST}:8443/live-operation/${EVENT}`);
  assert.equal(got.ok, true);
  assert.equal(got.host, `${HOST}:8443`);
});

test('accepts extra path segments after the event id', () => {
  assert.deepEqual(parseLiveOperationURL(`${FULL}/audio/mixer`), {
    ok: true,
    host: HOST,
    eventId: EVENT,
  });
});

test('accepts a path prefix before live-operation', () => {
  // A reverse proxy in front of M2L-X is not something this app can rule out.
  assert.deepEqual(parseLiveOperationURL(`https://${HOST}/m2lx/live-operation/${EVENT}`), {
    ok: true,
    host: HOST,
    eventId: EVENT,
  });
});

test('matches the segment case-insensitively', () => {
  assert.deepEqual(parseLiveOperationURL(`https://${HOST}/Live-Operation/${EVENT}`), {
    ok: true,
    host: HOST,
    eventId: EVENT,
  });
});

test('percent-decodes the event id', () => {
  const got = parseLiveOperationURL(`https://${HOST}/live-operation/ev%20ent-1`);
  assert.equal(got.ok, true);
  assert.equal(got.eventId, 'ev ent-1');
});

test('strips credentials from the authority', () => {
  const got = parseLiveOperationURL(`https://user:pass@${HOST}/live-operation/${EVENT}`);
  assert.equal(got.ok, true);
  assert.equal(got.host, HOST, 'a password must never end up in the host field');
});

test('rejects a URL with no live-operation segment, and says why', () => {
  const got = parseLiveOperationURL(`https://${HOST}/dashboard`);
  assert.equal(got.ok, false);
  assert.match(got.error, /live-operation/);
});

test('rejects the bare host', () => {
  const got = parseLiveOperationURL(HOST);
  assert.equal(got.ok, false);
  assert.match(got.error, /live-operation/);
});

test('rejects a live-operation URL with no event id after it', () => {
  for (const input of [`https://${HOST}/live-operation`, `https://${HOST}/live-operation/`]) {
    const got = parseLiveOperationURL(input);
    assert.equal(got.ok, false, `${input} was accepted`);
    assert.match(got.error, /event ID|live-operation/);
  }
});

test('rejects a URL with no host', () => {
  const got = parseLiveOperationURL('/live-operation/dl9-5p5ah0bd-empd');
  assert.equal(got.ok, false);
  assert.match(got.error, /host/);
});

test('rejects a non-web scheme by name rather than by shrug', () => {
  const got = parseLiveOperationURL(`ftp://${HOST}/live-operation/${EVENT}`);
  assert.equal(got.ok, false);
  assert.match(got.error, /ftp/);
});

test('rejects empty and non-string input', () => {
  for (const input of ['', '   ', null, undefined, 42, {}]) {
    const got = parseLiveOperationURL(input);
    assert.equal(got.ok, false, `${JSON.stringify(input)} was accepted`);
    assert.equal(typeof got.error, 'string');
    assert.notEqual(got.error, '');
  }
});

test('formatLiveOperationURL round-trips with the parser', () => {
  const url = formatLiveOperationURL(HOST, EVENT);
  assert.equal(url, FULL);
  assert.deepEqual(parseLiveOperationURL(url), { ok: true, host: HOST, eventId: EVENT });
});

test('formatLiveOperationURL tolerates a host that was pasted with a scheme', () => {
  assert.equal(formatLiveOperationURL(`https://${HOST}/`, EVENT), FULL);
});

test('formatLiveOperationURL returns nothing when either half is missing', () => {
  assert.equal(formatLiveOperationURL('', EVENT), '');
  assert.equal(formatLiveOperationURL(HOST, ''), '');
  assert.equal(formatLiveOperationURL(undefined, undefined), '');
});

// bareHost mirrors internal/config's hostOnly, which is what EffectiveSRTHost
// uses to derive the SRT target from the M2L-X host. These cases are the same
// ones as TestEffectiveSRTHost in internal/config/config_test.go: if the two
// ever disagree, the SRT host the UI implies and the one Go dials differ.
test('bareHost matches the Go fallback for every shape of host', () => {
  assert.equal(bareHost('m2lx.example.com'), 'm2lx.example.com');
  assert.equal(bareHost('https://m2lx.example.com'), 'm2lx.example.com');
  assert.equal(bareHost('http://127.0.0.1:8080'), '127.0.0.1');
  assert.equal(bareHost('m2lx.example.com:8443'), 'm2lx.example.com');
  assert.equal(bareHost('https://m2lx.example.com/live-operation/'), 'm2lx.example.com');
  assert.equal(bareHost('https://[2001:db8::1]:8443'), '[2001:db8::1]');
  assert.equal(bareHost('[2001:db8::1]'), '[2001:db8::1]');
  assert.equal(bareHost('  m2lx.example.com  '), 'm2lx.example.com');
  assert.equal(bareHost(''), '');
  assert.equal(bareHost(undefined), '');
});
