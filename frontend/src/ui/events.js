// The pure rule behind the event auto-select and picker on the Settings screen.
//
// Owner: WP-5b.
//
// NO DOM, NO BACKEND IMPORT. This is the house pattern — validate.js/settings.js,
// presets.js/settings.js, overlay.js/app.js — and it is the only way this rule
// gets driven tests under node:test, because package.json is frozen and there is
// no jsdom to fake a <select> with. events.test.js drives it for real;
// settings.js is the wiring that turns its answer into a hidden field, a note or
// a dropdown.
//
// ======================= WHAT THIS DECIDES, AND WHY =========================
//
// The event id is an opaque string an operator does not remember — it used to be
// recoverable only by pasting the live-operation URL. Once the app can LIST the
// instance's events (a signed-in client against a reachable instance), it can do
// better, and the operator asked for exactly this: "by default for the event to
// be auto selected when there is only 1" — a picker only when there is a genuine
// choice. So:
//
//   0 events -> the app cannot improve on the URL. KEEP the current id, no
//               picker; the URL-derived value stands.
//   1 event  -> AUTO-SELECT it. No picker — this is the operator's explicit
//               default, and a picker with one option is a question with one
//               answer.
//   2+ events -> a picker. Its selection defaults to the current id when that id
//               is one of the events (so re-opening Settings does not silently
//               move the choice), and to the first event otherwise.
//
// The id-less filtering the Go side already does is repeated here defensively:
// an entry with no id cannot be fed to the KVS calls, so it must never be the
// thing "exactly one event" auto-selects, whatever the caller passed in.

/**
 * chooseEvent decides what the Settings screen does with the instance's events.
 *
 * @param {Array<{id: string, name: string, status: string}>} events  from
 *        backend.listEvents(); a non-array is treated as no events.
 * @param {string} currentEventId  the id already derived from the pasted URL (or
 *        loaded from config) — the fallback, and the picker's default when it is
 *        one of the events.
 * @returns {{
 *   count: number,          // usable (id-bearing) events
 *   autoSelected: boolean,  // true only when there is exactly one
 *   needsPicker: boolean,   // true only when there are two or more
 *   selectedId: string,     // the id the hidden field should hold
 *   selectedName: string,   // the name of the selected event, or '' when none
 * }}
 */
export function chooseEvent(events, currentEventId) {
  const list = Array.isArray(events) ? events.filter((e) => e && e.id) : [];
  const current = typeof currentEventId === 'string' ? currentEventId : '';
  const count = list.length;

  // 0 events: nothing to choose. Keep whatever the URL gave us and offer no
  // picker — the fallback the whole feature degrades to.
  if (count === 0) {
    return {
      count: 0,
      autoSelected: false,
      needsPicker: false,
      selectedId: current,
      selectedName: '',
    };
  }

  // Exactly 1: the operator's default. Choose it, say so, no picker.
  if (count === 1) {
    return {
      count: 1,
      autoSelected: true,
      needsPicker: false,
      selectedId: list[0].id,
      selectedName: list[0].name || '',
    };
  }

  // 2+: a genuine choice. Default to the current id if it is in the list (so a
  // re-open does not move the selection), else the first.
  const chosen = list.find((e) => e.id === current) || list[0];
  return {
    count,
    autoSelected: false,
    needsPicker: true,
    selectedId: chosen.id,
    selectedName: chosen.name || '',
  };
}
