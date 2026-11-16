import {quotaSnapshot} from "./quota_data.js";

// The Quotas page: reads cached aiq state through the parent's reader, admits it
// with quotaSnapshot, and renders it. It never polls, logs in, or reads a
// provider; Refresh reads aiq's cached state again. Unknown stays unknown: a
// window without a reading never shows as 0% used or as free capacity.
const fail = () => { throw new TypeError("could not configure Quotas view"); };
function field(value, key) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail();
  const d = Object.getOwnPropertyDescriptor(value, key);
  if (!d || !Object.hasOwn(d, "value")) fail();
  return d.value;
}

// Percentages keep one decimal at most, so 83 used reads as 17 left.
export function quotaPercent(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "";
  const rounded = Math.round(value * 10) / 10;
  return `${Number.isInteger(rounded) ? rounded : rounded.toFixed(1)}%`;
}

// Currencies shown with two decimals: ISO 4217 gives them two, and aiq's CLI
// (ExtraUsage.Money) renders every amount as minor/100. This is a fixed table,
// not the browser's: engines' currency data (CLDR) disagrees with ISO for some
// codes and would scale amounts differently in different browsers. Any other
// code, including zero- and three-decimal ones whose provider scale is not
// confirmed, keeps its raw minor units rather than a guessed scale.
const exponents = new Map(["USD", "EUR", "GBP", "CAD", "AUD", "NZD", "CHF", "SEK", "NOK", "DKK", "PLN", "SGD", "HKD", "INR", "BRL", "MXN", "ZAR", "ILS", "AED"].map(c => [c, 2]));

// Minor units with the currency's own precision, by integer string arithmetic.
// An unknown currency keeps the raw minor units rather than guessing a scale.
export function quotaMoney(currency, minor) {
  if (!Number.isSafeInteger(minor)) return "";
  const digits = typeof currency === "string" ? exponents.get(currency) : undefined;
  if (digits === undefined) return `${minor} minor units${currency ? ` of ${currency}` : ", currency unknown"}`;
  let text = String(Math.abs(minor));
  if (digits > 0) {
    text = text.padStart(digits + 1, "0");
    text = `${text.slice(0, -digits)}.${text.slice(-digits)}`;
  }
  return `${minor < 0 ? "-" : ""}${currency} ${text}`;
}

// Duration between two unix-second times, as "2h 05m"; "" when either is unknown.
export function quotaSpan(from, to) {
  if (!Number.isSafeInteger(from) || !Number.isSafeInteger(to)) return "";
  const s = Math.abs(to - from);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${String(Math.floor(s % 3600 / 60)).padStart(2, "0")}m`;
  return `${Math.floor(s / 86400)}d ${Math.floor(s % 86400 / 3600)}h`;
}

// A login command for an account id, quoted for a POSIX shell. null when the id
// is not one aiq could have made (provider/name, no control characters).
export function quotaLoginCommand(id, provider) {
  if (typeof id !== "string" || typeof provider !== "string" || !/^[a-z]+$/.test(provider) ||
      !id.startsWith(`${provider}/`) || id.length > 256 || /[\u0000-\u001f\u007f-\u009f\s]/.test(id)) return null;
  const name = id.slice(provider.length + 1);
  if (name === "" || name.includes("/") || name.startsWith(".") || name.startsWith("-")) return null;
  const quoted = /^[A-Za-z0-9_./@+:=,%-]+$/.test(id) ? id : `'${id.replaceAll("'", "'\\''")}'`;
  return `aiq account login ${quoted}`;
}

function rungText(rung) {
  if (rung === 1) return "extra usage";
  if (rung > 1) return `last resort #${rung - 1}`;
  return "";
}

// The account's headline badge, most urgent first, from structured fields only.
export function quotaStanding(a) {
  if (!a.enabled) return {kind: "off", text: "disabled"};
  if (!a.login.hasCredential || a.login.cli === "out") return {kind: "bad", text: "no login", login: true};
  if (a.login.cli === "unreadable") return {kind: "bad", text: "login unreadable"};
  if (a.login.cliMismatch) return {kind: "bad", text: `CLI is ${a.login.cliIdentity || "someone else"}`, login: true};
  if (a.exhausted) return {kind: "bad", text: "exhausted"};
  if (!a.eligible) return {kind: "warn", text: a.ineligible || "not eligible"};
  const parts = [`#${a.rank}`, a.score === null ? "score unknown" : `${a.score.toFixed(1)}/h`];
  const rung = rungText(a.rung);
  if (rung) parts.push(rung);
  return {kind: a.rank === 1 ? "first" : "ok", text: parts.join(" · ")};
}

