/**
 * measure-home — the ACCEPTANCE MEASUREMENT for the two-column main screen.
 *
 * Owner: WP-5b.
 *
 * ============================ WHY THIS EXISTS ==============================
 *
 * The governing rule of the main screen, in the operator's words: "For the
 * comentators watching a live match, anything casuing their video to move is a
 * massive no." src/ui/homelayout.test.js pins every declaration and every append
 * that the property depends on, and that is the guard that runs everywhere — but
 * it is an argument about source, not a measurement. package.json is frozen:
 * there is no jsdom in this repo and no layout engine inside `node --test`.
 *
 * So this drives a REAL browser over the real stylesheet and reports .pgm-tile's
 * measured rectangle under each condition the acceptance asks about: the column
 * empty, with one alert, with ten alerts, with a note, and with the tray being
 * read. Every one of those numbers must be identical. It also reports the one
 * case that is ALLOWED to differ — the operator collapsing the column — so that
 * the difference is visible rather than buried.
 *
 * It is a TOOL and not a test, deliberately: it needs a Chromium on the machine,
 * and a suite that silently skips when one is missing is a suite that reports a
 * pass for a measurement nobody took.
 *
 * ============================== HOW TO RUN =================================
 *
 *   cd frontend && node tools/measure-home.mjs
 *   cd frontend && node tools/measure-home.mjs --width 1024 --height 820
 *
 * It starts a static server on a loopback port (ES modules cannot be loaded over
 * file://), runs headless Chromium once, prints a table and a verdict, and exits
 * non-zero if any of the "must be identical" rectangles differ. It kills the
 * browser and closes the server on every path out, including a failure.
 */

import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, normalize, extname } from 'node:path';
import { tmpdir } from 'node:os';

const here = dirname(fileURLToPath(import.meta.url));
const frontend = join(here, '..');

// Chromium, wherever this machine keeps it. Windows first because that is where
// this application ships; the others are for the porting machines.
const BROWSERS = [
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
];

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
};

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback;
}

function findBrowser() {
  for (const b of BROWSERS) if (existsSync(b)) return b;
  return null;
}

async function serve() {
  const server = createServer(async (req, res) => {
    // Path traversal is not a security question here — this serves a source tree
    // to a browser on loopback — but a request that escapes the tree would read
    // a file that is not under test, which makes the measurement a lie.
    const rel = normalize(decodeURIComponent(req.url.split('?')[0])).replace(/^(\.\.[/\\])+/, '');
    const file = join(frontend, rel);
    if (!file.startsWith(frontend)) {
      res.writeHead(403).end();
      return;
    }
    try {
      const body = await readFile(file);
      res.writeHead(200, { 'content-type': TYPES[extname(file)] || 'application/octet-stream' });
      res.end(body);
    } catch {
      res.writeHead(404).end();
    }
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  return { server, port: server.address().port };
}

/**
 * dumpDom runs the browser once and returns the rendered DOM.
 *
 * --dump-dom prints the document AFTER the module has run, which is why the page
 * writes its results into an element rather than logging them: there is no
 * console to read without a debugging protocol, and a protocol client would be a
 * dependency this repo does not have.
 */
function dumpDom(browser, url, width, height) {
  const profile = mkdtempSync(join(tmpdir(), 'measure-home-'));
  const args = [
    '--headless=new',
    '--disable-gpu',
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-extensions',
    // A measurement must not be sharing a machine with an updater, a sync client
    // or a component download. None of them changes a rectangle; all of them
    // change how long this takes and how much noise it prints.
    '--disable-background-networking',
    '--disable-component-update',
    '--disable-sync',
    `--user-data-dir=${profile}`,
    `--window-size=${width},${height}`,
    '--virtual-time-budget=4000',
    '--dump-dom',
    url,
  ];
  return new Promise((resolve, reject) => {
    const child = spawn(browser, args, { stdio: ['ignore', 'pipe', 'pipe'] });
    let out = '';
    let err = '';
    let done = false;

    // RESOLVE ON THE DOM, NOT ON THE EXIT. --dump-dom prints the document and
    // then the browser takes its own time going away — a Chrome install with an
    // updater attached can sit for a minute after it has answered. Waiting for
    // `close` measured that instead of the layout. The child is killed the
    // moment the answer is complete, which is also what keeps this tool from
    // leaving a browser behind on a machine whose capture card is exclusive.
    const finish = (fn, arg) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      child.kill('SIGKILL');
      rmSync(profile, { recursive: true, force: true });
      fn(arg);
    };
    const timer = setTimeout(
      () => finish(reject, new Error(`the browser did not print a DOM within 60 s\n${err}`)),
      60_000,
    );

    child.stdout.on('data', (d) => {
      out += d;
      if (out.includes('</html>')) finish(resolve, out);
    });
    child.stderr.on('data', (d) => (err += d));
    child.on('error', (e) => finish(reject, e));
    child.on('close', (code) => {
      if (out.includes('</html>')) finish(resolve, out);
      else finish(reject, new Error(`the browser produced no DOM (exit ${code})\n${err}`));
    });
  });
}

function extractResults(dom) {
  const open = dom.indexOf('<pre id="measure-out"');
  if (open < 0) throw new Error('the harness page did not render its output element');
  const start = dom.indexOf('>', open) + 1;
  const end = dom.indexOf('</pre>', start);
  const text = dom
    .slice(start, end)
    .replace(/&quot;/g, '"')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&amp;/g, '&');
  if (!text.trim()) {
    throw new Error('the harness ran but measured nothing — a module probably failed to load');
  }
  return JSON.parse(text);
}

