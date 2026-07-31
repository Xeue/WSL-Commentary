/**
 * A minimal DOM for testing the mixer drawer under `node --test`.
 *
 * Owner: WP-M4. TEST SUPPORT ONLY. Nothing that ships imports this file — it is
 * unreachable from index.js, so no bundle contains it.
 *
 * ======================= WHY A SHIM AND NOT A LIBRARY =======================
 *
 * package.json is frozen, so there is no jsdom and no test framework. Without
 * something like this the armed/disarmed gate could only be tested through the
 * pure model, and the assertion that MATTERS — that clicking a crosspoint on
 * the rendered drawer while disarmed reaches nothing — would be untested.
 *
 * ======================= AND WHAT IT DOES NOT PROVE =========================
 *
 * This is a stand-in, not a browser. It implements EXACTLY the DOM surface
 * drawer.js uses and nothing else: no layout, no CSS, no event propagation, no
 * default actions. A test passing here proves the drawer's LOGIC and the
 * attributes it sets; it does not prove the drawer looks right or that focus
 * behaves as a real browser's would. demo.html is how that is checked.
 *
 * If drawer.js ever needs a DOM API not listed below, the honest fix is to add
 * it here deliberately — not to widen the shim until the test passes.
 *
 * Implemented surface:
 *   document.createElement, addEventListener, removeEventListener, activeElement
 *   element: appendChild, removeChild, firstChild, parentNode, childNodes,
 *            tagName, className, textContent, hidden, style.width,
 *            setAttribute, getAttribute, addEventListener, focus, ownerDocument
 */

class FakeNode {
  constructor(doc, tag) {
    this.ownerDocument = doc;
    this.tagName = String(tag).toUpperCase();
    this.childNodes = [];
    this.parentNode = null;
    this.className = '';
    this.hidden = false;
    this.style = { width: '' };
    this._attrs = new Map();
    this._listeners = new Map();
    this._text = '';
  }

  get firstChild() {
    return this.childNodes.length > 0 ? this.childNodes[0] : null;
  }

  appendChild(child) {
    if (child.parentNode) child.parentNode.removeChild(child);
    child.parentNode = this;
    this.childNodes.push(child);
    return child;
  }

  removeChild(child) {
    const i = this.childNodes.indexOf(child);
    if (i >= 0) this.childNodes.splice(i, 1);
    child.parentNode = null;
    return child;
  }

  setAttribute(name, value) {
    this._attrs.set(String(name), String(value));
  }

  getAttribute(name) {
    return this._attrs.has(String(name)) ? this._attrs.get(String(name)) : null;
  }

  removeAttribute(name) {
    this._attrs.delete(String(name));
  }

  addEventListener(type, fn) {
    const list = this._listeners.get(type) || [];
    list.push(fn);
    this._listeners.set(type, list);
  }

  removeEventListener(type, fn) {
    const list = this._listeners.get(type) || [];
    this._listeners.set(
      type,
      list.filter((f) => f !== fn),
    );
  }

  focus() {
    this.ownerDocument.activeElement = this;
  }

  /** textContent replaces children, as the real property does. */
  set textContent(v) {
    this.childNodes = [];
    this._text = v === undefined || v === null ? '' : String(v);
  }

  get textContent() {
    if (this.childNodes.length === 0) return this._text;
    return this.childNodes.map((c) => c.textContent).join('');
  }
}

class FakeDocument {
  constructor() {
    this.activeElement = null;
    this._listeners = new Map();
  }

  createElement(tag) {
    return new FakeNode(this, tag);
  }

  addEventListener(type, fn) {
    const list = this._listeners.get(type) || [];
    list.push(fn);
    this._listeners.set(type, list);
  }

  removeEventListener(type, fn) {
    const list = this._listeners.get(type) || [];
    this._listeners.set(
      type,
      list.filter((f) => f !== fn),
    );
  }
}

/**
 * createTestDom returns a document and a detached mount element to hand to
 * createMixerDrawer.
 *
 * @returns {{doc: FakeDocument, mount: FakeNode}}
 */
export function createTestDom() {
  const doc = new FakeDocument();
  const mount = doc.createElement('div');
  return { doc, mount };
}

/**
 * fire invokes the listeners registered on a node for one event type. There is
 * no bubbling: the drawer registers directly on the elements it created and on
 * the document, so a test fires at whichever of those it means.
 *
 * @param {FakeNode|FakeDocument} node
 * @param {string} type
 * @param {Object} [event]
 * @returns {Object} the event object, after any preventDefault
 */
export function fire(node, type, event = {}) {
  const ev = {
    preventDefault() {
      ev.defaultPrevented = true;
    },
    stopPropagation() {
      ev.propagationStopped = true;
    },
    defaultPrevented: false,
    propagationStopped: false,
    ...event,
  };
  const list = node._listeners.get(type) || [];
  for (const fn of [...list]) fn(ev);
  return ev;
}

/**
 * query walks the tree for the first node matching a predicate.
 *
 * @param {FakeNode} root
 * @param {(node: FakeNode) => boolean} pred
 * @returns {FakeNode|null}
 */
export function query(root, pred) {
  if (pred(root)) return root;
  for (const child of root.childNodes) {
    const found = query(child, pred);
    if (found) return found;
  }
  return null;
}

/**
 * queryAll walks the tree for every node matching a predicate, in document
 * order.
 *
 * @param {FakeNode} root
 * @param {(node: FakeNode) => boolean} pred
 * @returns {FakeNode[]}
 */
export function queryAll(root, pred) {
  const out = [];
  const walk = (node) => {
    if (pred(node)) out.push(node);
    for (const child of node.childNodes) walk(child);
  };
  walk(root);
  return out;
}

/**
 * crosspoint finds the crosspoint button for a strip and bus.
 *
 * @param {FakeNode} root
 * @param {string} strip strip wire name
 * @param {string} bus bus wire name
 * @returns {FakeNode|null}
 */
export function crosspoint(root, strip, bus) {
  return query(root, (n) => n.getAttribute && n.getAttribute('data-strip') === strip && n.getAttribute('data-bus') === bus);
}

/**
 * buttonWithText finds a <button> whose rendered text contains a substring.
 * Used to reach the drawer's controls the way an operator does — by what the
 * control says — rather than by an internal handle the drawer would have to
 * expose for the tests' benefit.
 *
 * @param {FakeNode} root
 * @param {string} substring
 * @returns {FakeNode|null}
 */
export function buttonWithText(root, substring) {
  return query(root, (n) => n.tagName === 'BUTTON' && n.textContent.includes(substring));
}

/** text returns the whole rendered text of a subtree. */
export function text(root) {
  return root.textContent;
}
