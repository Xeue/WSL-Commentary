/**
 * Tests for normalising internal/kvs.Credentials.
 *
 * Owner: WP-5a. `cd frontend && node --test src/monitor/`
 *
 * The measured sample this is built on (docs/test-results.md §2.4 item 6,
 * CONTRACT.md "SP-1 is partly answered already"):
 *
 *   GET /api/live_operation/kvs/webrtc_info/{event}
 *     -> {"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}
 *
 * which Go turns into a kvs.Credentials whose channelArn is empty and whose
 * channelName carries the whole identity. Everything below exists because that
 * is one sample from one instance.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  normaliseCredentials,
  parseExpiry,
  credentialsExpired,
  describeCredentials,
  SKEW_MS,
} from './credentials.js';
import { MonitorErrorCode } from './errors.js';

/** The shape internal/kvs.Credentials marshals to, with the measured values. */
const MEASURED = Object.freeze({
  region: 'eu-west-1',
  channelName: 'webrtc-wslstudios-matcht',
  channelArn: '',
  accessKeyId: 'ASIAEXAMPLEEXAMPLE00',
  secretKey: 'wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY',
  sessionToken: 'IQoJb3JpZ2luX2VjEXAMPLE',
  expiry: '2030-01-01T12:00:00Z',
});

test('the measured credential shape', async (t) => {
  await t.test('normalises with the channel name as the identifier', () => {
    const c = normaliseCredentials(MEASURED);
    assert.equal(c.region, 'eu-west-1');
    assert.equal(c.channelName, 'webrtc-wslstudios-matcht');
    assert.equal(c.channelArn, '', 'M2L-X does not return an ARN; it is resolved in JS');
  });

  await t.test('renames secretKey to secretAccessKey for the AWS SDKs', () => {
    const c = normaliseCredentials(MEASURED);
    assert.equal(c.aws.secretAccessKey, MEASURED.secretKey);
    assert.equal(c.aws.accessKeyId, MEASURED.accessKeyId);
    assert.equal(c.aws.sessionToken, MEASURED.sessionToken);
    assert.equal(c.aws.secretKey, undefined, 'the Go spelling must not leak into the SDK object');
  });

  await t.test('parses the expiry', () => {
    const c = normaliseCredentials(MEASURED);
    assert.ok(c.expiry instanceof Date);
    assert.equal(c.expiry.toISOString(), '2030-01-01T12:00:00.000Z');
  });
});

test('normaliseCredentials rejects unusable input', async (t) => {
  const cases = [
    { name: 'null', in: null, missing: [] },
    { name: 'undefined', in: undefined, missing: [] },
    { name: 'a string', in: 'creds', missing: [] },
    { name: 'a number', in: 42, missing: [] },
    {
      name: 'no region',
      in: { ...MEASURED, region: '' },
      missing: ['region'],
    },
    {
      name: 'neither channel name nor ARN',
      in: { ...MEASURED, channelName: '', channelArn: '' },
      missing: ['channelName'],
    },
    {
      name: 'no access key',
      in: { ...MEASURED, accessKeyId: '' },
      missing: ['accessKeyId'],
    },
    {
      name: 'no secret key',
      in: { ...MEASURED, secretKey: '' },
      missing: ['secretKey'],
    },
    {
      name: 'an empty object reports every missing field at once',
      in: {},
      missing: ['region', 'channelName', 'accessKeyId', 'secretKey'],
    },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      assert.throws(
        () => normaliseCredentials(c.in),
        (err) => {
          assert.equal(err.code, MonitorErrorCode.BAD_CREDENTIALS);
          for (const field of c.missing) {
            assert.ok(
              err.message.includes(field),
              `expected "${field}" in: ${err.message}`,
            );
          }
          return true;
        },
      );
    });
  }
});

