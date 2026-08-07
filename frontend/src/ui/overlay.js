/**
 * The native picture overlay: reserving a rectangle, and describing it to Go.
 *
 * Owner: WP-5b. Pure — no DOM, no browser API. The measuring and the IPC are
 * injected, which is what lets every rule below be tested rather than reasoned
 * about.
 *
 * ======================= WHAT THE FRONTEND'S JOB BECAME =====================
 *
 * A browser element cannot play SRT. The high-quality picture is decoded in Go
 * and painted by a NATIVE CHILD WINDOW sitting on top of this page. So the
 * frontend stops displaying video and starts doing two things instead:
 *
 *   1. RESERVE a rectangle — the same .pgm-tile box the mosaic uses, so the
 *      layout is identical whichever picture is showing and switching between
 *      them does not move the controls.
 *
 *   2. DESCRIBE it every time it changes.
 *
 * ======================== PHYSICAL PIXELS, COMPUTED TWICE ===================
 *
 * getBoundingClientRect returns CSS pixels. A native window is positioned in
 * physical device pixels. On the operator's machine those are not the same
 * number — a 3840-wide display at 150% scaling reports 2560 CSS pixels — and the
 * failure mode of getting it wrong is not an error: it is a picture two thirds
 * of the right size, sitting up and to the left of where it belongs, with
 * everything still reporting success.
 *
 * THE CONVERSION HAPPENS ON THE GO SIDE, in gst.ScaleRect, from CSS pixels and
 * the ratio this page measured — because Go cannot read the ratio for itself
 * without getting a different number: GetDpiForWindow is the monitor's scale
 * factor, and Ctrl+scroll on a WebView2 changes the page's ratio without
 * changing it. The ratio therefore travels with the rectangle in one call.
 *
 * toPhysicalRect below does the SAME arithmetic anyway, and that is deliberate
 * rather than duplicated work. It is what decides whether to report at all: a
 * DPI change moves every physical coordinate without moving the CSS box by a
 * pixel, so change detection on the CSS box alone would miss it entirely and
 * change detection on nothing would put an IPC call behind every resize frame.
 * It is also what goes in the console line, so the number logged here is the
 * number Go will land on.
 *
 * It matches gst.ScaleRect exactly, and both round the EDGES rather than the
 * origin and the size:
 *
 *     left  = round(x * dpr)          right  = round((x + w) * dpr)
 *     width = right - left
 *
 * Rounding origin and size independently lets both round the same way and leaves
 * a sub-pixel seam of page background along the bottom or right edge of the
 * picture, on some window sizes and not others. Rounding the edges makes
 * adjacent boxes share an edge exactly, which is the property that matters.
 *
 * ========================= AND THE ORIGIN IS THE CLIENT AREA =================
 *
 * The rectangle is relative to the TOP-LEFT OF THE WEBVIEW CLIENT AREA, not the
 * screen. The native window is a child of the same top-level window, so dragging
 * the window changes nothing about it — which is why there is no "window moved"
 * report anywhere in this application, and why nobody has to find out that the
 * page cannot observe one.
 *
 * A DPI change is different: it changes the multiplier, so it must re-report
 * even when the CSS box has not moved by a pixel. app.js watches for it; sync()
 * re-reads the ratio on every call so it cannot be missed.
 *
 * ======================= IT IS OPAQUE AND IT IS ON TOP ======================
 *
 * That is the whole hazard. A native child window is not in the page's stacking
 * context and no amount of z-index reaches it: anything the page draws over that
 * rectangle is simply not visible. Settings, the mixer drawer, a modal — every
 * one of them would be a screen the operator can see two thirds of.
 *
 * So visibility is EXPLICIT and it is a SET of reasons, not a boolean.
 * Two things can want it hidden at once — Settings opened from behind the mixer
 * drawer — and if hiding were a boolean then whichever closed first would
 * uncover the other. block()/unblock() name their reason and the overlay stays
 * hidden until the last one has gone.
 */

/**
 * BLOCK_SETTINGS and BLOCK_MIXER are the two screens that must never be covered.
 * They are constants rather than free strings so that a typo in unblock() cannot
 * silently leave the overlay hidden for the rest of the match.
 */
export const BLOCK_SETTINGS = 'settings';
export const BLOCK_MIXER = 'mixer';

/** BLOCK_HIDDEN is the home view itself not being on screen. */
export const BLOCK_HIDDEN = 'home-not-visible';

function finite(v) {
  return typeof v === 'number' && Number.isFinite(v);
}

/**
 * DPR_CEILING is the largest device pixel ratio treated as real, mirroring
 * gst.ScaleRect's own bound. Far beyond any shipping display; it is there
 * because a NaN or an absurd value crossing the Wails boundary reaches an int
 * conversion in Go, which is implementation-defined for out-of-range floats.
 */
export const DPR_CEILING = 16;

/**
 * normaliseDpr coerces a devicePixelRatio to something usable, by exactly the
 * rule gst.ScaleRect applies to the same number.
 *
 * 1 is the fallback, not 0: a ratio of zero would collapse every rectangle to
 * nothing, and a picture of zero size is indistinguishable from a picture that
 * never started.
 *
 * @param {unknown} dpr
 * @returns {number}
 */
