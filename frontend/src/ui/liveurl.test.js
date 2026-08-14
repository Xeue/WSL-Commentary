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

import {
  parseLiveOperationURL,
  parseM2LXAddress,
  formatM2LXAddress,
  bareHost,
} from './liveurl.js';

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

// ---------------------------------------------------------------------------
// parseM2LXAddress: the field the operator actually types into
// ---------------------------------------------------------------------------
//
// THE DEFECT THESE PIN. The address field ran everything through
// parseLiveOperationURL, which REQUIRES a /live-operation/<id> segment, so
// pasting the instance's own address — the obvious thing to type, and the thing
// the app itself can turn into an event id through /api/events/overview — was
// refused with an error and the host was never filled in. The operator was
// still forced to go and find a full live-operation URL for an id the app can
// now ask for.
//
// Both forms must therefore work, and the full-URL form must keep working
// EXACTLY as it did: it is the only source of an event id before sign-in
// succeeds, and it is what is written down in operators' notes.

test('accepts the bare instance address, leaving the event to the API', () => {
  assert.deepEqual(parseM2LXAddress(`https://${HOST}`), { ok: true, host: HOST, eventId: '' });
});

test('accepts the bare address in every shape a paste arrives in', () => {
  for (const input of [
    HOST, // no scheme at all
    `http://${HOST}`, // http, as the strict parser also allows
    `https://${HOST}/`, // trailing slash
    `HTTPS://${HOST}`, // the scheme is matched case-insensitively
    `  https://${HOST}  \n`, // a paste's whitespace
    `https://${HOST}?tab=audio#mix`, // query and fragment, both meaningless here
    `https://user:pass@${HOST}/`, // credentials, which are not part of the host
  ]) {
    assert.deepEqual(
      parseM2LXAddress(input),
      { ok: true, host: HOST, eventId: '' },
      `${JSON.stringify(input)} must be accepted as the instance address`,
    );
  }
});

test('keeps a port on the bare address, because the port is part of the host', () => {
  // internal/config.EffectiveSRTHost reduces this to the bare host for SRT, but
  // the API client dials exactly what is stored — dropping :8443 here would send
  // the sign-in to port 443 of an instance that is not there.
  assert.deepEqual(parseM2LXAddress(`https://${HOST}:8443/`), {
    ok: true,
    host: `${HOST}:8443`,
    eventId: '',
  });
  assert.deepEqual(parseM2LXAddress('[2001:db8::1]:8443'), {
    ok: true,
    host: '[2001:db8::1]:8443',
    eventId: '',
  });
});

test('accepts an IP address and localhost, which are hosts an engineer does use', () => {
  for (const host of ['192.168.1.50', '192.168.1.50:8443', 'localhost', 'localhost:8443', '[2001:db8::1]']) {
    const got = parseM2LXAddress(`https://${host}`);
    assert.equal(got.ok, true, `${host} must be accepted`);
    assert.equal(got.host, host);
  }
});

test('the full live-operation URL still fills BOTH fields, exactly as before', () => {
  // Muscle memory, and the URLs already written down in notes. This is also the
  // fallback the whole feature degrades to when the instance cannot be listed,
  // so it must not become a second-class path.
  for (const input of [FULL, `${FULL}/`, `${HOST}/live-operation/${EVENT}`, `${FULL}/audio/mixer`]) {
    assert.deepEqual(
      parseM2LXAddress(input),
      { ok: true, host: HOST, eventId: EVENT },
      `${input} must still yield both halves`,
    );
  }
  // And it agrees with the strict parser wherever the strict parser accepts.
  assert.deepEqual(parseM2LXAddress(FULL), parseLiveOperationURL(FULL));
});

test('a live-operation URL truncated before the id is taken as the instance address', () => {
  // The host is still unambiguous and the id it lost is precisely the one the
  // events API supplies. Refusing it would be the old dead end by another route.
  for (const input of [`https://${HOST}/live-operation`, `https://${HOST}/live-operation/`]) {
    assert.deepEqual(parseM2LXAddress(input), { ok: true, host: HOST, eventId: '' }, input);
  }
});

test('rejects a scheme this app cannot speak, by name', () => {
  for (const input of [`ftp://${HOST}`, `file://${HOST}/live-operation/${EVENT}`]) {
    const got = parseM2LXAddress(input);
    assert.equal(got.ok, false, `${input} was accepted`);
    assert.match(got.error, /ftp|file/);
  }
});

test('rejects empty and non-string input with a reason', () => {
  for (const input of ['', '   ', null, undefined, 42, {}]) {
    const got = parseM2LXAddress(input);
    assert.equal(got.ok, false, `${JSON.stringify(input)} was accepted`);
    assert.equal(typeof got.error, 'string');
    assert.notEqual(got.error, '');
  }
});

test('rejects an address with no host in it', () => {
  const got = parseM2LXAddress('https:///live-operation/x');
  assert.equal(got.ok, false);
  assert.match(got.error, /host/);
});