const same = (a, b) => a.x === b.x && a.y === b.y && a.width === b.width && a.height === b.height;
const fmt = (r) => `x=${r.x} y=${r.y} w=${r.width} h=${r.height}`;

async function main() {
  const browser = findBrowser();
  if (!browser) {
    console.error('measure-home: no Chromium-family browser found. Looked in:');
    for (const b of BROWSERS) console.error(`  ${b}`);
    process.exit(2);
  }

  const width = Number(arg('width', 1600));
  const height = Number(arg('height', 900));
  const { server, port } = await serve();
  let results;
  try {
    const dom = await dumpDom(
      browser,
      `http://127.0.0.1:${port}/tools/measure-home.html`,
      width,
      height,
    );
    results = extractResults(dom);
  } finally {
    server.close();
  }

  console.log(`measure-home — ${browser}`);
  console.log(`window ${width}x${height}, viewport ${results.viewport.w}x${results.viewport.h}`);
  console.log('');
  console.log('.pgm-tile — the box the native SRT overlay is told to occupy');
  console.log('-'.repeat(96));

  const base = results.scenarios[0].rect;
  let identical = true;
  for (const s of results.scenarios) {
    const ok = same(s.rect, base);
    if (!ok) identical = false;
    console.log(`  ${ok ? 'same' : 'MOVED'}  ${s.name.padEnd(44)} ${fmt(s.rect)}`);
    if (s.note) console.log(`         ${' '.repeat(44)} ${s.note}`);
  }

  console.log('');
  console.log('the operator\'s own hand — allowed to move it, and only this');
  console.log('-'.repeat(96));
  for (const a of results.operatorActions) {
    console.log(`  ${same(a.rect, base) ? 'same ' : 'moved'}  ${a.name.padEnd(44)} ${fmt(a.rect)}`);
  }

  const reexpanded = results.operatorActions.find((a) => a.name === 're-expanded');
  console.log('');
  if (!identical) {
    console.error('FAIL: the picture moved. Something in the column reached the main area.');
    process.exit(1);
  }
  if (reexpanded && !same(reexpanded.rect, base)) {
    console.error('FAIL: re-expanding the column did not restore the picture to where it was.');
    process.exit(1);
  }
  console.log(
    `PASS: ${results.scenarios.length} conditions, one identical rectangle — ${fmt(base)}`,
  );
}

main().catch((err) => {
  console.error(`measure-home: ${err.message}`);
  process.exit(2);
});
