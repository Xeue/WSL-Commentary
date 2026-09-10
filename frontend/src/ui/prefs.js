/**
 * prefs.js — per-machine conveniences, remembered in the page's localStorage.
 *
 * These are NOT configuration: nothing here reaches config.json, a preset, or
 * another seat. They are the things an operator sets by hand on THIS screen
 * and expects to find where they left them — the return level, whether the
 * controls column is folded away. The PGM monitor window has its own WebView2
 * profile, so its storage is its own; a remote seat's browser likewise.
 *
 * Every read and write is wrapped: storage can be absent (a test, a
 * thumbnail), disabled, or full, and a preference that cannot be kept is a
 * default, never an error.
 */

/** The return level, a 0..1 linear multiplier (levelmap.js sliderToLevel). */
export const PREF_RETURN_LEVEL = 'wslcomms.return.level';

/** Whether the PGM monitor window's controls column is collapsed. */
export const PREF_MONITOR_RAIL_COLLAPSED = 'wslcomms.monitor.railCollapsed';

function defaultStorage() {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

/**
 * readPref returns the stored JSON value for key, or null when there is none
 * or storage is unusable.
 * @param {string} key
 * @param {Storage|null} [storage]
 */
export function readPref(key, storage = defaultStorage()) {
  try {
    if (!storage) return null;
    const raw = storage.getItem(key);
    if (raw == null) return null;
    return JSON.parse(raw);
  } catch {
    return null;
  }
}

/**
 * writePref stores value as JSON under key. Returns whether it was kept.
 * @param {string} key
 * @param {unknown} value
 * @param {Storage|null} [storage]
 */
export function writePref(key, value, storage = defaultStorage()) {
  try {
    if (!storage) return false;
    storage.setItem(key, JSON.stringify(value));
    return true;
  } catch {
    return false;
  }
}
