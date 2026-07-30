/**
 * A twelve-line typed event emitter.
 *
 * Owner: WP-5a.
 *
 * Why this exists rather than Node's `events`: the KVS WebRTC SDK's CommonJS
 * entry point (`lib/SignalingClient.js`) does `require('events')`, and `events`
 * is not an npm package in this tree — it is a Node builtin. Vite's browser
 * build externalises it to an empty module, so `class SignalingClient extends
 * EventEmitter` evaluates to `class extends undefined` and the *whole bundle*
 * throws on load. See kvs-sdk.js for how the monitor works around that for the
 * SDK. For our own eventing we simply do not need an EventEmitter, so we do not
 * import one.
 *
 * The emitter is deliberately strict about event names: `on('State', cb)` is a
 * typo that would otherwise silently never fire, and a lamp that never changes
 * looks exactly like a lamp that is working.
 */

/**
 * createEmitter builds an emitter restricted to a fixed set of event names.
 *
 * @param {string[]} names the only event names `on` and `emit` will accept
 * @returns {{
 *   on: (event: string, cb: Function) => () => void,
 *   emit: (event: string, ...args: unknown[]) => void,
 *   clear: () => void,
 *   count: (event: string) => number,
 * }}
 */
export function createEmitter(names) {
  const allowed = new Set(names);
  /** @type {Map<string, Set<Function>>} */
  const listeners = new Map();
  for (const n of allowed) listeners.set(n, new Set());

  function assertKnown(event, what) {
    if (!allowed.has(event)) {
      throw new Error(
        `${what}: unknown event "${event}"; known events are ${[...allowed].join(', ')}`,
      );
    }
  }

  return {
    /**
     * on registers a listener and returns a function that removes it.
     * Registering the same function twice registers it once.
     */
    on(event, cb) {
      assertKnown(event, 'on');
      if (typeof cb !== 'function') {
        throw new TypeError(`on("${event}"): listener must be a function`);
      }
      const set = listeners.get(event);
      set.add(cb);
      return () => set.delete(cb);
    },

    /**
     * emit calls every listener for the event. A listener that throws is
     * reported to the console and does not stop the remaining listeners: one
     * broken lamp renderer must not take down the media pipeline's error
     * reporting.
     */
    emit(event, ...args) {
      assertKnown(event, 'emit');
      // Copy first: a listener is allowed to unsubscribe itself.
      for (const cb of [...listeners.get(event)]) {
        try {
          cb(...args);
        } catch (err) {
          console.error(`[monitor] listener for "${event}" threw:`, err);
        }
      }
    },

    /** clear removes every listener for every event. Called by stop(). */
    clear() {
      for (const set of listeners.values()) set.clear();
    },

    /** count is for tests and diagnostics. */
    count(event) {
      assertKnown(event, 'count');
      return listeners.get(event).size;
    },
  };
}
