/**
 * Mosaic geometry: turning a tile rectangle and a scale factor into CSS.
 *
 * Owner: WP-5a. Pure — no DOM, no browser API. video.js applies what this
 * computes.
 *
 * ============================ THIS IS CONFIGURATION ============================
 * The tile rectangle is **M2L-X configuration measured on the dev event**, not a
 * constant. docs/test-results.md §2.3: all 31 <video> elements in Sony's own GUI
 * report intrinsic 2240x1440 and share exactly one video track id, wrapped in 31
 * distinct MediaStream objects and CSS-cropped per tile. The computed source
 * rects were PVW (0, 0, 640, 360) and PGM (0, 360, 640, 360), with the source
 * thumbnails on a 320x180 grid.
 *
 * That document also says, in terms: "Do not hard-code tile coordinates without
 * confirming they are fixed across input counts. [UNTESTED]". So the tile comes
 * in through opts.tile from config.monitorTile (spec §9), and the constants
 * below are defaults, not truths.
 * ===============================================================================
 */

/** MOSAIC_WIDTH is the measured intrinsic width of the multiviewer video track. */
export const MOSAIC_WIDTH = 2240;

/** MOSAIC_HEIGHT is the measured intrinsic height of the multiviewer video track. */
export const MOSAIC_HEIGHT = 1440;

/**
 * DEFAULT_TILE is the PGM tile: (0, 360, 640, 360). This is the same default
 * `internal/config` writes into config.json as `monitorTile`, and the two must
 * stay in step — but config.json wins at runtime.
 */
export const DEFAULT_TILE = Object.freeze({ x: 0, y: 360, w: 640, h: 360 });

/**
 * MIN_SCALE / MAX_SCALE bound the CSS scale factor. A scale of zero collapses
 * the picture to nothing and a scale of NaN silently removes the transform,
 * which shows the commentator the *whole multiviewer* instead of PGM — a wrong
 * picture that looks deliberate. Both are worth clamping out.
 */
export const MIN_SCALE = 0.05;
export const MAX_SCALE = 8;

function finitePositive(v) {
  return typeof v === 'number' && Number.isFinite(v) && v > 0;
}

function finiteNonNegative(v) {
  return typeof v === 'number' && Number.isFinite(v) && v >= 0;
}

/**
 * normaliseTile coerces a tile from config.json into a usable rectangle.
 *
 * Rules, all chosen so that a bad config file degrades to "shows the wrong
 * region" rather than "shows nothing":
 *   - a non-object, or any missing/non-finite field, falls back per-field to
 *     DEFAULT_TILE;
 *   - numeric strings are accepted (config round-trips through JSON and may be
 *     hand-edited);
 *   - a negative x or y, or a zero/negative w or h, is not a value to clamp but
 *     a typo: it falls back per-field to DEFAULT_TILE, so a botched Settings
 *     save shows the PGM tile rather than a sliver of the top-left corner;
 *   - after that, x and y are clamped inside the mosaic and w and h are clipped
 *     so the tile cannot extend past its edges.
 *
 * @param {unknown} tile
 * @param {{width?: number, height?: number}} [mosaic] defaults to the measured 2240x1440
 * @returns {{x: number, y: number, w: number, h: number}}
 */
export function normaliseTile(tile, mosaic = {}) {
  const mw = finitePositive(mosaic.width) ? mosaic.width : MOSAIC_WIDTH;
  const mh = finitePositive(mosaic.height) ? mosaic.height : MOSAIC_HEIGHT;
  const src = tile && typeof tile === 'object' ? tile : {};

  const num = (v) => {
    if (typeof v === 'string' && v.trim() !== '') {
      const n = Number(v);
      return Number.isFinite(n) ? n : undefined;
    }
    return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
  };

  let x = num(src.x);
  let y = num(src.y);
  let w = num(src.w);
  let h = num(src.h);

  if (!finiteNonNegative(x)) x = DEFAULT_TILE.x;
  if (!finiteNonNegative(y)) y = DEFAULT_TILE.y;
  if (!finitePositive(w)) w = DEFAULT_TILE.w;
  if (!finitePositive(h)) h = DEFAULT_TILE.h;

  x = Math.min(Math.max(0, Math.round(x)), Math.max(0, mw - 1));
  y = Math.min(Math.max(0, Math.round(y)), Math.max(0, mh - 1));
  w = Math.min(Math.max(1, Math.round(w)), mw - x);
  h = Math.min(Math.max(1, Math.round(h)), mh - y);

  return { x, y, w, h };
}

/**
 * clampScale bounds a scale factor and resolves anything non-finite to 1.
 *
 * @param {unknown} k
 * @returns {number}
 */
