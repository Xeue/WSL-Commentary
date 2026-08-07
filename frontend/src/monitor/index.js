/**
 * The monitor module's public surface.
 *
 * Owner: WP-5a.
 *
 * The seam with WP-5b is `createMonitor` from ./monitor.js and nothing else is
 * required. Everything else re-exported here is convenience for the UI:
 *
 *   BUS_MAP / busForMid       the Return dropdown's labels. spec §10 says that
 *                             dropdown has exactly two entries, CLN and PGM
 *                             (mids 2 and 1); the rest of the map is here so
 *                             the label does not have to be duplicated.
 *   DEFAULT_RETURN_MID        2 — aux1/CLN.
 *   DEFAULT_GAIN_DB           18 — the measured make-up gain.
 *   DEFAULT_TILE              {0,360,640,360} — the measured PGM tile.
 *   MonitorErrorCode          for a UI that wants to branch on the failure.
 *   CHANNEL_MODES             the return channel selector's three options and
 *                             the routing behind them. See below.
 *
 * Nothing in frontend/src/ui/, frontend/src/styles/, frontend/index.html or
 * frontend/src/main.js belongs to WP-5a, and nothing in frontend/src/monitor/
 * belongs to WP-5b. This file is the whole of the boundary.
 *
 * ONE EXCEPTION, stated so it is a decision rather than a drift: ./channels.js
 * may be imported directly from frontend/src/ui/. It is pure data and pure
 * arithmetic — no DOM, no Web Audio, no AWS SDK — and importing it through this
 * file would pull the whole KVS viewer into the import graph of a validator.
 * The alternative, a second hand-written copy of "Stereo / Left only / Right
 * only" on the UI side, is the exact bug frontend/src/ui/returns.js exists to
 * record: two tables that agreed with each other and were both wrong.
 */

export { createMonitor } from './monitor.js';

export {
  BUS_MAP,
  busForMid,
  isValidReturnMid,
  normaliseReturnMid,
  DEFAULT_RETURN_MID,
  VIDEO_MID,
  AUDIO_MIDS,
} from './buses.js';

export { DEFAULT_TILE, MOSAIC_WIDTH, MOSAIC_HEIGHT, normaliseTile, fitScale } from './geometry.js';

export { DEFAULT_GAIN_DB, MIN_GAIN_DB, MAX_GAIN_DB, dbToLinear, computeGain } from './gain.js';

export {
  CHANNEL_STEREO,
  CHANNEL_LEFT,
  CHANNEL_RIGHT,
  CHANNEL_MODES,
  DEFAULT_CHANNEL_MODE,
  isValidChannelMode,
  normaliseChannelMode,
  sourceChannelsForOutputs,
  channelRouting,
  describeChannelMode,
  channelModeLabel,
} from './channels.js';

export { MonitorError, MonitorErrorCode } from './errors.js';

export { kvsSdkVersion } from './kvs-sdk.js';