test('does not let a bare word through as a hostname', () => {
  // THE POINT OF THE HOST CHECK. Nothing downstream examines this string again:
  // it is saved as m2lxHost, dialled by the API client and reduced to the SRT
  // target. A typo accepted here comes back as a DNS failure inside a reconnect
  // ladder that does not name the field that caused it.
  for (const input of ['m2lx', 'wslstudios', 'the m2lx box', 'not a url at all']) {
    const got = parseM2LXAddress(input);
    assert.equal(got.ok, false, `${JSON.stringify(input)} was accepted as a host`);
    assert.equal(typeof got.error, 'string');
    assert.notEqual(got.error, '');
  }
  // And it says WHICH word, so the operator can see what was read.
  assert.match(parseM2LXAddress('m2lx').error, /m2lx/);
});

test('rejects malformed hosts and ports rather than storing them', () => {
  const cases = [
    `${HOST}:`, // a colon with no port
    `${HOST}:https`, // the scheme typed as the port
    `${HOST}:0`, // out of range, both ends
    `${HOST}:70000`,
    `${HOST}..com`, // an empty label
    `-${HOST}`, // a label may not start with a hyphen
    `${HOST}-`,
    '2001:db8::1', // IPv6 must be bracketed, or its last group looks like a port
    '[2001:db8::1', // no closing bracket
    '[not:an:address!]',
  ];
  for (const input of cases) {
    const got = parseM2LXAddress(input);
    assert.equal(got.ok, false, `${JSON.stringify(input)} was accepted as a host`);
  }
});

test('a full live-operation URL is NOT held to the host check', () => {
  // parseLiveOperationURL never checked the host, it ships on air on Windows,
  // and the /live-operation/ segment is itself the evidence that a real address
  // was pasted. A proxy on a single-label internal name must keep working.
  assert.deepEqual(parseM2LXAddress(`https://m2lx/live-operation/${EVENT}`), {
    ok: true,
    host: 'm2lx',
    eventId: EVENT,
  });
});

test('formatM2LXAddress writes the base address and nothing else', () => {
  // THE COMPLAINT THIS CLOSES: "it seems to do some stuff where it is writing
  // URLs with extra parts, not just pasting the whole URL in". This function
  // asked formatLiveOperationURL first, so a config holding both halves drew
  // https://host/live-operation/<id> into the box on every load — a longer
  // string than the one the operator pasted, naming a page rather than an
  // instance.
  assert.equal(formatM2LXAddress(HOST), `https://${HOST}`);
  assert.ok(!formatM2LXAddress(HOST).includes('live-operation'));

  // A host pasted with a scheme or a trailing slash is still reduced to the one
  // canonical spelling, so re-populating the form does not churn the box.
  assert.equal(formatM2LXAddress(`https://${HOST}/`), `https://${HOST}`);
  assert.equal(formatM2LXAddress(`  ${HOST}  `), `https://${HOST}`);
  assert.equal(formatM2LXAddress(''), '', 'with no host there is nothing to show');
  assert.equal(formatM2LXAddress(undefined), '');
});

test('formatM2LXAddress takes the host ALONE — the event id parameter is gone', () => {
  // Not merely ignored: gone. An ignored second parameter is an invitation to
  // pass the event id again and expect it to show up, which is how the
  // live-operation form got into the write path in the first place. The event
  // id comes from the events API and lives on its own row; a preset never
  // carries it.
  assert.equal(formatM2LXAddress.length, 1, 'formatM2LXAddress has grown a second parameter again');
});

test('the round trip that must hold is on the HOST, not on the whole string', () => {
  // The honest statement of it. format(parse(x)) === x was true while the
  // formatter re-synthesised the long form, and it is NOT true now: a pasted
  // live-operation URL parses into a host AND an event id, and only the host
  // goes back into the box. What still has to hold — and what a Settings screen
  // reporting an error against a value nobody typed would break — is that
  // whatever this writes parses back to the same host, with no event id
  // invented out of the address.
  const pasted = parseM2LXAddress(FULL);
  assert.deepEqual(pasted, { ok: true, host: HOST, eventId: EVENT });

  const written = formatM2LXAddress(pasted.host);
  assert.equal(written, `https://${HOST}`);
  assert.deepEqual(parseM2LXAddress(written), { ok: true, host: HOST, eventId: '' });

  // The event id is not lost by this — it is held in the form's own eventId
  // field and shown by the event picker, which is where it belongs. Losing it
  // would matter: before the first successful sign-in a pasted URL is the only
  // source of an id there is.
  assert.equal(pasted.eventId, EVENT);
});

test('the PARSER still accepts the full live-operation URL the app no longer writes', () => {
  // Reading and writing are deliberately asymmetric. Operators have these URLs
  // in their notes and in muscle memory, so a pasted one must go on filling
  // both fields — this half is untouched by the formatter's change, and this
  // test is what says so.
  assert.deepEqual(parseLiveOperationURL(FULL), { ok: true, host: HOST, eventId: EVENT });
  assert.deepEqual(parseM2LXAddress(FULL), { ok: true, host: HOST, eventId: EVENT });
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