test('normaliseCredentials is defensive about spelling, because SP-1 is one sample', async (t) => {
  const cases = [
    {
      name: 'Go-style capitalised field names',
      in: {
        Region: 'eu-west-1',
        ChannelName: 'webrtc-x',
        AccessKeyId: 'A',
        SecretKey: 'S',
        SessionToken: 'T',
      },
      want: { region: 'eu-west-1', channelName: 'webrtc-x' },
    },
    {
      name: 'the AWS spelling secretAccessKey is accepted too',
      in: {
        region: 'eu-west-1',
        channelName: 'webrtc-x',
        accessKeyId: 'A',
        secretAccessKey: 'S',
      },
      want: { region: 'eu-west-1', channelName: 'webrtc-x' },
    },
    {
      name: 'channelARN in the Go spelling is read',
      in: {
        region: 'eu-west-1',
        channelARN: 'arn:aws:kinesisvideo:eu-west-1:1:channel/x/1',
        accessKeyId: 'A',
        secretKey: 'S',
      },
      want: { channelArn: 'arn:aws:kinesisvideo:eu-west-1:1:channel/x/1' },
    },
    {
      name: 'whitespace is trimmed — a trailing newline in a pasted key is a real failure mode',
      in: {
        region: '  eu-west-1  ',
        channelName: ' webrtc-x\n',
        accessKeyId: ' A ',
        secretKey: ' S ',
      },
      want: { region: 'eu-west-1', channelName: 'webrtc-x' },
    },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      const got = normaliseCredentials(c.in);
      for (const [k, v] of Object.entries(c.want)) assert.equal(got[k], v);
    });
  }
});

test('an empty session token is omitted, not sent as an empty string', () => {
  const c = normaliseCredentials({
    region: 'eu-west-1',
    channelName: 'webrtc-x',
    accessKeyId: 'A',
    secretKey: 'S',
    sessionToken: '',
  });
  assert.equal(c.aws.sessionToken, undefined);
  assert.ok(
    !Object.values(c.aws).includes(''),
    'SigV4 signs X-Amz-Security-Token when it is present, and "" is present',
  );
});

test('parseExpiry', async (t) => {
  const cases = [
    { name: 'RFC 3339 from Go', in: { expiry: '2030-01-01T12:00:00Z' }, want: '2030-01-01T12:00:00.000Z' },
    { name: 'RFC 3339 with nanoseconds', in: { expiry: '2030-01-01T12:00:00.123456789Z' }, want: '2030-01-01T12:00:00.123Z' },
    { name: "Go's zero time means unset", in: { expiry: '0001-01-01T00:00:00Z' }, want: null },
    { name: 'absent', in: {}, want: null },
    { name: 'empty', in: { expiry: '' }, want: null },
    { name: 'garbage', in: { expiry: 'soon' }, want: null },
  ];
  for (const c of cases) {
    await t.test(c.name, () => {
      const got = parseExpiry(c.in);
      if (c.want === null) assert.equal(got, null);
      else assert.equal(got.toISOString(), c.want);
    });
  }
});

test('credentialsExpired', async (t) => {
  const at = (iso) => normaliseCredentials({ ...MEASURED, expiry: iso });
  const now = Date.parse('2030-01-01T12:00:00Z');

  const cases = [
    {
      name: 'an hour of life left is fine',
      creds: at('2030-01-01T13:00:00Z'),
      now,
      want: false,
    },
    {
      name: 'a minute of life left is fine — longer than the skew',
      creds: at('2030-01-01T12:01:00Z'),
      now,
      want: false,
    },
    {
      name: 'inside the 30 s skew counts as expired',
      creds: at('2030-01-01T12:00:20Z'),
      now,
      want: true,
    },
    {
      name: 'exactly expired',
      creds: at('2030-01-01T12:00:00Z'),
      now,
      want: true,
    },
    {
      name: 'long expired',
      creds: at('2020-01-01T00:00:00Z'),
      now,
      want: true,
    },
    {
      name: 'no expiry at all is not expired — M2L-X may not state one',
      creds: at('0001-01-01T00:00:00Z'),
      now,
      want: false,
    },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      assert.equal(credentialsExpired(c.creds, c.now), c.want);
    });
  }

  await t.test('the skew is 30 s', () => {
    assert.equal(SKEW_MS, 30_000);
    const creds = at('2030-01-01T12:00:31Z');
    assert.equal(credentialsExpired(creds, now), false);
    assert.equal(credentialsExpired(at('2030-01-01T12:00:29Z'), now), true);
  });
});

test('describeCredentials never leaks the secret or the session token', () => {
  const c = normaliseCredentials(MEASURED);
  const line = describeCredentials(c);
  assert.ok(!line.includes(MEASURED.secretKey), `secret leaked: ${line}`);
  assert.ok(!line.includes(MEASURED.sessionToken), `session token leaked: ${line}`);
  assert.ok(line.includes('eu-west-1'));
  assert.ok(line.includes('webrtc-wslstudios-matcht'));
  assert.ok(line.includes('ASIA'), 'the access key id prefix is useful and not a secret');
  assert.ok(!line.includes(MEASURED.accessKeyId), 'the full access key id is masked');
});
