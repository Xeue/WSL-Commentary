/*
 * shim.js — the entire frontend side of the remote bridge.
 *
 * WHY THIS EXISTS, AND WHY IT IS A CLASSIC SCRIPT
 *
 * frontend/src/ui/backend.js reaches Go by string-named binding
 * (window.go.main.App[method]) and by window.runtime.EventsOn, and computes
 * `export const usingFakeBackend = !hasWails()` at MODULE SCOPE. index.html
 * loads the app bundle as `<script type="module">`, which is deferred. A
 * classic (non-module) <script>, injected immediately before that module tag by
 * assets.go, therefore runs FIRST — so by the time backend.js evaluates,
 * window.go and window.runtime already exist and point at this WebSocket
 * transport. The result is that the whole existing frontend takes the real
 * backend path with ZERO changes to backend.js.
 *
 * WHY IT PRUNES window.go.main.App ON HELLO
 *
 * The server's hello frame carries the authoritative `methods` list for this
 * client. We install exactly those and no others. Host-only bindings (the SRT
 * picture geometry, the SRT return) are simply absent, so on a remote client
 * they do not EXIST to be called: backend.js's hasBinding() goes false,
 * pictureAvailable() returns false, and returnpath.js treats a missing
 * StopReturn as the benign case. Degradation is by omission, which is a
 * condition this frontend already handles, not by a server refusal it would
 * have to be taught to expect.
 *
 * WHY THERE IS NO LOGIN
 *
 * The listener is UNAUTHENTICATED by the owner's explicit decision — it runs on
 * a dedicated private facility network, and the network is the access control
 * (see docs/remote-access.md). So this shim connects straight to the WebSocket
 * with no login step, no cookie and no credentials, and derives ws:// vs wss://
 * from the page's own protocol so it works identically over HTTP and HTTPS.
 *
 * WHY IT TOLERATES A MISSING DOM
 *
 * Every document/overlay access is guarded. The transport core — queue a call
 * before open, resolve it after, deliver events, prune on hello — runs with
 * nothing but a WebSocket, a location and a window. That is what lets a headless
 * node:test drive it against a fake WebSocket without a browser.
 */
