/**
 * Tests for the ICE server list.
 *
 * Owner: WP-5a. `cd frontend && node --test src/monitor/`
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { buildIceServers, kvsStunUrl } from './ice.js';

/** A GetIceServerConfig response of the shape KVS returns. */
const TURN = Object.freeze({
  Uris: [
    'turn:1-2-3-4.t-abcdef12.kinesisvideo.eu-west-1.amazonaws.com:443?transport=udp',
    'turns:1-2-3-4.t-abcdef12.kinesisvideo.eu-west-1.amazonaws.com:443?transport=udp',
    'turns:1-2-3-4.t-abcdef12.kinesisvideo.eu-west-1.amazonaws.com:443?transport=tcp',
  ],
  Username: '1730000000:djE6...',
  Password: 'AbCdEf0123456789=',
  Ttl: 300,
});

test('kvsStunUrl is the regional KVS STUN endpoint', () => {
  // The measured region is eu-west-1 (docs/test-results.md §2.4 item 6).
  assert.equal(kvsStunUrl('eu-west-1'), 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443');
  assert.equal(kvsStunUrl('us-east-1'), 'stun:stun.kinesisvideo.us-east-1.amazonaws.com:443');
});

test('buildIceServers', async (t) => {
  const cases = [
    {
      name: 'the STUN server is always first, even with no TURN entries',
      region: 'eu-west-1',
      list: [],
      want: [{ urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' }],
    },
    {
      name: 'a null list behaves like an empty one',
      region: 'eu-west-1',
      list: null,
      want: [{ urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' }],
    },
    {
      name: 'a well-formed TURN entry keeps all its URIs and its credentials',
      region: 'eu-west-1',
      list: [TURN],
      want: [
        { urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' },
        { urls: [...TURN.Uris], username: TURN.Username, credential: TURN.Password },
      ],
    },
    {
      name: 'an entry with no Uris is skipped rather than crashing the constructor',
      region: 'eu-west-1',
      list: [{ Username: 'u', Password: 'p' }],
      want: [{ urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' }],
    },
    {
      name: 'an entry with an empty Uris array is skipped',
      region: 'eu-west-1',
      list: [{ Uris: [], Username: 'u', Password: 'p' }],
      want: [{ urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' }],
    },
    {
      name: 'blank and non-string URIs are dropped from an otherwise good entry',
      region: 'eu-west-1',
      list: [{ Uris: ['turn:a:443', '', '   ', null, 42], Username: 'u', Password: 'p' }],
      want: [
        { urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' },
        { urls: ['turn:a:443'], username: 'u', credential: 'p' },
      ],
    },
    {
      name: 'null entries in the list are skipped',
      region: 'eu-west-1',
      list: [null, undefined, TURN],
      want: [
        { urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' },
        { urls: [...TURN.Uris], username: TURN.Username, credential: TURN.Password },
      ],
    },
    {
      name: 'a TURN entry with a username but no password omits both',
      region: 'eu-west-1',
      list: [{ Uris: ['turn:a:443'], Username: 'u' }],
      want: [
        { urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' },
        { urls: ['turn:a:443'] },
      ],
    },
    {
      name: 'several TURN entries are preserved in order',
      region: 'eu-west-1',
      list: [
        { Uris: ['turn:a:443'], Username: 'u1', Password: 'p1' },
        { Uris: ['turn:b:443'], Username: 'u2', Password: 'p2' },
      ],
      want: [
        { urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' },
        { urls: ['turn:a:443'], username: 'u1', credential: 'p1' },
        { urls: ['turn:b:443'], username: 'u2', credential: 'p2' },
      ],
    },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      assert.deepEqual(buildIceServers(c.region, c.list), c.want);
    });
  }
});

test('buildIceServers uses the injected STUN URL builder when it works', () => {
  const got = buildIceServers('eu-west-1', [], () => 'stun:injected:443');
  assert.deepEqual(got, [{ urls: 'stun:injected:443' }]);
});

test('buildIceServers falls back to the documented STUN URL if the SDK throws', () => {
  const got = buildIceServers('eu-west-1', [], () => {
    throw new Error('the SDK did not load');
  });
  assert.deepEqual(got, [{ urls: 'stun:stun.kinesisvideo.eu-west-1.amazonaws.com:443' }]);
});

test('every produced entry is one an RTCPeerConnection would accept', () => {
  const servers = buildIceServers('eu-west-1', [TURN, { Uris: [] }, null]);
  for (const s of servers) {
    const urls = Array.isArray(s.urls) ? s.urls : [s.urls];
    assert.ok(urls.length > 0, 'an empty urls throws from the RTCPeerConnection constructor');
    for (const u of urls) {
      assert.equal(typeof u, 'string');
      assert.ok(u.length > 0);
      assert.ok(/^stun:|^stuns:|^turn:|^turns:/.test(u), `unexpected scheme: ${u}`);
    }
    if (s.username !== undefined) {
      assert.equal(typeof s.credential, 'string', 'a username without a credential is rejected');
    }
  }
});
