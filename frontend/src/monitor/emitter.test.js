/**
 * Tests for the typed event emitter and the error types.
 *
 * Owner: WP-5a. `cd frontend && node --test src/monitor/`
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createEmitter } from './emitter.js';
import { MonitorError, MonitorErrorCode, toMonitorError } from './errors.js';

test('createEmitter', async (t) => {
  await t.test('delivers to every listener in registration order', () => {
    const e = createEmitter(['state', 'error']);
    const seen = [];
    e.on('state', (s) => seen.push(`a:${s}`));
    e.on('state', (s) => seen.push(`b:${s}`));
    e.emit('state', 'connected');
    assert.deepEqual(seen, ['a:connected', 'b:connected']);
  });

  await t.test('passes every argument through', () => {
    const e = createEmitter(['state']);
    let got = null;
    e.on('state', (...args) => {
      got = args;
    });
    e.emit('state', 'failed', 42, { x: 1 });
    assert.deepEqual(got, ['failed', 42, { x: 1 }]);
  });

  await t.test('the returned function unsubscribes', () => {
    const e = createEmitter(['state']);
    let n = 0;
    const off = e.on('state', () => n++);
    e.emit('state', 'a');
    off();
    e.emit('state', 'b');
    assert.equal(n, 1);
    assert.equal(e.count('state'), 0);
  });

  await t.test('registering the same function twice registers it once', () => {
    const e = createEmitter(['state']);
    let n = 0;
    const cb = () => n++;
    e.on('state', cb);
    e.on('state', cb);
    e.emit('state', 'a');
    assert.equal(n, 1);
  });

  await t.test('a listener that throws does not stop the others', () => {
    const e = createEmitter(['error']);
    const seen = [];
    const consoleError = console.error;
    console.error = () => {};
    try {
      e.on('error', () => {
        throw new Error('the lamp renderer is broken');
      });
      e.on('error', () => seen.push('second listener still ran'));
      e.emit('error', new Error('x'));
    } finally {
      console.error = consoleError;
    }
    assert.deepEqual(seen, ['second listener still ran']);
  });

  await t.test('a listener may unsubscribe itself from inside the callback', () => {
    const e = createEmitter(['state']);
    let n = 0;
    const off = e.on('state', () => {
      n++;
      off();
    });
    e.emit('state', 'a');
    e.emit('state', 'b');
    assert.equal(n, 1);
  });

  await t.test('an unknown event name throws rather than silently never firing', () => {
    const e = createEmitter(['state', 'error']);
    assert.throws(() => e.on('State', () => {}), /unknown event "State"/);
    assert.throws(() => e.emit('states', 'x'), /unknown event "states"/);
    assert.throws(() => e.count('nope'), /unknown event "nope"/);
  });

  await t.test('a non-function listener throws', () => {
    const e = createEmitter(['state']);
    assert.throws(() => e.on('state', 'not a function'), TypeError);
  });

  await t.test('clear removes everything', () => {
    const e = createEmitter(['state', 'error']);
    e.on('state', () => {});
    e.on('error', () => {});
    assert.equal(e.count('state'), 1);
    e.clear();
    assert.equal(e.count('state'), 0);
    assert.equal(e.count('error'), 0);
  });

  await t.test('emitting with no listeners is a no-op', () => {
    const e = createEmitter(['state']);
    e.emit('state', 'connected');
  });
});

test('MonitorError carries a code and an optional cause', () => {
  const cause = new Error('underlying');
  const err = new MonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'boom', cause);
  assert.ok(err instanceof Error);
  assert.equal(err.name, 'MonitorError');
  assert.equal(err.code, 'SIGNALLING_FAILED');
  assert.equal(err.message, 'boom');
  assert.equal(err.cause, cause);
});

test('toMonitorError', async (t) => {
  const cases = [
    {
      name: 'an existing MonitorError passes through untouched',
      in: new MonitorError(MonitorErrorCode.AUTOPLAY_BLOCKED, 'click first'),
      wantCode: MonitorErrorCode.AUTOPLAY_BLOCKED,
      wantMessage: 'click first',
    },
    {
      name: 'a plain Error is wrapped with the prefix',
      in: new Error('network down'),
      wantCode: MonitorErrorCode.SIGNALLING_FAILED,
      wantMessage: 'GetIceServerConfig: network down',
    },
    {
      name: 'a DOMException-like object is wrapped',
      in: { name: 'NotAllowedError', message: 'permission denied' },
      wantCode: MonitorErrorCode.SIGNALLING_FAILED,
      wantMessage: 'GetIceServerConfig: permission denied',
    },
    {
      name: 'a thrown string is wrapped',
      in: 'something went wrong',
      wantCode: MonitorErrorCode.SIGNALLING_FAILED,
      wantMessage: 'GetIceServerConfig: something went wrong',
    },
    {
      name: 'a rejection with no reason still produces a readable message',
      in: undefined,
      wantCode: MonitorErrorCode.SIGNALLING_FAILED,
      wantMessage: 'GetIceServerConfig: unknown error',
    },
    {
      name: 'null is handled',
      in: null,
      wantCode: MonitorErrorCode.SIGNALLING_FAILED,
      wantMessage: 'GetIceServerConfig: null',
    },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      const err = toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'GetIceServerConfig', c.in);
      assert.ok(err instanceof MonitorError);
      assert.equal(err.code, c.wantCode);
      assert.equal(err.message, c.wantMessage);
    });
  }
});

test('every MonitorErrorCode value equals its key', () => {
  // The codes reach WP-5b as strings; a key/value mismatch would make
  // `err.code === MonitorErrorCode.X` quietly wrong.
  for (const [k, v] of Object.entries(MonitorErrorCode)) {
    assert.equal(k, v);
  }
});