(function () {
  'use strict';

  var WS_PATH = '/__wslremote/ws';

  var win = typeof window !== 'undefined' ? window : (typeof globalThis !== 'undefined' ? globalThis : this);
  // hasDoc gates every overlay/DOM access. When false — a headless test harness
  // driving the transport against a fake WebSocket — the shim still queues
  // calls, resolves results and delivers events; only the login/reconnect UI is
  // skipped. document.body may still be null while <head> is parsed, so the
  // element-creating paths re-check it at call time via docReady().
  var hasDoc = typeof document !== 'undefined' && document != null;

  function docReady() {
    return hasDoc && document.body != null;
  }

  // --- transport state -----------------------------------------------------

  var nextId = 1; // client-chosen call ids; the server only ever echoes them
  var pending = Object.create(null); // id -> {resolve, reject}
  var listeners = Object.create(null); // event name -> [cb, ...]
  var preOpenQueue = []; // serialized call frames made before the socket opened
  var socket = null;
  var opened = false; // the socket reached OPEN at least once this attempt
  var everConnected = false; // a hello has been received at least once
  var lastSeq = 0; // highest event seq seen, to detect a dropped event
  var reconnectDelay = 1000; // ms, backs off to reconnectMax
  var reconnectMax = 15000;

  // window.__wslcommsRemote is how the frontend (app.js) knows it is a remote
  // client rather than the local WebView2 — see isRemoteClient(). It exists
  // from the first line so a check that runs before hello still reads "remote".
  // `client` is filled in on hello with this connection's id, which the frontend
  // uses to recognise the echo of its own SaveConfig.
  win.__wslcommsRemote = { client: null };

  // --- window.go.main.App --------------------------------------------------
  //
  // Before hello we do not yet know the method list, so App is a Proxy whose
  // get-trap hands back a queuing call function for ANY method name: a call
  // made this early is serialized and flushed when the socket opens. On hello
  // we REPLACE window.go.main.App with a plain object exposing only the
  // server's authoritative methods. backend.js reads window.go.main.App[method]
  // fresh on every call (callGo does `const fn = window.go.main.App[method]`),
  // so the replacement takes effect for every subsequent call, and a pruned
  // (host-only) method simply reads back as undefined.

  function makeCallFn(method) {
    return function () {
      var args = Array.prototype.slice.call(arguments);
      return sendCall(method, args);
    };
  }

  var preHelloApp = new Proxy(
    {},
    {
      get: function (target, prop) {
        if (typeof prop !== 'string') return target[prop];
        if (!target[prop]) target[prop] = makeCallFn(prop);
        return target[prop];
      },
      has: function () {
        return true;
      },
    }
  );

  win.go = { main: { App: preHelloApp } };

  function installMethods(methods) {
    var app = {};
    for (var i = 0; i < methods.length; i++) {
      app[methods[i]] = makeCallFn(methods[i]);
    }
    win.go.main.App = app;
  }

  // --- window.runtime.Events* ---------------------------------------------
  //
  // EventsOn returns an unsubscribe function, which is the branch backend.js
  // prefers (it falls back to EventsOff only if no function is returned). Both
  // are provided so backend.js's subscribe() works unchanged.

  function eventsOn(name, cb) {
    (listeners[name] || (listeners[name] = [])).push(cb);
    return function () {
      var arr = listeners[name];
      if (!arr) return;
      var i = arr.indexOf(cb);
      if (i >= 0) arr.splice(i, 1);
    };
  }

  function eventsOff(name /*, ...cbs */) {
    // backend.js calls EventsOff(name) with just the name to drop every
    // handler for that event, which is what Wails' own EventsOff also does.
    delete listeners[name];
  }

  function emit(name, data) {
    var arr = listeners[name];
    if (!arr) return;
    // Copy before iterating: a handler is allowed to unsubscribe itself.
    var snapshot = arr.slice();
    for (var i = 0; i < snapshot.length; i++) {
      try {
        snapshot[i](data);
      } catch (e) {
        // A throwing subscriber must not stop the others or kill the socket.
        logError('event handler for ' + name, e);
      }
    }
  }

  win.runtime = {
    EventsOn: eventsOn,
    EventsOff: eventsOff,
    // EventsEmit is a no-op: this frontend never pushes events back to Go, and a
    // remote client certainly must not be able to forge one. Present only so a
    // stray call does not throw.
    EventsEmit: function () {},
  };

  // --- calls ---------------------------------------------------------------

  function sendCall(method, args) {
    var id = nextId++;
    var frame = JSON.stringify({ t: 'call', id: id, method: method, args: normalizeArgs(args) });
    return new Promise(function (resolve, reject) {
      pending[id] = { resolve: resolve, reject: reject };
      if (opened && socket && socket.readyState === 1) {
        socket.send(frame);
      } else {
        // Made before the socket opened (or during a reconnect): hold it and
        // flush in order once we are connected again.
        preOpenQueue.push(frame);
      }
    });
  }

  function normalizeArgs(args) {
    // The server decodes args as raw JSON values, one per Go parameter. undefined
    // is not valid JSON, so a trailing undefined (from a variadic call site) is
    // coerced to null, which unmarshals into a Go zero.
    var out = [];
    for (var i = 0; i < args.length; i++) {
      out.push(args[i] === undefined ? null : args[i]);
    }
    return out;
  }

  function flushPreOpen() {
    if (!socket || socket.readyState !== 1) return;
    var q = preOpenQueue;
    preOpenQueue = [];
    for (var i = 0; i < q.length; i++) {
      socket.send(q[i]);
    }
  }

  function rejectAllPending(reason) {
    var ids = Object.keys(pending);
    for (var i = 0; i < ids.length; i++) {
      var p = pending[ids[i]];
      delete pending[ids[i]];
      try {
        p.reject(new Error(reason));
      } catch (e) {
        /* ignore */
      }
    }
  }

  // --- socket lifecycle ----------------------------------------------------

  function wsURL() {
    var loc = win.location || {};
    var scheme = loc.protocol === 'http:' ? 'ws://' : 'wss://';
    return scheme + loc.host + WS_PATH;
  }

  function connect() {
    var WS = win.WebSocket || (typeof WebSocket !== 'undefined' ? WebSocket : null);
    if (!WS) {
      logError('WebSocket unavailable', null);
      return;
    }
    opened = false;
    try {
      socket = new WS(wsURL());
    } catch (e) {
      onCloseLike();
      return;
    }

    socket.onopen = function () {
      opened = true;
      reconnectDelay = 1000;
      // Do NOT flush pre-open calls yet: they must wait for hello so a call is
      // never sent to a method that is about to be pruned away. flushPreOpen is
      // called from the hello handler.
    };

    socket.onmessage = function (ev) {
      var msg;
      try {
        msg = JSON.parse(ev.data);
      } catch (e) {
        return;
      }
      handleFrame(msg);
    };

    socket.onerror = function () {
      // onerror is always followed by onclose; let onclose drive the state.
    };

    socket.onclose = function () {
      onCloseLike();
    };
  }

  function onCloseLike() {
    var wasConnected = everConnected && opened;
    opened = false;
    socket = null;
    // In-flight calls cannot be answered on a dead socket; reject them so the
    // frontend's promises settle instead of hanging forever.
    rejectAllPending('remote: connection lost');
    if (wasConnected) {
      showReconnecting();
    }
    // There is no login to offer — the listener is unauthenticated — so a close
    // before or after a hello is the same case: back off and reconnect.
    scheduleReconnect();
  }

  function scheduleReconnect() {
    var delay = reconnectDelay;
    reconnectDelay = Math.min(reconnectDelay * 2, reconnectMax);
    setTimeout(connect, delay);
  }

  function handleFrame(msg) {
    if (!msg || typeof msg !== 'object') return;
    switch (msg.t) {
      case 'hello':
        onHello(msg);
        break;
      case 'result':
        onResult(msg);
        break;
      case 'event':
        onEvent(msg);
        break;
      default:
        // Unknown frame types are ignored, not fatal: a newer server may add
        // one this shim predates, and dropping it is safer than disconnecting.
        break;
    }
  }

  function onHello(msg) {
    everConnected = true;
    win.__wslcommsRemote = { client: msg.client || null };
    installMethods(msg.methods || []);
    lastSeq = 0;
    hideOverlay();
    // Now that the authoritative method list is installed, release any calls
    // that were queued before the socket opened.
    flushPreOpen();
  }

  function onResult(msg) {
    var p = pending[msg.id];
    if (!p) return;
    delete pending[msg.id];
    if (msg.ok) {
      p.resolve(msg.value);
    } else {
      p.reject(new Error(msg.error || 'remote: call failed'));
    }
  }

  function onEvent(msg) {
    if (typeof msg.seq === 'number') {
      if (lastSeq !== 0 && msg.seq > lastSeq + 1) {
        // A gap means this client's bounded queue dropped an event under back
        // pressure. The events carried are edge-triggered current-value
        // displays, so the latest still arrives; we only note the loss.
        logInfo('remote: dropped ' + (msg.seq - lastSeq - 1) + ' event(s) before seq ' + msg.seq);
      }
      lastSeq = msg.seq;
    }
    emit(msg.name, msg.data);
  }

  // --- reconnect overlay (all DOM access guarded) --------------------------

  function showReconnecting() {
    if (!docReady()) return;
    var ov = ensureOverlay();
    ov.innerHTML = '';
    var box = el('div', 'wslremote-box');
    box.appendChild(el('h1', null, 'Reconnecting…'));
    box.appendChild(el('p', null, 'The connection to the commentary PC dropped. Retrying automatically.'));
    ov.appendChild(box);
    show(ov);
  }

  function ensureOverlay() {
    var ov = document.getElementById('wslremote-overlay');
    if (!ov) {
      ov = document.createElement('div');
      ov.id = 'wslremote-overlay';
      ov.setAttribute(
        'style',
        'position:fixed;inset:0;z-index:2147483647;display:none;align-items:center;' +
          'justify-content:center;background:rgba(10,12,16,0.92);color:#e6e8ec;' +
          'font-family:system-ui,sans-serif;'
      );
      document.body.appendChild(ov);
    }
    return ov;
  }

  function show(ov) {
    ov.style.display = 'flex';
  }

  function hideOverlay() {
    if (!hasDoc) return;
    var ov = document.getElementById('wslremote-overlay');
    if (ov) ov.style.display = 'none';
  }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    if (tag === 'h1') e.setAttribute('style', 'font-size:18px;margin:0 0 12px;');
    if (cls === 'wslremote-box')
      e.setAttribute('style', 'display:flex;flex-direction:column;min-width:280px;padding:24px;background:#161a20;border-radius:8px;');
    return e;
  }

  function logError(what, e) {
    if (typeof console !== 'undefined' && console.error) console.error('wslremote:', what, e || '');
  }
  function logInfo(m) {
    if (typeof console !== 'undefined' && console.info) console.info(m);
  }

  // --- go ------------------------------------------------------------------

  connect();

  // Exposed for a headless test harness: not used by the app, and namespaced so
  // it cannot collide with anything the frontend defines.
  win.__wslremoteShim = {
    connect: connect,
    sendCall: sendCall,
    handleFrame: handleFrame,
    _state: function () {
      return { opened: opened, everConnected: everConnected, pending: Object.keys(pending).length, queued: preOpenQueue.length };
    },
  };
})();
