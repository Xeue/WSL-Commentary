/**
 * Tests for the peer-connection helpers.
 *
 * Owner: WP-5a. `cd frontend && node --test src/monitor/`
 *
 * Node has no WebRTC, so these use hand-written fakes. That is enough to cover
 * the parts that are logic — the transceiver plan, the mid check, the teardown
 * order — and the parts that are not (whether Chromium actually negotiates)
 * belong to harness.html and to WP-9's Gate C testing.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  createViewerPeerConnection,
  createViewerOffer,
  applyAnswer,
  stopReceivers,
  closePeerConnection,
} from './peer.js';
import { TRANSCEIVER_PLAN } from './buses.js';
import { MonitorErrorCode } from './errors.js';

/** FakePeerConnection records what the monitor did to it. */
class FakePeerConnection {
  constructor(config) {
    this.config = config;
    this.transceivers = [];
    this.closed = false;
    this.localDescription = null;
    this.remoteDescription = null;
    this.receivers = [];
    this.ontrack = () => {};
    this.onicecandidate = () => {};
    this.onconnectionstatechange = () => {};
  }
  addTransceiver(kind, init) {
    const t = { kind, direction: init.direction, mid: null };
    this.transceivers.push(t);
    return t;
  }
  async createOffer() {
    return { type: 'offer', sdp: 'v=0\r\n' };
  }
  async setLocalDescription(d) {
    this.localDescription = d;
  }
  async setRemoteDescription(d) {
    this.remoteDescription = d;
    // A conforming answer assigns mids in offer order.
    this.transceivers.forEach((t, i) => {
      if (t.mid === null) t.mid = String(i);
    });
  }
  getReceivers() {
    return this.receivers;
  }
  close() {
    this.closed = true;
  }
}

/** withFakeRTC installs a global RTCPeerConnection for the duration of fn. */
function withFakeRTC(fn, Ctor = FakePeerConnection) {
  const had = 'RTCPeerConnection' in globalThis;
  const previous = globalThis.RTCPeerConnection;
  globalThis.RTCPeerConnection = Ctor;
  try {
    return fn();
  } finally {
    if (had) globalThis.RTCPeerConnection = previous;
    else delete globalThis.RTCPeerConnection;
  }
}

test('createViewerPeerConnection offers exactly the plan', () => {
  withFakeRTC(() => {
    const iceServers = [{ urls: 'stun:example:443' }];
    const { pc, transceivers } = createViewerPeerConnection(iceServers);

    assert.equal(transceivers.length, 8, 'eight transceivers, per spec §7');
    assert.deepEqual(
      transceivers.map((t) => t.kind),
      [...TRANSCEIVER_PLAN],
      'video first, then seven audio, in that order',
    );
    for (const t of transceivers) {
      assert.equal(t.direction, 'recvonly', 'the monitor publishes nothing');
    }
    assert.equal(pc.config.iceServers, iceServers);
    assert.equal(pc.config.bundlePolicy, 'max-bundle', 'one ICE transport, not eight');
  });
});

test('createViewerPeerConnection reports a browser with no WebRTC', () => {
  const had = 'RTCPeerConnection' in globalThis;
  const previous = globalThis.RTCPeerConnection;
  delete globalThis.RTCPeerConnection;
  try {
    assert.throws(
      () => createViewerPeerConnection([]),
      (err) => {
        assert.equal(err.code, MonitorErrorCode.SIGNALLING_FAILED);
        assert.match(err.message, /RTCPeerConnection/);
        return true;
      },
    );
  } finally {
    if (had) globalThis.RTCPeerConnection = previous;
  }
});

test('createViewerPeerConnection closes the connection if addTransceiver throws', () => {
  class Broken extends FakePeerConnection {
    addTransceiver(kind, init) {
      if (this.transceivers.length === 3) throw new Error('too many m-lines');
      return super.addTransceiver(kind, init);
    }
  }
  let created = null;
  class Recording extends Broken {
    constructor(cfg) {
      super(cfg);
      created = this;
    }
  }
  withFakeRTC(() => {
    assert.throws(() => createViewerPeerConnection([]), /too many m-lines/);
    assert.equal(created.closed, true, 'a half-built connection must not be leaked');
  }, Recording);
});

test('createViewerOffer applies the offer as the local description', async () => {
  await withFakeRTC(async () => {
    const { pc } = createViewerPeerConnection([]);
    const offer = await createViewerOffer(pc);
    assert.equal(offer.type, 'offer');
    assert.equal(pc.localDescription, offer);
  });
});