// The Quotas page in the dashboard's compact layout: one column per provider,
// a short card per account, then the next-pick table. Detail that the old
// dashboard kept in tooltips sits in one small disclosure per card.
export function createQuotaView(document, options) {
  const window = field(options, "window");
  let readQuotas = field(options, "readQuotas"), announce = field(options, "announce"), copyText = field(options, "copyText");
  if (!window || window !== document.defaultView || ![readQuotas, announce, copyText].every(f => typeof f === "function")) fail();
  const limits = {}, supplied = field(options, "limits");
  for (const key of ["arrayEntries", "stringUnits", "rows"]) {
    const value = field(supplied, key);
    if (!Number.isSafeInteger(value) || value <= 0) fail();
    limits[key] = value;
  }
  options = null;

  const removes = [], drawn = [];
  const fragment = document.createDocumentFragment();
  const root = node("section", "nav-quotas"), title = node("h1", "nav-quotas-heading", "Quotas");
  const toolbar = node("div", "nav-quotas-toolbar"), refreshButton = node("button", "nav-quotas-refresh", "Refresh");
  const meta = node("p", "nav-quotas-meta", "");
  const status = node("p", "nav-quotas-status", "Quotas not loaded yet"), content = node("div", "nav-quotas-content");
  title.id = "nav-quotas-heading";
  root.setAttribute("aria-labelledby", title.id);
  refreshButton.type = "button";
  status.setAttribute("role", "status");
  status.setAttribute("aria-live", "polite");
  content.setAttribute("aria-busy", "false");
  toolbar.append(title, meta, refreshButton, status);
  root.append(toolbar, content);
  fragment.append(root);

  // A new read retires the one in flight; shown is the last accepted snapshot.
  let mounted = false, disposed = false, flight = null, shown = null, shownNode = null, render = 0, copying = null;
  // Accounts whose session list the person opened; kept across refreshes.
  const openSessions = new Set();

  function node(tag, className, text) {
    const n = document.createElement(tag);
    if (className) n.className = className;
    if (text !== undefined) n.textContent = text;
    return n;
  }
  function hidden(text) { return node("span", "nav-quotas-sr", text); }
  function listen(target, callback, list = removes, type = "click") {
    target.addEventListener(type, callback);
    list.push(() => target.removeEventListener(type, callback));
  }
  function unlisten(list) { for (const remove of list.splice(0)) remove(); }
  function say(message) {
    if (disposed) return;
    status.textContent = message;
    try { announce(message); } catch { /* A status sink cannot change the page. */ }
  }
  function local(seconds) {
    if (seconds === null) return "unknown";
    const date = new Date(seconds * 1000);
    if (Number.isNaN(date.getTime())) return "unknown";
    try {
      return date.toLocaleString([], {weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit"});
    } catch { return date.toISOString(); }
  }
  function ago(now, seconds) {
    if (seconds === null || now === null) return "never";
    return seconds > now ? `${quotaSpan(now, seconds)} after this read` : `${quotaSpan(seconds, now)} ago`;
  }
  function inSpan(now, seconds) {
    if (seconds === null || now === null) return "";
    return seconds <= now ? "due" : `in ${quotaSpan(now, seconds)}`;
  }
  function copy(token, text) {
    if (disposed || !mounted || token !== render || copying || !root.isConnected) return;
    const mark = {};
    copying = mark;
    const finish = ok => {
      if (copying === mark) copying = null;
      if (!disposed && token === render) say(ok ? "Copied the login command" : "Could not copy the login command");
    };
    try {
      const result = copyText(text);
      const then = result?.then;
      if (typeof then === "function") then.call(result, () => finish(true), () => finish(false));
      else finish(true);
    } catch { finish(false); }
  }
  function remedy(id, command, token) {
    const wrap = node("p", "nav-quotas-remedy");
    const b = node("button", "nav-quotas-copy", "Copy");
    b.type = "button";
    b.setAttribute("aria-label", `Copy login command for ${id}`);
    listen(b, () => copy(token, command), drawn);
    wrap.append(node("code", "nav-quotas-command", command), b);
    return wrap;
  }

  // name | bar | used%, as on the dashboard. The bar is decoration; the text
  // says used and left. A window without a reading shows "–", never 0%.
  function windowRow(w, now) {
    const row = node("li", "nav-quotas-window");
    row.setAttribute("data-binding", w.binding ? "true" : "false");
    row.title = [w.key, w.source || "unknown source", `read ${ago(now, w.observedAt)}`].join(" · ");
    const bar = node("span", "nav-quotas-bar");
    bar.setAttribute("aria-hidden", "true");
    const value = node("span", "nav-quotas-window-value");
    if (w.used === null) {
      bar.setAttribute("data-unknown", "true");
      value.append(node("span", "", "–"), hidden(" usage unknown"));
    } else {
      const fill = node("span", "nav-quotas-fill");
      fill.style.width = `${Math.min(100, w.used)}%`;
      fill.setAttribute("data-level", w.used >= 95 ? "bad" : w.used >= 80 ? "warn" : "ok");
      bar.append(fill);
      value.append(node("span", "", quotaPercent(w.used)), hidden(` used, ${quotaPercent(w.remaining)} left`));
    }
    row.append(node("span", "nav-quotas-window-name", w.label || w.key), bar, value);
    return row;
  }
  function money(e) {
    const parts = [e.balanceMinor === null ? "balance unknown" : `${quotaMoney(e.currency, e.balanceMinor)} prepaid`];
    if (e.limitMinor > 0) parts.push(`${quotaMoney(e.currency, e.usedMinor)} of ${quotaMoney(e.currency, e.limitMinor)} this month`);
    else if (e.usedMinor > 0) parts.push(`${quotaMoney(e.currency, e.usedMinor)} this month`);
    if (e.limitReached) parts.push("monthly limit reached");
    return `extra usage ${parts.join(", ")}${e.note ? ` (${e.note})` : ""}`;
  }
  // The dashboard's one-line summary under the bars.
  function subLine(a, now) {
    const sub = node("p", "nav-quotas-sub");
    const parts = [];
    const add = (text, className = "") => parts.push(node("span", className, text));
    const resets = a.windows.filter(w => w.binding && w.resetsAt !== null).map(w => `${w.label || w.key} resets ${inSpan(now, w.resetsAt)}`);
    if (resets.length) add(resets.join(", "));
    if (a.exhausted && a.reason) add(a.reason, "nav-quotas-bad");
    if (a.cooldownUntil !== null && (now === null || a.cooldownUntil > now)) add(`cooling down, usable ${inSpan(now, a.cooldownUntil)}`, "nav-quotas-warn");
    if (a.resetCredits > 0) {
      add(`⟳ ${a.resetCredits} reset credit${a.resetCredits === 1 ? "" : "s"}${a.resetCreditExpiresAt === null ? "" : `, next expires ${local(a.resetCreditExpiresAt)}`}`, "nav-quotas-credit");
    } else if (a.exhausted && (a.provider === "claude" || a.provider === "codex")) add("no reset credit");
    if (a.extraUsage) add(money(a.extraUsage));
    if (a.pollError) add("last usage read failed", "nav-quotas-bad");
    add(a.observedAt === null ? "never read" : `seen ${ago(now, a.observedAt)}`);
    parts.forEach((p, i) => { if (i) sub.append(" · "); sub.append(p); });
    return sub;
  }
  // Where a session runs: this machine, or another one, with the age of its
  // last answer when that machine did not answer the last ask.
  function machineText(machines, name, now) {
    if (name === "") return "here";
    const m = machines.get(name);
    if (!m || m.status === "ok") return name;
    return `${name} (${m.seenAt === null ? "no answer" : ago(now, m.seenAt)})`;
  }
  function leaseRow(l, machines, now) {
    const row = node("li", "nav-quotas-lease");
    if (machines.size) {
      const where = node("span", "nav-quotas-lease-machine", l.machine || "here"), m = machines.get(l.machine);
      if (m && m.status !== "ok") {
        where.setAttribute("data-status", m.status);
        where.title = machineText(machines, l.machine, now);
        where.append(hidden(` (not answering; last answer ${m.seenAt === null ? "never" : ago(now, m.seenAt)})`));
      }
      row.append(where);
    }
    row.setAttribute("data-here", l.machine === "" ? "true" : "false");
    const mode = [l.mode || "session"];
    if (l.drain) mode.push(l.drain);
    if (l.depth) mode.push(`d${l.depth}`);
    const target = l.pane || (l.pid ? `pid ${l.pid}` : "terminal unknown");
    row.append(node("span", "nav-quotas-lease-mode", mode.join(" · ")), node("span", "nav-quotas-lease-target", target),
      node("span", "nav-quotas-lease-workspace", l.workspace || l.cwd || "workspace unknown"));
    row.setAttribute("data-mode", l.mode);
    if (l.why) row.append(node("span", "nav-quotas-lease-why", l.why));
    return row;
  }
  // Everything the dashboard showed only in tooltips, plus raw errors.
  function details(a, now) {
    const box = node("details", "nav-quotas-details"), list = node("ul", "nav-quotas-detail-list");
    box.append(node("summary", "", "Details"), list);
    const item = text => list.append(node("li", "", text));
    for (const w of a.windows) {
      const facts = [w.used === null ? "usage unknown" : `${quotaPercent(w.used)} used, ${quotaPercent(w.remaining)} left`,
        w.resetsAt === null ? "reset time unknown" : `resets ${local(w.resetsAt)}`,
        w.binding ? "limits this account" : "does not limit this account"];
      if (w.scope) facts.push(`model scope ${w.scope}`);
      facts.push(`from ${w.source || "unknown source"}`, `read ${ago(now, w.observedAt)}`);
      item(`${w.label || w.key}: ${facts.join(" · ")}`);
    }
    const login = [`CLI login ${a.login.cli}`];
    if (a.login.cliIdentity) login.push(`as ${a.login.cliIdentity}`);
    if (a.login.checkedAt !== null) login.push(`checked ${ago(now, a.login.checkedAt)}`);
    item(login.join(" · "));
    if (!a.eligible && a.ineligible) item(`not eligible: ${a.ineligible}`);
    for (const t of a.terms) list.append(node("li", "nav-quotas-term", t));
    for (const [label, raw] of [["Login error", a.login.problem], ["Last usage read error", a.pollError]]) {
      if (!raw) continue;
      const li = node("li", "");
      li.append(`${label}:`, node("pre", "nav-quotas-raw-text", raw));
      list.append(li);
    }
    return box;
  }
  // The account's long and interactive sessions on every machine, collapsed
  // to one line of counts so every account stays above the fold.
  function sessions(a, machines, now) {
    const box = node("details", "nav-quotas-sessions");
    const counts = new Map();
    for (const l of a.leases) counts.set(l.machine, (counts.get(l.machine) || 0) + 1);
    const n = a.leases.length, parts = [`${n} session${n === 1 ? "" : "s"}`];
    if (machines.size) {
      // Machines in the server's order: here first, then [fleet] order.
      for (const name of ["", ...machines.keys()]) if (counts.has(name)) parts.push(`${counts.get(name)} ${machineText(machines, name, now)}`);
    }
    box.append(node("summary", "", parts.join(" · ")));
    const list = node("ul", "nav-quotas-leases");
    list.setAttribute("aria-label", `Sessions on ${a.id}`);
    for (const l of a.leases) list.append(leaseRow(l, machines, now));
    box.append(list);
    box.open = openSessions.has(a.id);
    listen(box, () => { if (box.open) openSessions.add(a.id); else openSessions.delete(a.id); }, drawn, "toggle");
    return box;
  }
  // Other machines that answer for these subscriptions, and what each said.
  function machineLine(s, now) {
    const line = node("p", "nav-quotas-machines");
    line.append("Sessions from this machine");
    for (const m of s.machines.slice(1)) {
      let text = m.name;
      if (m.status === "pending") text += ": asking…";
      else if (m.status === "error") text += m.seenAt === null ? ": not answering" : `: not answering since ${ago(now, m.seenAt)}`;
      if (m.unmatched > 0) text += ` (${m.unmatched} on accounts not set up here)`;
      const span = node("span", "nav-quotas-machine", text);
      span.setAttribute("data-status", m.status);
      line.append(", ", span);
    }
    return line;
  }
  function accountCard(a, now, token, machines) {
    const card = node("article", "nav-quotas-card");
    const standing = quotaStanding(a);
    card.setAttribute("data-standing", standing.kind);
    const head = node("h3", "nav-quotas-card-head");
    head.append(node("span", "nav-quotas-account", a.id));
    if (a.plan) head.append(node("span", "nav-quotas-plan", a.plan));
    const who = node("span", "nav-quotas-who", a.label || a.identity);
    if (a.identity) who.title = a.identity;
    head.append(who, node("span", "nav-quotas-badge", standing.text));
    card.append(head);
    if (standing.login) {
      const command = quotaLoginCommand(a.id, a.provider);
      if (command) card.append(remedy(a.id, command, token));
    }
    if (a.login.cli === "unreadable") card.append(node("p", "nav-quotas-bad nav-quotas-note", "The stored login could not be read; see Details."));
    if (a.windows.length) {
      const list = node("ul", "nav-quotas-windows");
      list.setAttribute("aria-label", `Usage windows for ${a.id}`);
      for (const w of a.windows) list.append(windowRow(w, now));
      card.append(list);
    } else card.append(node("p", "nav-quotas-empty", "no telemetry yet"));
    card.append(subLine(a, now), details(a, now));
    if (a.leases.length) card.append(sessions(a, machines, now));
    return card;
  }
  // One table, a group of rows per provider and mode, as on the dashboard.
  function rankingTable(rankings) {
    const box = node("section", "nav-quotas-rankings"), h = node("h2", "nav-quotas-section-heading", "Next pick");
    h.id = `nav-quotas-rankings-${render}`;
    box.setAttribute("aria-labelledby", h.id);
    const table = node("table", "nav-quotas-table");
    for (const r of rankings) {
      const body = node("tbody"), headRow = node("tr"), th = node("th", "nav-quotas-ranking-key", `${r.provider} · ${r.mode}`);
      th.colSpan = 3;
      th.setAttribute("scope", "colgroup");
      headRow.append(th);
      body.append(headRow);
      r.entries.forEach((e, i) => {
        const tr = node("tr");
        tr.setAttribute("data-eligible", e.eligible ? "true" : "false");
        const id = node("th", "nav-quotas-mono", `${i + 1}. ${e.id}`);
        id.setAttribute("scope", "row");
        const score = e.eligible ? (e.score === null ? "score unknown" : `${e.score.toFixed(1)}/h`) : "";
        const why = e.eligible ? [rungText(e.rung), ...e.terms].filter(Boolean).join(" + ") : e.reason;
        tr.append(id, node("td", "nav-quotas-mono", score), node("td", "nav-quotas-why", why));
        body.append(tr);
      });
      if (!r.entries.length) {
        const tr = node("tr"), td = node("td", "nav-quotas-empty", "no account");
        td.colSpan = 3;
        tr.append(td);
        body.append(tr);
      }
      table.append(body);
    }
    box.append(h, table);
    return box;
  }
  function draw(s) {
    unlisten(drawn); // The previous render's buttons stop working before new ones exist.
    render++;
    const token = render, now = s.generatedAt;
    const out = node("div", "nav-quotas-snapshot");
    out.setAttribute("data-stale", "false");
    // Other machines by name; empty when [fleet] lists none.
    const machines = new Map(s.machines.slice(1).map(m => [m.name, m]));
    if (machines.size) out.append(machineLine(s, now));
    const poll = [s.poll.lastAt === null ? "not polled yet" : `polled ${ago(now, s.poll.lastAt)}`];
    if (s.poll.intervalS > 0) poll.push(`every ${quotaSpan(0, s.poll.intervalS)}`);
    if (s.poll.polling) poll.push("polling…");
    for (const m of s.modelScope) if (m.scope) poll.push(`${m.provider} scope: ${m.scope}`);
    meta.textContent = poll.join(" · ");
    if (s.poll.error) {
      const box = node("details", "nav-quotas-poll-error");
      box.append(node("summary", "nav-quotas-bad", "The last poll reported an error"), node("pre", "nav-quotas-raw-text", s.poll.error));
      out.append(box);
    }
    if (!s.providers.length) out.append(node("p", "nav-quotas-empty", "No accounts — run aiq account add <provider> <name>"));
    else {
      const columns = node("div", "nav-quotas-columns");
      columns.style.setProperty("--nav-quota-cols", String(s.providers.length));
      s.providers.forEach((p, i) => {
        const column = node("section", "nav-quotas-column"), h = node("h2", "nav-quotas-provider-heading", p.provider);
        h.id = `nav-quotas-provider-${token}-${i}`;
        column.setAttribute("aria-labelledby", h.id);
        column.append(h);
        for (const a of p.accounts) column.append(accountCard(a, now, token, machines));
        columns.append(column);
      });
      out.append(columns);
    }
    if (s.rankings.length) out.append(rankingTable(s.rankings));
    const tech = node("details", "nav-quotas-technical"), facts = node("ul", "nav-quotas-detail-list");
    facts.append(node("li", "", `Machine identity used for sessions: ${s.hostname}`), node("li", "", `Read ${local(now)}`));
    for (const m of machines.values()) {
      const li = node("li", "");
      li.append(`${m.name}: ${m.status === "ok" ? "answered" : m.status === "pending" ? "not asked yet" : "did not answer"}${m.seenAt === null ? "" : `, last answer ${local(m.seenAt)}`}`);
      if (m.error) li.append(node("pre", "nav-quotas-raw-text", m.error));
      facts.append(li);
    }
    tech.append(node("summary", "", "Technical details"), facts);
    out.append(tech);

    const focused = content.contains(document.activeElement);
    content.replaceChildren(out);
    shownNode = out;
    if (focused) refreshButton.focus();
  }
  function settle(token, ok, envelope) {
    if (disposed || token !== flight) return;
    flight = null;
    content.setAttribute("aria-busy", "false");
    let snapshot = null;
    if (ok) {
      try { snapshot = quotaSnapshot(field(envelope, "data"), limits); } catch { snapshot = null; }
    }
    if (snapshot) {
      try {
        draw(snapshot);
        shown = snapshot;
        say(`Quotas updated, read ${local(snapshot.generatedAt)}`);
        return;
      } catch { /* A snapshot that cannot be drawn is a failed read. */ }
    }
    if (shown && shownNode) {
      // Keep the last accepted display as it is, and say how old it is.
      if (shownNode.getAttribute("data-stale") !== "true") {
        shownNode.setAttribute("data-stale", "true");
        shownNode.prepend(node("p", "nav-quotas-stale", `Refresh failed. These quotas were read ${local(shown.generatedAt)} and may be out of date.`));
      }
      say(`Could not refresh quotas. Showing the read from ${local(shown.generatedAt)}.`);
    } else say("Quotas unavailable");
  }
  function refresh() {
    if (!mounted || disposed) return false;
    const old = flight;
    flight = null;
    try { old?.abort?.abort(); } catch { /* Abort cannot fail the new read. */ }
    const token = {abort: null};
    flight = token;
    content.setAttribute("aria-busy", "true");
    say(shown ? "Refreshing quotas" : "Loading quotas");
    try {
      token.abort = new window.AbortController();
      const result = readQuotas({}, token.abort.signal);
      const then = result?.then;
      if (typeof then !== "function") throw new Error("Quotas unavailable");
      then.call(result, value => settle(token, true, value), () => settle(token, false));
    } catch { settle(token, false); }
    return true;
  }
  function mount() {
    if (disposed) return false;
    if (mounted) return true;
    if (!root.isConnected || root.ownerDocument !== document) return false;
    mounted = true;
    refresh();
    return true;
  }
  function teardown() {
    if (disposed) return;
    disposed = true;
    mounted = false;
    const old = flight;
    flight = null;
    try { old?.abort?.abort(); } catch { /* Teardown continues. */ }
    unlisten(removes);
    unlisten(drawn);
    root.remove();
    root.replaceChildren();
    shown = null;
    shownNode = null;
    readQuotas = null; announce = null; copyText = null;
  }
  listen(refreshButton, refresh);
  return Object.freeze({fragment, mount, refresh, teardown});
}