export function clampScale(k) {
  const n = typeof k === 'string' && k.trim() !== '' ? Number(k) : k;
  if (typeof n !== 'number' || !Number.isFinite(n) || n <= 0) return 1;
  return Math.min(Math.max(n, MIN_SCALE), MAX_SCALE);
}

/**
 * fitScale computes the scale factor that fits the tile into the given box.
 *
 * If only a width is supplied the tile is scaled to that width, which is what
 * spec §10 asks for ("The PGM tile fills the top, scaled to the window width").
 * If a height is supplied too, the smaller of the two ratios wins so the tile
 * never overflows its box — the tile's aspect ratio is preserved either way,
 * because a stretched programme picture in a commentary position is a bug
 * report waiting to happen.
 *
 * @param {{w: number, h: number}} tile a normalised tile
 * @param {number} availWidth CSS pixels available
 * @param {number} [availHeight] CSS pixels available; omit or pass 0 to fit width only
 * @returns {number} the clamped scale factor
 */
export function fitScale(tile, availWidth, availHeight) {
  const t = normaliseTile(tile);
  const byWidth = finitePositive(availWidth) ? availWidth / t.w : NaN;
  const byHeight = finitePositive(availHeight) ? availHeight / t.h : NaN;

  let k;
  if (Number.isFinite(byWidth) && Number.isFinite(byHeight)) k = Math.min(byWidth, byHeight);
  else if (Number.isFinite(byWidth)) k = byWidth;
  else if (Number.isFinite(byHeight)) k = byHeight;
  else k = 1;

  return clampScale(k);
}

/**
 * cropStyles produces the three sets of CSS declarations that implement the
 * crop described in spec §7:
 *
 *   stage  — occupies tile.w*k x tile.h*k in the layout, so the surrounding
 *            page flows correctly. `transform` does not affect layout, which is
 *            the one trap in the spec's recipe: without this element the crop
 *            box would still claim its unscaled 640x360 of layout space.
 *   crop   — tile.w x tile.h, overflow hidden, transform: scale(k) with
 *            transform-origin: 0 0.
 *   video  — the <video> at the mosaic's natural size, offset by -tile.x /
 *            -tile.y so the wanted region lands at the crop box's origin.
 *
 * Decode cost is the whole mosaic either way and it is hardware-decoded, so
 * there is nothing to be gained by trying to decode less.
 *
 * @param {{x: number, y: number, w: number, h: number}} tile normalised tile
 * @param {number} scale clamped scale factor
 * @param {{width?: number, height?: number}} [mosaic] intrinsic video size
 * @returns {{stage: Record<string,string>, crop: Record<string,string>, video: Record<string,string>}}
 */
export function cropStyles(tile, scale, mosaic = {}) {
  const t = normaliseTile(tile, mosaic);
  const k = clampScale(scale);
  const mw = finitePositive(mosaic.width) ? mosaic.width : MOSAIC_WIDTH;
  const mh = finitePositive(mosaic.height) ? mosaic.height : MOSAIC_HEIGHT;

  return {
    stage: {
      position: 'relative',
      overflow: 'hidden',
      width: `${t.w * k}px`,
      height: `${t.h * k}px`,
      // The stage is a plain block: it must not pick up flex/grid stretching
      // from whatever WP-5b wraps it in, or the crop box drifts off its origin.
      flex: '0 0 auto',
      lineHeight: '0',
    },
    crop: {
      position: 'absolute',
      top: '0',
      left: '0',
      overflow: 'hidden',
      width: `${t.w}px`,
      height: `${t.h}px`,
      transform: `scale(${k})`,
      transformOrigin: '0 0',
      // Black rather than transparent: before the first frame arrives this is
      // what the commentator looks at, and a black rectangle reads as "no
      // picture yet" while a transparent one reads as "broken layout".
      backgroundColor: '#000',
    },
    video: {
      position: 'absolute',
      left: `${-t.x}px`,
      top: `${-t.y}px`,
      width: `${mw}px`,
      height: `${mh}px`,
      maxWidth: 'none',
      maxHeight: 'none',
      objectFit: 'fill',
      display: 'block',
      // The mosaic is a monitor feed, not a reference picture. Telling the
      // compositor it will move keeps the scale animation off the main thread.
      pointerEvents: 'none',
    },
  };
}

/**
 * layoutSize is what WP-5b needs to size the area it gives the monitor: the
 * on-screen size of the cropped tile at the current scale.
 *
 * @param {{w: number, h: number}} tile normalised tile
 * @param {number} scale clamped scale factor
 * @returns {{width: number, height: number}}
 */
export function layoutSize(tile, scale) {
  const t = normaliseTile(tile);
  const k = clampScale(scale);
  return { width: t.w * k, height: t.h * k };
}
