import {captureSessions, captureDetail} from "./controller_data.js";
import {createConversationFeed} from "./feed.js";

const fail = () => { throw new TypeError("could not configure session controller"); };
const uuid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
function field(value, key) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail();
  const d = Object.getOwnPropertyDescriptor(value, key);
  if (!d || !Object.hasOwn(d, "value")) fail();
  return d.value;
}
function text(value) { if (typeof value !== "string") fail(); return value; }
function bool(value) { if (typeof value !== "boolean") fail(); return value; }
function integer(value, min, max) {
  if (!Number.isSafeInteger(value) || value < min || value > max) fail();
  return value;
}
function scope(value) {
  const provider = text(field(value, "provider")), id = text(field(value, "id"));
  if (!["claude", "codex", "agy"].includes(provider) || !uuid.test(id)) fail();
  return {provider, id};
}

// An unmounted host component: readers, query policy and limits are injected.
// Readers return a trusted promise/thenable of an inert {data: DTO} envelope.
export function createSessionController(document, options) {
  const {window, idPrefix} = options || {};
  let {readSessions, readConversation, copyText, announce} = options || {};
  if (!window || window !== document.defaultView || typeof idPrefix !== "string" ||
      !/^[A-Za-z][A-Za-z0-9_-]*$/.test(idPrefix) ||
      ![readSessions, readConversation, copyText, announce].every(f => typeof f === "function")) fail();
  const suppliedLimits = field(options, "limits"), query = field(options, "detailQuery");
  const limits = {};
  for (const key of ["arrayEntries", "stringUnits", "rows"]) {
    limits[key] = integer(field(suppliedLimits, key), 1, Number.MAX_SAFE_INTEGER);
  }
  const detailQuery = {last:integer(field(query, "last"), 1, 10000),
    markup:bool(field(query, "markup")), anchor:integer(field(query, "anchor"), 0, 9999999999999)};
  const fragment = document.createDocumentFragment();
  const root = node("div", "nav-controller"); fragment.append(root);
  const listPane = pane("Sessions", "sessions"), detailPane = pane("Conversation", "conversation");
  root.append(listPane.root, detailPane.root);
  let mounted = false, disposed = false, activeScope = null, listQuery = null, selection = null, listView = null;
  const current = {list:null, detail:null}, serial = {list:0n, detail:0n}, removes = [];
  function node(tag, className, value) {
    const n = document.createElement(tag); n.className = className;
    if (value !== undefined) n.textContent = value;
    return n;
  }
  function pane(label, suffix) {
    const section = node("section", "nav-controller-pane");
    const heading = node("h2", "nav-controller-heading", label);
    heading.id = `${idPrefix}-${suffix}`; heading.setAttribute("tabindex", "-1");
    section.setAttribute("aria-labelledby", heading.id);
    const refresh = node("button", "nav-controller-refresh", `Refresh ${suffix}`);
    refresh.type = "button";
    const status = node("p", "nav-controller-status", ""); status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    const content = node("div", "nav-controller-content");
    section.append(heading, refresh, status, content);
    return {root:section, heading, refresh, status, content};
  }
  function alive(op) {
    return mounted && !disposed && !op.retired && current[op.channel] === op && op.scope === activeScope;
  }
  function cancelFocus(op) {
    if (op?.intent) { op.intent.remove(); op.intent = null; }
  }
  function cleanCandidate(candidate) {
    if (!candidate) return;
    candidate.retired = true; candidate.root.remove();
    // A paused activation may install more resources after reentry returns.
    if (candidate.busy || candidate.cleaned) return;
    candidate.cleaned = true;
    try { candidate.child?.teardown(); } catch { /* Trusted cleanup failure stays local. */ }
    candidate.child = null;
  }
  function retireMany(...operations) {
    const old = operations.filter(Boolean).map(op => {
      op.retired = true;
      if (op.candidate) op.candidate.retired = true;
      const abort = op.abort; op.abort = null;
      return {op, abort};
    });
    // Detach BOTH channels before any cleanup or abort can synchronously reenter.
    for (const {op} of old) { op.candidate?.root.remove(); cancelFocus(op); }
    for (const {op, abort} of old) {
      cleanCandidate(op.candidate);
      try { abort?.abort(); } catch { /* Only captured old resources are retired. */ }
    }
  }
  function retire(op) { retireMany(op); }
  function reserve(channel) {
    const previous = current[channel];
    const op = {channel, version:++serial[channel], scope:activeScope, retired:false, abort:null,
      candidate:null, intent:null};
    current[channel] = op;
    return {op, previous};
  }
  function status(op, pane, value, busy) {
    if (!alive(op)) return;
    pane.status.textContent = value;
    pane.root.setAttribute("aria-busy", busy ? "true" : "false");
    try { announce(value); } catch { /* Fixed status sinks do not control requests. */ }
  }
  function clearList() {
    if (listView) {
      if (current.detail?.intent?.view === listView) cancelFocus(current.detail);
      if (selection) selection.row = null;
      for (const remove of listView.removes) remove();
      listView.root.remove(); listView = null;
    }
  }
  function markers() {
    for (const item of listView?.rows || []) {
      if (!item.button) continue;
      if (selection?.row === item) item.button.setAttribute("aria-current", "true");
      else item.button.removeAttribute("aria-current");
    }
  }
  function failed(op, target) {
    if (!alive(op)) return;
    cleanCandidate(op.candidate); cancelFocus(op);
    status(op, target, op.channel === "list" ? (listView ? "Sessions unavailable; showing previous results." : "Sessions unavailable.") :
      "Conversation unavailable.", false);
  }
  function read(op, reader, request, target, commit) {
    if (!alive(op)) return;
    let promise;
    try {
      op.abort = new window.AbortController();
      if (!alive(op)) { retire(op); return; }
      const signal = op.abort.signal;
      if (!alive(op)) { retire(op); return; }
      const result = reader(request, signal);
      // Read/invoke the trusted outer then exactly once. Never resolve a DTO.
      const then = result?.then;
      if (typeof then !== "function") throw new TypeError("reader result");
      promise = new Promise((resolve, reject) => Reflect.apply(then, result, [resolve, reject]));
    } catch { failed(op, target); return; }
    // Even synchronous reader/thenable reentry must consume the returned rejection.
    promise.then(envelope => {
      if (!alive(op)) return;
      try {
        const data = field(envelope, "data");
        const owned = op.channel === "list" ? captureSessions(data, op.query.canonicalDir, limits) : captureDetail(data, op.query, limits);
        if (op.channel === "detail" && owned.markup !== detailQuery.markup) throw new TypeError("response mode");
        if (alive(op)) commit(op, owned);
      } catch { failed(op, target); }
    }, () => failed(op, target));
  }
  function listCommit(op, data) {
    const view = {root:node("div", "nav-controller-list"), rows:[], removes:[]};
    try {
      view.root.append(node("p", "nav-controller-facts", `Directory: ${data.dir}`),
        node("p", "nav-controller-facts", `Scanned ${data.scanned} of ${data.of}; unreadable ${data.unreadable_count}; partial: ${data.partial ? "yes" : "no"}`));
      for (const rootPath of data.roots_searched) view.root.append(node("p", "nav-controller-facts", `Root searched: ${rootPath}`));
      const rows = node("ul", "nav-controller-rows"); view.root.append(rows);
      for (const value of data.sessions) {
        const li = node("li", "nav-controller-row"), item = {button:null}; view.rows.push(item); rows.append(li);
        const label = `${value.provider} · ${value.label} · ${value.id}`;
        if (value.selectable) {
          item.button = node("button", "nav-controller-select", label); item.button.type = "button"; li.append(item.button);
          const selectedScope = {provider:value.provider, id:value.id};
          const click = () => { if (mounted && !disposed && listView === view) select(selectedScope, {item, view}); };
          item.button.addEventListener("click", click);
          view.removes.push(() => item.button.removeEventListener("click", click));
        } else li.append(node("p", "nav-controller-unavailable", `${label} · Unavailable for selection`));
        li.append(node("p", "nav-controller-facts", `Title: ${value.title} · Directory: ${value.cwd}`),
          node("p", "nav-controller-facts", `Model: ${value.model} · Branch: ${value.branch} · Updated: ${value.updated}`),
          node("p", "nav-controller-facts", `Turns: ${value.turns} · Tools: ${value.tools} · Worker: ${value.worker ? "yes" : "no"} · Bypass: ${value.bypass ? "yes" : "no"}`));
      }
      if (!alive(op)) return;
      clearList(); if (!alive(op)) return;
      listView = view; listPane.content.append(view.root); markers();
      status(op, listPane, data.sessions.length ? "Sessions ready." : "No sessions.", false);
    } finally {
      if (listView !== view) { for (const remove of view.removes) remove(); view.root.remove(); }
    }
  }
  function timeText(value) {
    if (!value.known) return "Unknown";
    const date = new Date(value.ms);
    return Number.isFinite(date.getTime()) ? date.toISOString() : `${value.ms} ms since Unix epoch`;
  }
  function detailCommit(op, data) {
    const candidate = {root:node("div", "nav-controller-conversation"), child:null, busy:true, retired:false, cleaned:false};
    op.candidate = candidate;
    try {
      const s = data.session;
      candidate.root.append(node("p", "nav-controller-facts", `${s.provider} · ${s.id}`),
        node("p", "nav-controller-facts", `Title: ${s.title} · Directory: ${s.cwd}`),
        node("p", "nav-controller-facts", `Model: ${s.model} · Branch: ${s.branch}`),
        node("p", "nav-controller-facts", `Started: ${timeText(s.started)} · Updated: ${timeText(s.updated)}`),
        node("p", "nav-controller-facts", `Worker: ${s.worker ? "yes" : "no"} · Bypass: ${s.bypass ? "yes" : "no"}`),
        node("p", "nav-controller-facts", `Earlier hidden: ${data.earlier_hidden} · Later hidden: ${data.later_hidden} · Window start: ${data.window_start}`));
      candidate.child = createConversationFeed(document, data, {window,
        idPrefix:`${idPrefix}-feed-${op.version}`, expectedScope:op.query,
        copyText:raw => { if (alive(op) && !candidate.retired) return copyText(raw); },
        announce:value => { if (alive(op) && !candidate.retired) announce(value); }});
      candidate.root.append(candidate.child.fragment);
      if (!alive(op)) return;
      detailPane.content.append(candidate.root);
      if (!alive(op)) return;
      if (!candidate.child.mount()) throw new Error("feed activation");
    } finally {
      candidate.busy = false;
      if (!alive(op) || candidate.retired) cleanCandidate(candidate);
    }
    if (!alive(op)) return;
    status(op, detailPane, data.messages.length ? "Conversation ready." : "No messages in this window.", false);
    if (!alive(op)) return;
    const intent = op.intent;
    if (intent && !intent.revoked && listView === intent.view && intent.button.isConnected && document.activeElement === intent.button) {
      cancelFocus(op);
      detailPane.heading.focus();
    } else cancelFocus(op);
  }
  function loadSessions(value) {
    if (!mounted || disposed) return;
    const {op, previous} = reserve("list"); let oldDetail = null;
    try {
      op.query = {dir:text(field(value, "dir")), canonicalDir:text(field(value, "canonicalDir")), workers:bool(field(value, "workers"))};
      if (!op.query.dir || !op.query.canonicalDir) fail();
      if (!alive(op)) return;
      if (!activeScope || activeScope.dir !== op.query.canonicalDir || activeScope.workers !== op.query.workers) {
        oldDetail = current.detail; current.detail = null; ++serial.detail;
        activeScope = {dir:op.query.canonicalDir, workers:op.query.workers}; op.scope = activeScope;
        selection = null; clearList();
        detailPane.status.textContent = ""; detailPane.root.setAttribute("aria-busy", "false");
      }
      listQuery = {...op.query};
    } catch { failed(op, listPane); return; }
    finally { retireMany(previous, oldDetail); }
    if (!alive(op)) return;
    status(op, listPane, listView ? "Refreshing sessions; showing previous results." : "Loading sessions.", true);
    if (alive(op)) read(op, readSessions, {dir:op.query.dir, workers:op.query.workers}, listPane, listCommit);
  }
  function select(value, trigger = null) {
    if (!mounted || disposed) return;
    const {op, previous} = reserve("detail");
    selection = null; markers();
    try {
      op.query = scope(value);
      if (!alive(op)) return;
      selection = {scope:{...op.query}, row:trigger?.item || null}; markers();
      if (trigger) {
        const button = trigger.item.button, intent = {button, view:trigger.view, revoked:false};
        const leave = () => { intent.revoked = true; };
        button.addEventListener("focusout", leave);
        intent.remove = () => button.removeEventListener("focusout", leave); op.intent = intent;
      }
    } catch { failed(op, detailPane); return; }
    finally { retire(previous); }
    if (!alive(op)) return;
    status(op, detailPane, "Loading conversation.", true);
    if (alive(op)) read(op, readConversation, {...op.query, ...detailQuery}, detailPane, detailCommit);
  }
  function refresh(which = "all") {
    if (!mounted || disposed) return;
    const list = listQuery && {...listQuery}, selected = selection, beforeScope = activeScope;
    const listVersion = serial.list + ((which === "all" || which === "list") && list ? 1n : 0n);
    if ((which === "all" || which === "list") && list) loadSessions(list);
    if ((which === "all" || which === "detail") && selected && mounted && !disposed &&
        serial.list === listVersion && activeScope === beforeScope && selection === selected) select({...selected.scope});
  }
  function teardown() {
    if (disposed) return;
    disposed = true; mounted = false;
    const oldList = current.list, oldDetail = current.detail;
    current.list = current.detail = null; ++serial.list; ++serial.detail;
    selection = listQuery = activeScope = null;
    clearList(); for (const remove of removes) remove(); removes.length = 0;
    retireMany(oldList, oldDetail); root.remove();
    readSessions = readConversation = copyText = announce = null;
  }
  function mount() {
    if (disposed) return false;
    if (mounted) return true;
    if (!root.isConnected || root.ownerDocument !== document) return false;
    try {
      mounted = true;
      for (const [button, callback] of [[listPane.refresh, () => refresh("list")], [detailPane.refresh, () => refresh("detail")]]) {
        button.addEventListener("click", callback); removes.push(() => button.removeEventListener("click", callback));
      }
      return true;
    } catch { teardown(); return false; }
  }
  return {fragment, mount, loadSessions, selectConversation:value => select(value), refresh, teardown};
}
