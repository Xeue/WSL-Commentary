/**
 * The error log: what the dismissible banner remembers.
 *
 * Owner: WP-5b.
 *
 * The banner used to be one message wide. A second error overwrote the first,
 * so the operator who looked up after two things had gone wrong saw one of
 * them, and no trace that it had a predecessor. During a match that is not a
 * hypothetical: the SRT return failing and the mixer refusing a write are
 * different problems with different fixes, and whichever happened second was
 * eating the evidence of the first.
 *
 * This module is the memory: a list of everything shown, newest first, each
 * with when it first happened, when it last happened, and how many times.
 * home.js renders it; nothing here touches the DOM, which is what makes it
 * testable under node:test like every other model in this directory.
 *
 * ============================ THE REPEAT RULE ===============================
 *
 * A message identical to the NEWEST entry increments that entry rather than
 * adding a row. This is what keeps a retry loop honest without letting it
 * flood: a return that fails every backoff for ten minutes is one row saying
 * so and counting, not two hundred rows saying the same sentence. The
 * comparison is against the newest entry only — two problems alternating DO
 * produce alternating rows, because that is what happened.
 */

// The picture-state spellings come from the module that OWNS them and asserts
// them against internal/gst. Writing 'backoff' here as a string literal is the
// two-tables bug ./returns.js exists to record.
import {
  PICTURE_STATE_BACKOFF,
  PICTURE_STATE_CONNECTING,
} from './picturesource.js';
// The severity vocabulary, and the decisions about which messages are worth an
// operator's attention mid-match. It lives in ./alerts.js because it is a
// judgement about the PRODUCT, not about this list's bookkeeping, and because
// app.js has to classify a monitor error before it ever reaches this module.
import { SEVERITY, normaliseSeverity } from './alerts.js';

/**
 * ERROR_LOG_LIMIT caps the history. Fifty distinct errors is far beyond any
 * real session; the cap exists so that a pathological loop that composes a
 * DIFFERENT message each time (a timestamp in the text, say) cannot grow the
 * list without bound. The oldest fall off.
 */
export const ERROR_LOG_LIMIT = 50;

/**
 * createErrorLog builds the model.
 *
 * `now` is injectable for tests and defaults to the real clock. Entries are
 * plain objects: { message, severity, count, firstAt, lastAt }, newest first.
 *
 * ============================ SEVERITY =====================================
 *
 * Every entry carries one, defaulting to ALERT for the reason alerts.js's
 * normaliseSeverity records: a caller that forgets to classify should
 * over-report. It is part of the REPEAT KEY as well as of the row: a NOTE and an
 * ALERT that happen to share a sentence are different events, and merging them
 * would let a note's count hide an alert or the reverse. That has never happened
 * and the rule costs nothing; the alternative is a silent merge in the one
 * surface whose whole job is not to lose things.
 */
export function createErrorLog(now = () => new Date()) {
  /** @type {Array<{message: string, severity: string, count: number, firstAt: Date, lastAt: Date}>} */
  const entries = [];

  return {
    /**
     * record adds a message (or counts a repeat of the newest) and returns the
     * entry it landed in.
     *
     * @param {unknown} message
     * @param {string} [severity] one of alerts.js's SEVERITY; ALERT by default.
     */
    record(message, severity) {
      const text = String(message);
      const level = normaliseSeverity(severity);
      const at = now();
      const newest = entries[0];
      if (newest && newest.message === text && newest.severity === level) {
        newest.count += 1;
        newest.lastAt = at;
        return newest;
      }
      const entry = { message: text, severity: level, count: 1, firstAt: at, lastAt: at };
      entries.unshift(entry);
      if (entries.length > ERROR_LOG_LIMIT) entries.length = ERROR_LOG_LIMIT;
      return entry;
    },

    /**
     * dismiss removes ONE entry by identity — the column's per-row ✕.
     *
     * By identity rather than by index or by message: the list re-renders on
     * every arrival, so an index captured when a row was drawn can point at a
     * different row by the time it is clicked, and dismissing the wrong alert
     * mid-match is how a real one disappears unread.
     */
    dismiss(entry) {
      const i = entries.indexOf(entry);
      if (i >= 0) entries.splice(i, 1);
      return i >= 0;
    },

    /**
     * dismissMatching removes every entry with this exact message, whatever its
     * severity or position. This is the RESOLUTION path: the SRT picture coming
     * back takes its own complaint down without touching anything else, which
     * the old single-message banner could not do.
     */
    dismissMatching(message) {
      const text = String(message);
      let removed = 0;
      for (let i = entries.length - 1; i >= 0; i--) {
        if (entries[i].message === text) {
          entries.splice(i, 1);
          removed += 1;
        }
      }
      return removed;
    },

    /** clear empties the history. The banner's "Clear history" button. */
    clear() {
      entries.length = 0;
    },

    /** entries, newest first. The live array — callers render, not mutate. */
    get entries() {
      return entries;
    },

    /** size is the number of DISTINCT rows, not the sum of their counts. */
    get size() {
      return entries.length;
    },
  };
}

/**
 * formatErrorTime renders a timestamp for the history list: local HH:MM:SS.
 *
 * Local on purpose, unlike the log FILE, which is UTC because it travels.
 * The history is read by the operator who was in the room when it happened,
 * against the clock on their wall.
 */
export function formatErrorTime(date) {
  const p = (n) => String(n).padStart(2, '0');
  return `${p(date.getHours())}:${p(date.getMinutes())}:${p(date.getSeconds())}`;
}

/**
 * describeEntry is one history row's text: time, message, and a repeat count
 * when there is one. The time shown is the LAST occurrence — "is this still
 * happening" is the question the list answers; firstAt is in the tooltip
 * home.js sets, for "when did it start".
 */
export function describeEntry(entry) {
  const times = entry.count > 1 ? ` (×${entry.count})` : '';
  return `${formatErrorTime(entry.lastAt)}  ${entry.message}${times}`;
}

/**
 * createBackoffEpisode tracks the SRT picture's reconnect EPISODES, so the
 * banner can speak once per failure rather than once per retry.
 *
 * The receiver cycles connecting → backoff → connecting → backoff for as long
 * as the far end is unreachable. The old inline status line could re-render
 * that cycle for free; a BANNER cannot — raised on every transition into
 * backoff it would reappear seconds after every dismissal, for the rest of the
 * match. An episode is the whole unbroken run of failures: it RAISES on the
 * first backoff, stays silent through the retry cycling, and CLEARS when the
 * picture is genuinely back ("showing") or nobody wants it any more ("stopped"
 * or the receiver going away).
 *
 * track() returns what the caller should do to the banner: 'raise', 'clear',
 * or null for "nothing".
 */
export function createBackoffEpisode() {
  let inEpisode = false;

  return {
    /**
     * track takes the NORMALISED picture state (picturesource.js spelling:
     * 'stopped' | 'connecting' | 'showing' | 'backoff', or '' / null for
     * unknown or gone).
     */
    track(state) {
      if (state === PICTURE_STATE_BACKOFF) {
        if (inEpisode) return null;
        inEpisode = true;
        return 'raise';
      }
      // 'connecting' while failing is the retry in progress: the episode is
      // not over until the picture is showing or the receiver is stopped.
      if (state === PICTURE_STATE_CONNECTING) return null;
      if (inEpisode) {
        inEpisode = false;
        return 'clear';
      }
      return null;
    },
  };
}
