/**
 * The mixer drawer's entry point. Import THIS, not ./drawer.js.
 *
 * Owner: WP-M4.
 *
 *   import { createMixerDrawer } from './mixer/index.js';
 *
 * ./contract.js says WP-M4 "replaces the body" of its createMixerDrawer with
 * the real drawer. It could not: contract.js is owned by WP-M0 and frozen for
 * this work package (see the report). The implementation is therefore in
 * ./drawer.js in the same directory, as the contract requires, and this module
 * is the single name the host binds to. contract.js's own createMixerDrawer
 * still throws and must not be called.
 *
 * The split exists for one more reason: this file is where ./mixer.css is
 * imported, so ./drawer.js stays loadable by `node --test`, which cannot parse
 * a CSS import. Nothing else here may import mixer.css.
 */

import './mixer.css';

export { createMixerDrawer } from './drawer.js';

// Re-exported for the host's own status line and for anything that needs the
// drawer's vocabulary without constructing one.
export { BUS_LABELS, ALL_BUSES, CLEAN_FEED_BUS, NO_EGRESS_BUSES, busLabel } from './contract.js';
export {
  METER_FLOOR_DB,
  METER_CEIL_DB,
  MUTE_FADER_DB,
  buildMatrixModel,
  compareSnapshots,
  diffHeadline,
  formatDb,
  meterFraction,
  meterPercent,
  meterZone,
  sortDiffs,
  stripLabel,
} from './model.js';
