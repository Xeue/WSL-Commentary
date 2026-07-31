/**
 * The PGM crop, computed from the mosaic that ACTUALLY ARRIVED.
 *
 * Owner: WP-5b. Pure — no DOM, no browser API. home.js applies what this
 * computes to the CSS custom properties `.pgm-tile` reads.
 *
 * ======================== WHY THIS MODULE EXISTS ============================
 * config.monitorTile is a rectangle in PIXELS — {x:0, y:360, w:640, h:360} —
 * and a rectangle in pixels means nothing without the picture it was measured
 * against. It was measured on the dev event, where every multiviewer track
 * reported 2240x1440 (docs/test-results.md §2.3), and that same document says
 * in terms:
 *
 *   "Do not hard-code tile coordinates without confirming they are fixed
 *    across input counts. [UNTESTED]"
 *
 * The application did hard-code them. `.pgm-tile` sized itself from the tile
 * and positioned the <video> from a compiled-in 2240x1440, so the day the live
 * mosaic arrived at any other size the crop pointed at the wrong region and the
 * commentator got a picture that was the wrong part of the multiviewer, or a
 * black margin, or both.
 *
 * So: read videoWidth/videoHeight off the element, and scale the configured
 * rectangle by the ratio between the mosaic it was measured against and the
 * mosaic that arrived. A 640x360 tile at (0,360) of a 2240x1440 mosaic is the
 * same REGION as a 320x180 tile at (0,180) of a 1120x720 one, and it is the
 * region the operator meant.
 *
 * This is a rescale, not a re-measure. If Sony changes the multiviewer LAYOUT
 * — a different number of inputs, a different arrangement — no arithmetic here
 * can know, and the operator has to re-measure the tile. What this removes is
 * the far more likely case: the same layout delivered at a different
 * resolution.
 * ===========================================================================
 */

/**
 * REFERENCE_MOSAIC is the mosaic size config.monitorTile was measured against.
 * It is the denominator of the rescale, not a claim about the live track.
 */
export const REFERENCE_MOSAIC = Object.freeze({ w: 2240, h: 1440 });

/** DEFAULT_TILE mirrors internal/config.DefaultMonitorTile. */
export const DEFAULT_TILE = Object.freeze({ x: 0, y: 360, w: 640, h: 360 });

