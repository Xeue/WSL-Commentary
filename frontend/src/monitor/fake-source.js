/**
 * A synthetic M2L-X: a 2240x1440 multiviewer mosaic and seven distinguishable
 * audio buses, generated locally.
 *
 * Owner: WP-5a. Used only by harness.html. Nothing in the shipped app imports
 * it.
 *
 * The point of this file is that R3 and R4 can be verified with no Go, no
 * GStreamer, no AWS and no M2L-X instance:
 *
 *   - the mosaic is drawn to the measured 2240x1440 with the measured tile
 *     rectangles labelled and a moving element in each, so a wrong crop is
 *     obvious at a glance rather than plausible;
 *   - each of the seven audio buses is a different musical pitch, so switching
 *     the return with setReturnMid is audible and unambiguous. If you select
 *     CLN and hear the PGM pitch, the mid map is wrong.
 *
 * The tile rectangles below are the ones measured on the dev event
 * (docs/test-results.md §2.3). They are drawn here purely so the harness looks
 * like the real thing; geometry.js is where they matter.
 */

/** MOSAIC_W / MOSAIC_H are the measured intrinsic size of the real mosaic. */
export const MOSAIC_W = 2240;
export const MOSAIC_H = 1440;

/**
 * TILES are the measured source rects. PVW and PGM are 640x360; the source
 * thumbnails sit on a 320x180 grid at x = 640/960/1280/1600/1920 and
 * y = 0/181/362/543/723/904.
 */
const TILES = [
  { x: 0, y: 0, w: 640, h: 360, label: 'PVW' },
  { x: 0, y: 360, w: 640, h: 360, label: 'PGM' },
];
for (const [col, x] of [640, 960, 1280, 1600, 1920].entries()) {
  for (const [row, y] of [0, 181, 362, 543, 723, 904].entries()) {
    TILES.push({ x, y, w: 320, h: 180, label: `SRC ${col * 6 + row + 1}` });
  }
}

/**
 * BUS_TONES are the seven audio buses' pitches, one per mid 1..7. They are far
 * enough apart to be told apart by ear without training; 440 Hz (concert A) is
 * mid 1 / master, and mid 2 / aux1 — the bus the commentator actually monitors —
 * is 554.37 Hz, a major third above it.
 */
export const BUS_TONES = Object.freeze([
  { mid: 1, hz: 440.0, name: 'master / PGM' },
  { mid: 2, hz: 554.37, name: 'aux1 / CLN' },
  { mid: 3, hz: 659.26, name: 'aux2' },
  { mid: 4, hz: 783.99, name: 'mon1' },
  { mid: 5, hz: 880.0, name: 'mon2' },
  { mid: 6, hz: 987.77, name: 'mon3' },
  { mid: 7, hz: 1108.73, name: 'mon4 / PFL' },
]);

/**
 * createFakeMosaic draws an animated 2240x1440 multiviewer to an offscreen
 * canvas and returns its captured video track.
 *
 * 25 fps rather than 50: this is a harness, and halving the canvas work leaves
 * the machine free to do the thing being tested.
 *
 * @returns {{track: MediaStreamTrack, canvas: HTMLCanvasElement, stop: () => void}}
 */
export function createFakeMosaic() {
  const canvas = document.createElement('canvas');
  canvas.width = MOSAIC_W;
  canvas.height = MOSAIC_H;
  const ctx = canvas.getContext('2d');

  let raf = 0;
  let stopped = false;
  const started = performance.now();

  function draw() {
    if (stopped) return;
    const t = (performance.now() - started) / 1000;

    ctx.fillStyle = '#111';
    ctx.fillRect(0, 0, MOSAIC_W, MOSAIC_H);

    for (const tile of TILES) {
      const isPgm = tile.label === 'PGM';
      ctx.fillStyle = isPgm ? '#0b3d2e' : '#1b1b2a';
      ctx.fillRect(tile.x, tile.y, tile.w, tile.h);

      ctx.strokeStyle = isPgm ? '#4ade80' : '#3f3f5a';
      ctx.lineWidth = 4;
      ctx.strokeRect(tile.x + 2, tile.y + 2, tile.w - 4, tile.h - 4);

      ctx.fillStyle = isPgm ? '#e7ffe9' : '#8f8fae';
      ctx.font = `${Math.round(tile.h / 8)}px system-ui, sans-serif`;
      ctx.textBaseline = 'top';
      ctx.fillText(tile.label, tile.x + 12, tile.y + 12);

      // A sweeping bar per tile. If the crop is off by a tile the bar is in the
      // wrong place, and if the crop is off by a few pixels the tile border
      // gives it away.
      const phase = (t * 0.5 + (tile.x + tile.y) / 3000) % 1;
      ctx.fillStyle = isPgm ? '#22c55e' : '#3b3b5c';
      ctx.fillRect(tile.x + 8, tile.y + tile.h - 28, (tile.w - 16) * phase, 16);
    }

    // A clock across the PGM tile, so a frozen picture is obvious.
    const pgm = TILES[1];
    ctx.fillStyle = '#ffffff';
    ctx.font = '96px ui-monospace, monospace';
    ctx.fillText(t.toFixed(1).padStart(7, ' '), pgm.x + 150, pgm.y + 140);

    raf = requestAnimationFrame(draw);
  }
  draw();

  const stream = canvas.captureStream(25);
  const track = stream.getVideoTracks()[0];

  return {
    track,
    canvas,
    stop() {
      stopped = true;
      if (raf) cancelAnimationFrame(raf);
      for (const tr of stream.getTracks()) tr.stop();
    },
  };
}

/**
 * createFakeBuses builds seven audio tracks, one per bus, each a steady tone at
 * its own pitch.
 *
 * The tones are at -20 dBFS rather than full scale. Two reasons: it is roughly
 * where the real buses sit before the 18 dB make-up gain, so the harness
 * exercises the gain law in the region that matters; and a full-scale sine into
 * headphones at 18 dB of make-up gain would be genuinely painful.
 *
 * @returns {{tracks: MediaStreamTrack[], context: AudioContext, stop: () => Promise<void>}}
 *          tracks[i] is the bus for mid i+1
 */
export function createFakeBuses() {
  const Ctor = typeof AudioContext !== 'undefined' ? AudioContext : window.webkitAudioContext;
  const context = new Ctor();
  const oscillators = [];
  const tracks = [];

  for (const bus of BUS_TONES) {
    const osc = context.createOscillator();
    osc.type = 'sine';
    osc.frequency.value = bus.hz;
    const g = context.createGain();
    g.gain.value = 0.1; // -20 dBFS
    const dest = context.createMediaStreamDestination();
    osc.connect(g);
    g.connect(dest);
    osc.start();
    oscillators.push(osc);
    tracks.push(dest.stream.getAudioTracks()[0]);
  }

  return {
    tracks,
    context,
    async stop() {
      for (const o of oscillators) {
        try {
          o.stop();
        } catch {
          /* already stopped */
        }
      }
      for (const t of tracks) t.stop();
      try {
        await context.close();
      } catch {
        /* already closed */
      }
    },
  };
}
