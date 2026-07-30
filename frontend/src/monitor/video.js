/**
 * R3 — the programme picture: CSS-cropping the PGM tile out of the multiviewer
 * mosaic.
 *
 * Owner: WP-5a. geometry.js holds the arithmetic; this module applies it to the
 * DOM and owns the wrapper elements.
 *
 * DOM contract with WP-5b
 * -----------------------
 * WP-5b supplies one <video> element in `opts.videoEl` and gives it a parent to
 * live in. This module inserts two wrapper <div>s around that <video>:
 *
 *   host          ← WP-5b's element, untouched
 *     stage       ← ours, data-monitor-stage; occupies tile.w*k x tile.h*k
 *       crop      ← ours, data-monitor-crop; tile.w x tile.h, overflow hidden,
 *                   transform: scale(k); transform-origin: 0 0
 *         video   ← WP-5b's element, restyled to the mosaic's natural size and
 *                   offset by -tile.x / -tile.y
 *
 * The stage exists because `transform` does not affect layout: without it the
 * crop box would claim its unscaled 640x360 of page space no matter what k was,
 * and the controls below the picture would sit in the wrong place at every
 * window size but one.
 *
 * WP-5b drives the scale with setScale(k) or fitTo(w, h) — see monitor.js. The
 * monitor never installs its own resize listener: the UI owns layout, and two
 * things reacting to resize is how you get a picture that jitters.
 *
 * The wrappers are created once per createMonitor() and survive stop()/start(),
 * so reconnecting does not churn the DOM.
 *
 * OPTING OUT
 * ----------
 * Pass `manageVideoLayout: false` to createMonitor and this module touches no
 * layout at all: no wrappers, no inline styles, nothing but srcObject. Use that
 * if the page already crops the mosaic itself — which WP-5b's `.pgm-tile` CSS
 * currently does, with a percentage-based crop that is responsive without any
 * scale factor. Two crops applied to the same <video> is one crop too many, and
 * this flag is how the coordinator picks which one wins without either package
 * editing the other's files.
 *
 * With the flag off, setScale/fitTo still return a clamped number and
 * getLayout still reports the tile, so nothing WP-5b calls throws — they simply
 * have no visual effect.
 */

import { cropStyles, layoutSize, normaliseTile, clampScale, fitScale, MOSAIC_WIDTH, MOSAIC_HEIGHT } from './geometry.js';

const STAGE_ATTR = 'data-monitor-stage';
const CROP_ATTR = 'data-monitor-crop';

/**
 * applyStyles writes a plain style object onto an element.
 * @param {HTMLElement} el
 * @param {Record<string, string>} styles
 */
function applyStyles(el, styles) {
  for (const [k, v] of Object.entries(styles)) el.style[k] = v;
}

/**
 * createMosaicView wraps a <video> element in the crop structure and returns a
 * handle for driving it.
 *
 * @param {object} args
 * @param {HTMLVideoElement} args.videoEl
 * @param {{x: number, y: number, w: number, h: number}} args.tile
 * @param {(err: Error) => void} [args.onError]
 * @param {boolean} [args.manageLayout] default true; false leaves all layout to the page
 * @returns {{
 *   attach: (stream: MediaStream) => void,
 *   detach: () => void,
 *   setTile: (tile: object) => void,
 *   setScale: (k: number) => number,
 *   fitTo: (w: number, h?: number) => number,
 *   getLayout: () => {tile: object, scale: number, width: number, height: number, mosaic: {width: number, height: number}, managed: boolean},
 *   destroy: () => void,
 * }}
 */