export function normaliseDpr(dpr) {
  return finite(dpr) && dpr > 0 && dpr <= DPR_CEILING ? dpr : 1;
}

/**
 * roundHalfAwayFromZero is gst.ScaleRect's rounding, written out.
 *
 * Math.round would do for every coordinate this application will ever see — a
 * client-area rectangle is not negative — but it rounds -0.5 towards positive
 * infinity where Go rounds it away from zero, and two implementations of the
 * same arithmetic that differ anywhere are two implementations that have to be
 * argued about rather than compared.
 */
function roundHalfAwayFromZero(v) {
  return v >= 0 ? Math.floor(v + 0.5) : Math.ceil(v - 0.5);
}

/**
 * toPhysicalRect converts a CSS-pixel rectangle to integer physical pixels.
 *
 * Returns null for a rectangle that is not usable — no measurement yet, a
 * collapsed box, a hidden element reporting zeros. Null means "do not report",
 * which is different from reporting a rectangle of zero size: the first leaves
 * the overlay where it was, the second would move it to a corner and shrink it
 * to nothing.
 *
 * @param {unknown} rect {x,y,width,height} or {left,top,width,height}
 * @param {unknown} dpr
 * @returns {{x: number, y: number, w: number, h: number}|null}
 */
export function toPhysicalRect(rect, dpr) {
  if (!rect || typeof rect !== 'object') return null;
  const x = finite(rect.x) ? rect.x : rect.left;
  const y = finite(rect.y) ? rect.y : rect.top;
  const w = finite(rect.width) ? rect.width : rect.w;
  const h = finite(rect.height) ? rect.height : rect.h;
  if (!finite(x) || !finite(y) || !finite(w) || !finite(h)) return null;
  if (!(w > 0) || !(h > 0)) return null;

  const k = normaliseDpr(dpr);
  // Edges, not origin-and-size. See the header, and gst.ScaleRect.
  const left = roundHalfAwayFromZero(x * k);
  const top = roundHalfAwayFromZero(y * k);
  const right = roundHalfAwayFromZero((x + w) * k);
  const bottom = roundHalfAwayFromZero((y + h) * k);

  const width = right - left;
  const height = bottom - top;
  if (width < 1 || height < 1) return null;

  return { x: left, y: top, w: width, h: height };
}

/**
 * rectsEqual compares two physical rectangles, either of which may be null.
 * @param {{x: number, y: number, w: number, h: number}|null} a
 * @param {{x: number, y: number, w: number, h: number}|null} b
 * @returns {boolean}
 */
export function rectsEqual(a, b) {
  if (a === b) return true;
  if (!a || !b) return false;
  return a.x === b.x && a.y === b.y && a.w === b.w && a.h === b.h;
}

/**
 * describeRect is the one console line when the rectangle moves. It states the
 * ratio as well as the numbers, because "the picture is the wrong size" is
 * undiagnosable from a screenshot without knowing which multiplier was applied.
 *
 * @param {{x: number, y: number, w: number, h: number}} rect
 * @param {number} dpr
 * @returns {string}
 */
export function describeRect(rect, dpr) {
  if (!rect) return 'wslcomms: picture overlay has no usable rectangle yet';
  return (
    `wslcomms: picture overlay ${rect.w}x${rect.h} at (${rect.x},${rect.y}) ` +
    `physical pixels, device pixel ratio ${normaliseDpr(dpr)}`
  );
}

/**
 * @typedef {object} OverlayIO
 * @property {() => (object|null)} measure   the CSS rect of the reserved box
 * @property {() => number} dpr              window.devicePixelRatio, read fresh
 * @property {(css: object, dpr: number, physical: object) => void} setRect
 *   Called with the CSS rectangle and the ratio it was measured with — which is
 *   what App.SetPictureRect takes, together, in one call — and with the physical
 *   rectangle this module derived from them, for logging and for tests. The IPC
 *   must send the first two: gst.ScaleRect does the multiplication, because the
 *   ratio Go could read for itself is a different number.
 * @property {(visible: boolean) => void} setVisible
 * @property {(visible: boolean) => void} [onVisible]
 *   Called with the SAME answer setVisible is given, but on every apply rather
 *   than only on a change, and never conditionally. It is how the page learns
 *   whether the native window is on screen, which is what decides whether the
 *   fallback mosaic underneath is suppressed. See shouldShow.
 * @property {(message: string) => void} [log]
 */

/**
 * createOverlay builds the controller.
 *
 * It starts BLOCKED and INVISIBLE. That is deliberate and it is the safe
 * direction: an overlay that has not yet been told where to be, painted at
 * whatever rectangle a native default puts it at, is an opaque box over an
 * application the operator is trying to configure. Nothing shows until sync()
 * has measured a real rectangle and nothing is blocking.
 *
 * @param {OverlayIO} io
 */