test('createViewerOffer reports a null local description', async () => {
  class NoLocal extends FakePeerConnection {
    async setLocalDescription() {
      /* silently does nothing, as a broken adapter might */
    }
  }
  await withFakeRTC(async () => {
    const { pc } = createViewerPeerConnection([]);
    await assert.rejects(
      () => createViewerOffer(pc),
      (err) => {
        assert.equal(err.code, MonitorErrorCode.SIGNALLING_FAILED);
        return true;
      },
    );
  }, NoLocal);
});

test('applyAnswer', async (t) => {
  await t.test('a conforming answer produces no mid problems', async () => {
    await withFakeRTC(async () => {
      const { pc, transceivers } = createViewerPeerConnection([]);
      await createViewerOffer(pc);
      const problems = await applyAnswer(pc, transceivers, { type: 'answer', sdp: 'v=0\r\n' });
      assert.deepEqual(problems, []);
      assert.equal(pc.remoteDescription.type, 'answer');
    });
  });

  await t.test('a re-mapped answer is reported but not fatal', async () => {
    class Remapping extends FakePeerConnection {
      async setRemoteDescription(d) {
        this.remoteDescription = d;
        // Swap the two audio mids the return depends on.
        const order = ['0', '2', '1', '3', '4', '5', '6', '7'];
        this.transceivers.forEach((tr, i) => {
          tr.mid = order[i];
        });
      }
    }
    await withFakeRTC(async () => {
      const { pc, transceivers } = createViewerPeerConnection([]);
      await createViewerOffer(pc);
      const problems = await applyAnswer(pc, transceivers, { type: 'answer', sdp: 'v=0\r\n' });
      assert.equal(problems.length, 2);
      assert.ok(problems.join(' ').includes('master'));
      assert.ok(problems.join(' ').includes('aux1'));
    }, Remapping);
  });

  await t.test('an answer with no sdp is rejected', async () => {
    await withFakeRTC(async () => {
      const { pc, transceivers } = createViewerPeerConnection([]);
      for (const bad of [null, undefined, {}, { type: 'answer' }, 'answer']) {
        await assert.rejects(
          () => applyAnswer(pc, transceivers, bad),
          (err) => {
            assert.equal(err.code, MonitorErrorCode.SIGNALLING_FAILED);
            return true;
          },
        );
      }
    });
  });

  await t.test('setRemoteDescription failing is wrapped, not leaked raw', async () => {
    class Failing extends FakePeerConnection {
      async setRemoteDescription() {
        throw new Error('m-line mismatch');
      }
    }
    await withFakeRTC(async () => {
      const { pc, transceivers } = createViewerPeerConnection([]);
      await assert.rejects(
        () => applyAnswer(pc, transceivers, { type: 'answer', sdp: 'v=0' }),
        (err) => {
          assert.equal(err.code, MonitorErrorCode.SIGNALLING_FAILED);
          assert.match(err.message, /setRemoteDescription: m-line mismatch/);
          return true;
        },
      );
    }, Failing);
  });
});

test('stopReceivers stops every track', () => {
  const stopped = [];
  const pc = new FakePeerConnection({});
  pc.receivers = [
    { track: { stop: () => stopped.push('a') } },
    { track: null },
    {},
    {
      track: {
        stop: () => {
          throw new Error('already ended');
        },
      },
    },
    { track: { stop: () => stopped.push('b') } },
  ];
  stopReceivers(pc);
  assert.deepEqual(stopped, ['a', 'b'], 'a throwing track must not stop the sweep');
});

test('stopReceivers tolerates a connection with no getReceivers', () => {
  stopReceivers(null);
  stopReceivers({});
});

test('closePeerConnection detaches handlers before closing', () => {
  const pc = new FakePeerConnection({});
  let stateChanges = 0;
  pc.onconnectionstatechange = () => stateChanges++;
  pc.close = function close() {
    this.closed = true;
    // A real RTCPeerConnection fires connectionstatechange -> 'closed' here.
    if (this.onconnectionstatechange) this.onconnectionstatechange();
  };

  closePeerConnection(pc);

  assert.equal(pc.closed, true);
  assert.equal(stateChanges, 0, 'our own close must not be able to trigger a restart');
  assert.equal(pc.ontrack, null);
  assert.equal(pc.onicecandidate, null);
});

test('closePeerConnection is safe on null and safe twice', () => {
  closePeerConnection(null);
  const pc = new FakePeerConnection({});
  closePeerConnection(pc);
  closePeerConnection(pc);
  assert.equal(pc.closed, true);
});
