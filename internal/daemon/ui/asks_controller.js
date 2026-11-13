import {createAsksView} from "./asks_view.js";

const fail = () => { throw new TypeError("could not configure Asks controller"); };
function field(value, key) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail();
  const d = Object.getOwnPropertyDescriptor(value, key);
  if (!d || !Object.hasOwn(d, "value")) fail();
  return d.value;
}
const copyFailure = () => new Error("Could not copy quote");

// Unmounted, manual-loading host. Trusted readers complete ALL physical body work
// before settling an inert own-data {data: DTO} envelope. The view alone admits DTOs.
// A terminal adapter throw/then failure must leave no unreported background work.
export function createAsksController(document, options) {
  const window = field(options, "window"), idPrefix = field(options, "idPrefix");
  let readAsks = field(options, "readAsks"), copyText = field(options, "copyText"),
    announce = field(options, "announce"), onConversation = field(options, "onConversation");
  if (!window || window !== document.defaultView || typeof idPrefix !== "string" ||
      !/^[A-Za-z][A-Za-z0-9_-]*$/.test(idPrefix) ||
      ![readAsks, copyText, announce, onConversation].every(f => typeof f === "function")) fail();
  const limits = {}, supplied = field(options, "limits");
  for (const key of ["arrayEntries", "stringUnits", "rows"]) {
    const value = field(supplied, key);
    if (!Number.isSafeInteger(value) || value <= 0) fail();
    limits[key] = value;
  }
  options = null;
  const fragment = document.createDocumentFragment();
  const root = node("section", "nav-asks-controller"), refreshButton = node("button", "nav-asks-refresh", "Refresh Asks"),
    status = node("p", "nav-asks-load-status", "Choose a scope for Asks"), content = node("div", "nav-asks-content");
  refreshButton.type = "button";
  content.setAttribute("role", "region"); content.setAttribute("aria-label", "Asks results");
  content.setAttribute("aria-busy", "false");
  status.setAttribute("role", "status"); status.setAttribute("aria-live", "polite");
  root.append(refreshButton, status, content); fragment.append(root);
  let mounted = false, disposed = false, intent = null, retry = null, candidate = null,
    readFlight = null, copyFlight = null, depth = 0, queued = false;
  function node(tag, className, text) {
    const n = document.createElement(tag); n.className = className;
    if (text !== undefined) n.textContent = text;
    return n;
  }
  function current(op) { return mounted && !disposed && intent === op; }
  function eligible(c) { return current(c.op) && candidate === c && !c.retired && c.published && c.root.isConnected; }
  function activity(fn) {
    depth++;
    try { return fn(); } finally { depth--; pumpLater(); }
  }
  function pumpLater() {
    if (depth || queued || disposed || !mounted || readFlight || !intent?.waiting) return;
    queued = true;
    queueMicrotask(() => {
      queued = false;
      if (!depth && !disposed && mounted && !readFlight && intent?.waiting) activity(() => start(intent));
      else pumpLater();
    });
  }
  function report(op, message, busy) {
    if (!current(op)) return;
    content.setAttribute("aria-busy", busy ? "true" : "false");
    if (!current(op)) return;
    status.textContent = message;
    if (!current(op)) return;
    try { announce(message); } catch { /* A status sink cannot change load outcome. */ }
  }
  function cleanup(c) {
    if (!c || !c.retired || c.busy || c.cleaned) return;
    c.cleaned = true;
    try { c.child?.teardown(); } catch { /* Retired cleanup stays local. */ }
    c.child = null;
  }
  function abort(token) {
    const controller = token?.abort;
    if (token) token.abort = null;
    try { controller?.abort(); } catch { /* Abort never releases the physical slot. */ }
  }
  function retire(c, op, recoverFocus) {
    if (!c) return;
    const focused = document.activeElement, heldFocus = c.root.contains(focused);
    c.retired = true; c.published = false; c.root.remove();
    // Recover only a focus witness from this retired content, before callouts.
    if (recoverFocus && heldFocus && current(op) &&
        (document.activeElement === focused || document.activeElement === document.body)) {
      refreshButton.focus();
    }
    cleanup(c);
  }
  function reserve() {
    const op = {waiting:false, mode:"", dir:""}, old = candidate, flight = readFlight;
    intent = op; retry = null; candidate = null;
    retire(old, op, true); abort(flight);
    return op;
  }
  function loadScope(value) {
    if (!mounted || disposed) return false;
    return activity(() => {
      const op = reserve();
      if (!current(op)) return false;
      try {
        const mode = field(value, "mode"), dir = field(value, "dir");
        if (!(mode === "all" && dir === "" || mode === "directory" && typeof dir === "string" && dir !== "") ||
            dir.length > limits.stringUnits) fail();
        if (!current(op)) return false;
        op.mode = mode; op.dir = dir; op.waiting = true;
        retry = {mode, dir};
        report(op, "Loading Asks", true);
        return current(op);
      } catch {
        if (current(op)) report(op, "Asks unavailable", false);
        return false;
      }
    });
  }
  function refresh() {
    if (!mounted || disposed) return false;
    if (retry) return loadScope({mode:retry.mode, dir:retry.dir});
    return activity(() => {
      const op = reserve(); report(op, "Choose a scope for Asks", false); return current(op);
    });
  }
  function copy(c, text) {
    if (!eligible(c) || copyFlight) throw copyFailure();
    // The token retains neither quote nor candidate; it outlives retired children.
    const token = {}; copyFlight = token;
    const release = () => { if (copyFlight === token) copyFlight = null; };
    try {
      const result = copyText(text);
      if (result === undefined) { release(); return; }
      const then = result?.then;
      if (typeof then !== "function") throw copyFailure();
      return new Promise((resolve, reject) => {
        let settled = false;
        const done = ok => {
          if (settled) return;
          settled = true; release();
          if (ok) resolve(); else reject(copyFailure());
        };
        try { then.call(result, () => done(true), () => done(false)); } catch { done(false); }
      });
    } catch { release(); throw copyFailure(); }
  }
  function publish(op, envelope) {
    const data = field(envelope, "data");
    if (!current(op)) return;
    const c = {op, root:node("div", "nav-asks-candidate"), child:null, retired:false,
      published:false, busy:1, cleaned:false};
    c.root.hidden = true; candidate = c;
    try {
      content.append(c.root);
      if (!current(op) || candidate !== c) return;
      c.child = createAsksView(document, data, {window, idPrefix,
        expectedScope:{mode:op.mode, dir:op.dir}, limits:{...limits},
        copyText:text => copy(c, text),
        announce:message => { if (eligible(c)) announce(message); },
        onConversation:value => { if (eligible(c)) onConversation(value); }});
      if (!current(op) || candidate !== c) return;
      c.root.append(c.child.fragment);
      const ok = c.child.mount() === true;
      if (!current(op) || candidate !== c) return;
      if (!ok) throw new Error("Asks unavailable");
      report(op, "Asks updated", false);
      if (!current(op) || candidate !== c || c.retired) return;
      // No host callback between eligibility and whole-candidate publication.
      c.published = true; c.root.hidden = false;
    } finally {
      c.busy--;
      if (!current(op) || candidate !== c) { c.retired = true; c.published = false; c.root.remove(); }
      cleanup(c);
    }
  }
  function start(op) {
    if (!current(op) || !op.waiting || readFlight) return;
    op.waiting = false;
    const token = {abort:null, settled:false}; readFlight = token;
    const done = (ok, envelope) => activity(() => {
      if (token.settled) return;
      token.settled = true;
      try {
        if (current(op)) {
          try {
            if (!ok) throw new Error("Asks unavailable");
            publish(op, envelope);
          } catch {
            if (current(op)) {
              const failed = candidate; candidate = null;
              retire(failed, op, true); report(op, "Asks unavailable", false);
            }
          }
        }
      } finally {
        if (readFlight === token) readFlight = null;
        token.abort = null;
      }
    });
    try {
      token.abort = new window.AbortController();
      if (!current(op)) { abort(token); done(false); return; }
      const signal = token.abort.signal;
      if (!current(op)) { abort(token); done(false); return; }
      const result = readAsks({mode:op.mode, dir:op.dir}, signal);
      // Consume even a stale outer rejection. Never assimilate its DTO payload.
      const then = result?.then;
      if (typeof then !== "function") throw new Error("Asks unavailable");
      then.call(result, value => done(true, value), () => done(false));
    } catch { done(false); }
  }
  function mount() {
    if (disposed) return false;
    if (mounted) return true;
    if (!root.isConnected || root.ownerDocument !== document) return false;
    mounted = true; return true;
  }
  function teardown() {
    if (disposed) return;
    activity(() => {
      disposed = true; mounted = false; intent = null; retry = null;
      const old = candidate, flight = readFlight; candidate = null;
      retire(old, null, false); root.remove(); root.replaceChildren();
      refreshButton.removeEventListener("click", refresh);
      abort(flight);
      readAsks = null; copyText = null; announce = null; onConversation = null;
    });
  }
  refreshButton.addEventListener("click", refresh);
  return {fragment, mount, loadScope, refresh, teardown};
}