export function createOverlay(io) {
  const log = typeof io.log === 'function' ? io.log : () => {};
  const blockers = new Set([BLOCK_HIDDEN]);

  /** @type {{x: number, y: number, w: number, h: number}|null} */
  let rect = null;
  let visible = false;
  /** What the picture control and the receiver's state jointly want. */
  let wanted = false;
  let lastDescription = '';

  function callSetRect(css, dpr, physical) {
    try {
      io.setRect(css, dpr, physical);
    } catch (err) {
      // A build without the binding, or a Go side that is not up yet. It is a
      // console line and not a banner: the mosaic is still on screen and the
      // commentator has a picture. See app.js for where this is surfaced once.
      log(`wslcomms: could not report the picture rectangle: ${err?.message || err}`);
    }
  }

  function callSetVisible(next) {
    try {
      io.setVisible(next);
    } catch (err) {
      log(`wslcomms: could not set the picture overlay visibility: ${err?.message || err}`);
    }
  }

  /**
   * callOnVisible tells the page what the native window is doing.
   *
   * ================= WHY THE PAGE HAS TO BE TOLD THIS AT ALL ==================
   *
   * Because the mosaic underneath is suppressed while the overlay covers it, and
   * the page cannot work out for itself whether it is covered. It used to guess,
   * from "SRT is the chosen source" — which is a different fact. The overlay is
   * hidden whenever something must appear above it (the mixer drawer, Settings,
   * a modal), and none of those touch the source, so opening the drawer hid the
   * native video and left the mosaic suppressed: the commentator got BLACK.
   *
   * So this is the overlay's own visibility, the same expression that decides
   * the native SetVisible, and the invariant it carries is: WHENEVER THE OVERLAY
   * IS NOT VISIBLE, THE MOSAIC IS.
   *
   * It is called on every apply, not only on a change, and that asymmetry with
   * setVisible is deliberate. setVisible is IPC and repeating it is a swapchain
   * resize; this is a class name on an element the page already owns, and making
   * it unconditional means the page's state is a pure function of the overlay's
   * with no memory to get out of step — which is worth more than the toggle it
   * saves.
   */
  function callOnVisible(next) {
    if (typeof io.onVisible !== 'function') return;
    try {
      io.onVisible(next);
    } catch (err) {
      log(`wslcomms: could not report the picture overlay visibility to the page: ${err?.message || err}`);
    }
  }

  /**
   * shouldShow is the whole visibility rule, in one expression.
   *
   * Every term is necessary and the order they are written in is the order they
   * matter: no rectangle means nothing to show; a blocker means something the
   * operator is reading is behind it; and `wanted` is the picture control and the
   * receiver's own state agreeing that there is an SRT picture to paint.
   */
  function shouldShow() {
    return rect !== null && blockers.size === 0 && wanted;
  }

  function apply() {
    const next = shouldShow();
    if (next !== visible) {
      visible = next;
      callSetVisible(visible);
    }
    // Unconditional, and AFTER the window has been told, so the page never
    // uncovers the mosaic a moment before the native window has gone. See
    // callOnVisible for why the page is told at all.
    callOnVisible(visible);
  }

  return {
    /**
     * sync re-measures and reports. Safe and cheap to call on every resize,
     * every DPI change, every view swap: it only reaches Go when something has
     * actually moved.
     *
     * @returns {{rect: object|null, visible: boolean, changed: boolean}}
     */
    sync() {
      // Both read in the same breath, and both sent together. A rectangle
      // measured now and a ratio measured a moment later describe a window that
      // existed at neither instant.
      const dpr = io.dpr();
      const css = io.measure();
      const next = toPhysicalRect(css, dpr);
      let changed = false;

      // A rectangle that cannot be measured does NOT clear the last one. The
      // usual reason for it is the home view being hidden, and in that state the
      // overlay is blocked anyway — throwing the geometry away would mean
      // reporting a fresh rectangle, and a frame at the wrong place, every time
      // somebody came back from Settings.
      if (next && !rectsEqual(next, rect)) {
        rect = next;
        changed = true;
        callSetRect(css, normaliseDpr(dpr), rect);
        const line = describeRect(rect, dpr);
        if (line !== lastDescription) {
          lastDescription = line;
          log(line);
        }
      }

      apply();
      return { rect, visible, changed };
    },

    /**
     * block hides the overlay for a named reason and keeps it hidden until that
     * same reason is released. Idempotent.
     * @param {string} reason
     */
    block(reason) {
      blockers.add(String(reason));
      apply();
    },

    /**
     * unblock releases one reason. The overlay comes back only when the last one
     * has gone.
     * @param {string} reason
     */
    unblock(reason) {
      blockers.delete(String(reason));
      apply();
    },

    /**
     * setWanted records whether there is an SRT picture to paint at all — the
     * picture control's selection AND the receiver actually delivering. False
     * puts the mosaic on screen underneath, which is the point of reserving the
     * same box for both.
     * @param {boolean} on
     */
    setWanted(on) {
      wanted = on === true;
      apply();
    },

    /** The reasons currently holding it hidden, for diagnostics and tests. */
    get blockers() {
      return [...blockers].sort();
    },
    get visible() {
      return visible;
    },
    get wanted() {
      return wanted;
    },
    get rect() {
      return rect ? { ...rect } : null;
    },
  };
}