export function createMosaicView({ videoEl, tile, onError, manageLayout = true }) {
  let currentTile = normaliseTile(tile);
  let scale = 1;
  // Until the first frame arrives we assume the measured mosaic size
  // (docs/test-results.md §2.3). The moment metadata is available we use the
  // real intrinsic size instead, so a Sony change to the mosaic resolution
  // degrades to "the tile is in the wrong place" rather than "the tile is in the
  // wrong place AND stretched".
  let mosaic = { width: MOSAIC_WIDTH, height: MOSAIC_HEIGHT };
  let destroyed = false;

  const host = videoEl.parentElement;
  const doc = videoEl.ownerDocument || document;

  // Build (or adopt) the wrappers. Adoption matters if WP-5b ever re-runs the
  // monitor over the same element: creating a second stage would nest the crop
  // inside itself and halve the picture.
  let crop = null;
  let stage = null;
  if (manageLayout) {
    crop = videoEl.parentElement;
    if (crop && crop.hasAttribute && crop.hasAttribute(CROP_ATTR)) {
      stage = crop.parentElement;
    } else {
      stage = doc.createElement('div');
      stage.setAttribute(STAGE_ATTR, '');
      crop = doc.createElement('div');
      crop.setAttribute(CROP_ATTR, '');
      if (host) host.insertBefore(stage, videoEl);
      stage.appendChild(crop);
      crop.appendChild(videoEl);
    }
  }

  // The mosaic is a monitor feed; the element must never try to autoplay with
  // sound (that is what the <audio> element and Web Audio are for) and must
  // never go fullscreen-inline on its own.
  videoEl.muted = true;
  videoEl.defaultMuted = true;
  videoEl.autoplay = true;
  videoEl.playsInline = true;
  videoEl.controls = false;
  videoEl.disablePictureInPicture = true;

  function onLoadedMetadata() {
    if (destroyed) return;
    const w = videoEl.videoWidth;
    const h = videoEl.videoHeight;
    if (w > 0 && h > 0 && (w !== mosaic.width || h !== mosaic.height)) {
      mosaic = { width: w, height: h };
      // Re-normalise: a smaller-than-expected mosaic can put the configured tile
      // partly or wholly outside the picture, and normaliseTile clamps it back
      // in rather than showing a black rectangle.
      currentTile = normaliseTile(currentTile, mosaic);
      render();
    }
  }
  videoEl.addEventListener('loadedmetadata', onLoadedMetadata);

  function render() {
    if (!manageLayout) return;
    const s = cropStyles(currentTile, scale, mosaic);
    applyStyles(stage, s.stage);
    applyStyles(crop, s.crop);
    applyStyles(videoEl, s.video);
  }

  render();

  return {
    /**
     * attach points the <video> at a stream and starts it.
     *
     * The play() promise is handled explicitly: a muted video is exempt from
     * every autoplay policy Chromium has, so a rejection here means something
     * genuinely wrong (the element was removed from the document, the track
     * ended), and swallowing it would leave a black rectangle with no
     * explanation.
     *
     * @param {MediaStream} stream
     */
    attach(stream) {
      if (destroyed) return;
      videoEl.srcObject = stream;
      const p = videoEl.play();
      if (p && typeof p.catch === 'function') {
        p.catch((err) => {
          // AbortError is normal: it means a newer play()/load() superseded this
          // one, which happens on a fast reconnect.
          if (err && err.name === 'AbortError') return;
          if (onError) onError(err);
        });
      }
    },

    /** detach clears the element. Called by stop() and before a reconnect. */
    detach() {
      if (destroyed) return;
      try {
        videoEl.pause();
      } catch {
        /* not playing */
      }
      videoEl.srcObject = null;
    },

    /**
     * setTile changes the cropped region live, e.g. when the Settings view
     * saves a new monitorTile.
     * @param {object} t
     */
    setTile(t) {
      if (destroyed) return;
      currentTile = normaliseTile(t, mosaic);
      render();
    },

    /**
     * setScale sets the CSS scale factor and returns the value actually used
     * after clamping.
     * @param {number} k
     * @returns {number}
     */
    setScale(k) {
      if (destroyed) return scale;
      scale = clampScale(k);
      render();
      return scale;
    },

    /**
     * fitTo scales the tile into the given box and returns the scale used.
     * Pass only a width to fill the window width, per spec §10.
     * @param {number} w
     * @param {number} [h]
     * @returns {number}
     */
    fitTo(w, h) {
      if (destroyed) return scale;
      scale = fitScale(currentTile, w, h);
      render();
      return scale;
    },

    /** getLayout reports the geometry WP-5b needs to size its own container. */
    getLayout() {
      const size = layoutSize(currentTile, scale);
      return {
        tile: { ...currentTile },
        scale,
        width: size.width,
        height: size.height,
        mosaic: { ...mosaic },
        managed: manageLayout,
      };
    },

    /**
     * destroy removes the listener, clears the element and unwraps the DOM,
     * returning the <video> to WP-5b's element exactly where it was found.
     */
    destroy() {
      if (destroyed) return;
      destroyed = true;
      videoEl.removeEventListener('loadedmetadata', onLoadedMetadata);
      try {
        videoEl.pause();
      } catch {
        /* not playing */
      }
      videoEl.srcObject = null;
      if (!manageLayout) return;
      if (host && stage && stage.parentElement === host) {
        host.insertBefore(videoEl, stage);
        stage.remove();
      }
      // Only styles this module wrote are removed, and it wrote all of them.
      videoEl.removeAttribute('style');
    },
  };
}