function num(v) {
  if (typeof v === 'string' && v.trim() !== '') {
    const n = Number(v);
    return Number.isFinite(n) ? n : undefined;
  }
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function positive(v) {
  const n = num(v);
  return n !== undefined && n > 0 ? n : undefined;
}

function nonNegative(v) {
  const n = num(v);
  return n !== undefined && n >= 0 ? n : undefined;
}

/**
 * normaliseMosaic coerces {width,height} or {w,h} into {w,h}, or returns null
 * if neither dimension is usable. A <video> with no metadata yet reports 0x0,
 * which must read as "not known" rather than as a mosaic of zero size.
 *
 * @param {unknown} m
 * @returns {{w: number, h: number}|null}
 */
export function normaliseMosaic(m) {
  if (!m || typeof m !== 'object') return null;
  const w = positive(m.w !== undefined ? m.w : m.width);
  const h = positive(m.h !== undefined ? m.h : m.height);
  if (w === undefined || h === undefined) return null;
  return { w, h };
}

/**
 * normaliseTile coerces a tile from config.json into a usable rectangle within
 * a mosaic.
 *
 * A missing or nonsensical field falls back per-field to DEFAULT_TILE rather
 * than being clamped: a negative width is a typo, not a small tile, and showing
 * the documented PGM region beats showing a sliver of the top-left corner.
 * After that the rectangle is clipped so it cannot extend past the mosaic's
 * edges — a tile that hangs off the picture would be rendered as black.
 *
 * @param {unknown} tile
 * @param {{w: number, h: number}} [mosaic] defaults to REFERENCE_MOSAIC
 * @returns {{x: number, y: number, w: number, h: number}}
 */
export function normaliseTile(tile, mosaic = REFERENCE_MOSAIC) {
  const m = normaliseMosaic(mosaic) || REFERENCE_MOSAIC;
  const src = tile && typeof tile === 'object' ? tile : {};

  let x = nonNegative(src.x);
  let y = nonNegative(src.y);
  let w = positive(src.w);
  let h = positive(src.h);

  if (x === undefined) x = DEFAULT_TILE.x;
  if (y === undefined) y = DEFAULT_TILE.y;
  if (w === undefined) w = DEFAULT_TILE.w;
  if (h === undefined) h = DEFAULT_TILE.h;

  x = Math.min(Math.round(x), Math.max(0, m.w - 1));
  y = Math.min(Math.round(y), Math.max(0, m.h - 1));
  w = Math.min(Math.max(1, Math.round(w)), m.w - x);
  h = Math.min(Math.max(1, Math.round(h)), m.h - y);

  return { x, y, w, h };
}

/**
 * rescaleTile maps a tile measured against one mosaic onto another, preserving
 * the region it describes.
 *
 * Both axes are scaled independently, because a mosaic can change aspect ratio
 * (2240x1440 is 14:9, not 16:9 — the multiviewer is not a picture, it is a
 * grid) and forcing one factor onto both would move the tile off its row.
 *
 * @param {unknown} tile the configured rectangle
 * @param {{w: number, h: number}} from the mosaic it was measured against
 * @param {{w: number, h: number}} to the mosaic that actually arrived
 * @returns {{x: number, y: number, w: number, h: number}} a tile in `to`'s pixels
 */
export function rescaleTile(tile, from, to) {
  const src = normaliseMosaic(from) || REFERENCE_MOSAIC;
  const dst = normaliseMosaic(to);
  const t = normaliseTile(tile, src);

  // Nothing usable to rescale onto: the configured tile is the best available
  // answer and is returned untouched rather than multiplied by a guess.
  if (!dst) return t;
  if (dst.w === src.w && dst.h === src.h) return t;

  const kx = dst.w / src.w;
  const ky = dst.h / src.h;

  return normaliseTile(
    {
      x: Math.round(t.x * kx),
      y: Math.round(t.y * ky),
      w: Math.round(t.w * kx),
      h: Math.round(t.h * ky),
    },
    dst,
  );
}

/**
 * effectiveCrop is what home.js actually calls: everything needed to write the
 * CSS custom properties, plus enough about what happened to log it.
 *
 * @param {unknown} configTile config.monitorTile
 * @param {unknown} liveMosaic {width,height} from the <video> element, or null
 *   before metadata has arrived
 * @param {{w: number, h: number}} [reference] the mosaic configTile was
 *   measured against; defaults to REFERENCE_MOSAIC
 * @returns {{
 *   tile: {x: number, y: number, w: number, h: number},
 *   mosaic: {w: number, h: number},
 *   reference: {w: number, h: number},
 *   rescaled: boolean,
 *   known: boolean,
 *   aspect: number,
 * }}
 */
export function effectiveCrop(configTile, liveMosaic, reference = REFERENCE_MOSAIC) {
  const ref = normaliseMosaic(reference) || REFERENCE_MOSAIC;
  const live = normaliseMosaic(liveMosaic);
  const mosaic = live || ref;
  const tile = live ? rescaleTile(configTile, ref, live) : normaliseTile(configTile, ref);

  return {
    tile,
    mosaic,
    reference: ref,
    rescaled: !!live && (live.w !== ref.w || live.h !== ref.h),
    known: !!live,
    aspect: tile.w / tile.h,
  };
}

/**
 * describeCrop is the one line that goes in the console when the crop changes.
 * It states BOTH mosaics, because "the picture is in the wrong place" is
 * otherwise undiagnosable from a screenshot.
 *
 * @param {ReturnType<typeof effectiveCrop>} crop
 * @param {unknown} configTile
 * @returns {string}
 */
export function describeCrop(crop, configTile) {
  const c = normaliseTile(configTile, crop.reference);
  const configured = `${c.w}x${c.h} at (${c.x},${c.y}) of ${crop.reference.w}x${crop.reference.h}`;
  if (!crop.known) {
    return `wslcomms: PGM crop ${configured} — no video track metadata yet, using the configured mosaic size`;
  }
  const live = `${crop.mosaic.w}x${crop.mosaic.h}`;
  if (!crop.rescaled) {
    return `wslcomms: PGM crop ${configured}; live mosaic is ${live}, the size it was measured against`;
  }
  return (
    `wslcomms: PGM crop rescaled — configured ${configured}, ` +
    `live mosaic is ${live}, using ${crop.tile.w}x${crop.tile.h} at (${crop.tile.x},${crop.tile.y})`
  );
}
