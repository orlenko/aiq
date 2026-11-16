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

// Minor units with the currency's own precision, by integer string arithmetic.
// An unknown currency keeps the raw minor units rather than guessing a scale.
export function quotaMoney(currency, minor) {
  if (!Number.isSafeInteger(minor)) return "";
  let digits;
  try {
    if (typeof currency !== "string" || !/^[A-Z]{3}$/.test(currency)) throw new RangeError();
    digits = new Intl.NumberFormat("en", {style: "currency", currency}).resolvedOptions().maximumFractionDigits;
  } catch {
    return `${minor} minor units${currency ? ` of ${currency}` : ", currency unknown"}`;
  }
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
  if (rung === 1) return "uses paid extra usage";
  if (rung > 1) return `last-resort provider #${rung - 1}`;
  return "";
}

// The account's headline state, most urgent first, from structured fields only.
export function quotaStanding(a) {
  if (!a.enabled) return {kind: "off", text: "Disabled"};
  if (!a.login.hasCredential || a.login.cli === "out") return {kind: "bad", text: "Not logged in", login: true};
  if (a.login.cli === "unreadable") return {kind: "bad", text: "Login could not be read"};
  if (a.login.cliMismatch) return {kind: "bad", text: `CLI is logged in as ${a.login.cliIdentity || "someone else"}`, login: true};
  if (a.exhausted) return {kind: "bad", text: a.reason ? `Exhausted: ${a.reason}` : "Exhausted"};
  if (!a.eligible) return {kind: "warn", text: a.ineligible ? `Not used now: ${a.ineligible}` : "Not used now"};
  const parts = [`Rank ${a.rank}`, a.score === null ? "score unknown" : `score ${a.score.toFixed(1)}/h`];
  const rung = rungText(a.rung);
  if (rung) parts.push(rung);
  return {kind: a.rank === 1 ? "first" : "ok", text: parts.join(" · ")};
}

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
  const status = node("p", "nav-quotas-status", "Quotas not loaded yet"), content = node("div", "nav-quotas-content");
  title.id = "nav-quotas-heading";
  root.setAttribute("aria-labelledby", title.id);
  refreshButton.type = "button";
  status.setAttribute("role", "status");
  status.setAttribute("aria-live", "polite");
  content.setAttribute("aria-busy", "false");
  toolbar.append(refreshButton, status);
  root.append(title, toolbar, content);
  fragment.append(root);

  // A new read retires the one in flight; shown is the last accepted snapshot.
  let mounted = false, disposed = false, flight = null, shown = null, shownNode = null, render = 0, copying = null;

  function node(tag, className, text) {
    const n = document.createElement(tag);
    if (className) n.className = className;
    if (text !== undefined) n.textContent = text;
    return n;
  }
  function listen(target, callback, list = removes) {
    target.addEventListener("click", callback);
    list.push(() => target.removeEventListener("click", callback));
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
  function until(now, seconds) {
    if (seconds === null || now === null) return "";
    return seconds <= now ? `due ${local(seconds)}` : `in ${quotaSpan(now, seconds)} (${local(seconds)})`;
  }
  // A visible one-line summary with the raw text behind a disclosure.
  function problem(className, summaryText, raw) {
    const box = node("div", `nav-quotas-problem ${className}`);
    box.append(node("p", "nav-quotas-problem-summary", summaryText));
    if (raw) {
      const details = node("details", "nav-quotas-raw"), summary = node("summary", "", "Show the error text");
      details.append(summary, node("pre", "nav-quotas-raw-text", raw));
      box.append(details);
    }
    return box;
  }
  function copyButton(label, text, token) {
    const b = node("button", "nav-quotas-copy", label);
    b.type = "button";
    const command = node("code", "nav-quotas-command", text);
    listen(b, () => copy(token, text), drawn);
    const wrap = node("p", "nav-quotas-remedy");
    wrap.append(command, b);
    return wrap;
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

  function windowRow(w, now) {
    const row = node("li", "nav-quotas-window");
    row.setAttribute("data-binding", w.binding ? "true" : "false");
    const name = node("span", "nav-quotas-window-name", w.label || w.key);
    const bar = node("span", "nav-quotas-bar");
    bar.setAttribute("aria-hidden", "true");
    let reading;
    if (w.used === null) {
      bar.setAttribute("data-unknown", "true");
      reading = "usage unknown";
    } else {
      const fill = node("span", "nav-quotas-fill");
      const width = Math.min(100, w.used);
      fill.style.width = `${width}%`;
      fill.setAttribute("data-level", w.used >= 95 ? "bad" : w.used >= 80 ? "warn" : "ok");
      bar.append(fill);
      reading = `${quotaPercent(w.used)} used · ${quotaPercent(w.remaining)} left`;
    }
    const value = node("span", "nav-quotas-window-value", reading);
    const facts = [];
    if (w.resetsAt !== null) facts.push(`resets ${until(now, w.resetsAt)}`);
    else facts.push("reset time unknown");
    facts.push(w.binding ? "limits this account" : "does not limit this account");
    if (w.scope) facts.push(`model scope ${w.scope}`);
    facts.push(`from ${w.source || "unknown source"}`);
    facts.push(`read ${ago(now, w.observedAt)}`);
    row.append(name, bar, value, node("span", "nav-quotas-window-facts", facts.join(" · ")));
    return row;
  }
  function leaseRow(l) {
    const row = node("li", "nav-quotas-lease");
    const mode = [l.mode || "session"];
    if (l.drain) mode.push(`wrap-up ${l.drain}`);
    if (l.depth) mode.push(`nested depth ${l.depth}`);
    const target = l.pane || (l.pid ? `process ${l.pid}` : "terminal unknown");
    const workspace = l.workspace || l.cwd || "workspace unknown";
    row.append(node("span", "nav-quotas-lease-mode", mode.join(" · ")), node("span", "nav-quotas-lease-target", target),
      node("span", "nav-quotas-lease-workspace", workspace));
    if (l.why) row.append(node("p", "nav-quotas-lease-why", l.why));
    return row;
  }
  function accountCard(a, now, token) {
    const card = node("article", "nav-quotas-card");
    const standing = quotaStanding(a);
    card.setAttribute("data-standing", standing.kind);
    const head = node("header", "nav-quotas-card-head");
    const heading = node("h3", "nav-quotas-account", a.id);
    head.append(heading);
    const who = [a.plan, a.label || a.identity].filter(Boolean).join(" · ");
    if (who) head.append(node("p", "nav-quotas-who", who));
    head.append(node("p", "nav-quotas-standing", standing.text));
    card.append(head);
    if (standing.login && a.enabled) {
      const command = quotaLoginCommand(a.id, a.provider);
      if (command) card.append(copyButton(`Copy login command for ${a.id}`, command, token));
    }
    if (a.login.cli === "unreadable") card.append(problem("nav-quotas-login-problem", "The stored login could not be read; aiq keeps the last known state.", a.login.problem));
    if (a.cooldownUntil !== null && (now === null || a.cooldownUntil > now)) card.append(node("p", "nav-quotas-cooldown", `Cooling down, usable again ${until(now, a.cooldownUntil)}`));

    if (a.windows.length) {
      const list = node("ul", "nav-quotas-windows");
      list.setAttribute("aria-label", `Usage windows for ${a.id}`);
      for (const w of a.windows) list.append(windowRow(w, now));
      card.append(list);
    } else card.append(node("p", "nav-quotas-empty", "No usage reading yet"));

    const facts = node("ul", "nav-quotas-facts");
    if (a.resetCredits > 0) {
      const expiry = a.resetCreditExpiresAt === null ? "" : `, next expires ${local(a.resetCreditExpiresAt)}`;
      facts.append(node("li", "nav-quotas-credits", `${a.resetCredits} reset credit${a.resetCredits === 1 ? "" : "s"}${expiry}`));
    } else if (a.exhausted && (a.provider === "claude" || a.provider === "codex")) {
      facts.append(node("li", "nav-quotas-credits", "No reset credit"));
    }
    const e = a.extraUsage;
    if (e) {
      const parts = [e.balanceMinor === null ? "balance unknown" : `${quotaMoney(e.currency, e.balanceMinor)} prepaid`];
      if (e.limitMinor > 0) parts.push(`${quotaMoney(e.currency, e.usedMinor)} of ${quotaMoney(e.currency, e.limitMinor)} used this month`);
      else if (e.usedMinor > 0) parts.push(`${quotaMoney(e.currency, e.usedMinor)} used this month`);
      if (e.limitReached) parts.push("monthly limit reached");
      const item = node("li", "nav-quotas-extra", `Paid extra usage: ${parts.join(", ")}`);
      if (e.note) item.append(node("span", "nav-quotas-note", ` (${e.note})`));
      facts.append(item);
    }
    facts.append(node("li", "nav-quotas-seen", a.observedAt === null ? "Never read" : `Last read ${ago(now, a.observedAt)}`));
    card.append(facts);
    if (a.pollError) card.append(problem("nav-quotas-poll-problem", "The last usage read failed; the values above may be old.", a.pollError));

    if (a.leases.length) {
      const list = node("ul", "nav-quotas-leases");
      list.setAttribute("aria-label", `Sessions on ${a.id}`);
      for (const l of a.leases) list.append(leaseRow(l));
      card.append(list);
    }
    if (a.terms.length || a.login.checkedAt !== null || a.login.cliIdentity) {
      const details = node("details", "nav-quotas-technical"), summary = node("summary", "", "Scoring details");
      details.append(summary);
      if (a.terms.length) {
        const terms = node("ul", "nav-quotas-terms");
        for (const t of a.terms) terms.append(node("li", "", t));
        details.append(terms);
      }
      const login = [`CLI login: ${a.login.cli}`];
      if (a.login.cliIdentity) login.push(`as ${a.login.cliIdentity}`);
      if (a.login.checkedAt !== null) login.push(`checked ${ago(now, a.login.checkedAt)}`);
      details.append(node("p", "", login.join(" · ")));
      card.append(details);
    }
    return card;
  }
  function rankingTable(r) {
    const box = node("section", "nav-quotas-ranking");
    const table = node("table", "nav-quotas-table"), caption = node("caption", "", `${r.provider} · ${r.mode === "worker" ? "workers" : "interactive sessions"}`);
    const head = node("thead"), headRow = node("tr");
    for (const text of ["Order", "Account", "Score", "Why"]) {
      const th = node("th", "", text);
      th.setAttribute("scope", "col");
      headRow.append(th);
    }
    head.append(headRow);
    const body = node("tbody");
    r.entries.forEach((e, i) => {
      const tr = node("tr");
      tr.setAttribute("data-eligible", e.eligible ? "true" : "false");
      const id = node("th", "", e.id);
      id.setAttribute("scope", "row");
      const score = e.eligible ? (e.score === null ? "unknown" : `${e.score.toFixed(1)}/h`) : "not eligible";
      const why = e.eligible ? [rungText(e.rung), ...e.terms].filter(Boolean).join(" · ") : e.reason;
      tr.append(node("td", "", String(i + 1)), id, node("td", "", score), node("td", "nav-quotas-why", why));
      body.append(tr);
    });
    if (!r.entries.length) {
      const tr = node("tr"), td = node("td", "nav-quotas-empty", "No account");
      td.colSpan = 4;
      tr.append(td);
      body.append(tr);
    }
    table.append(caption, head, body);
    box.append(table);
    return box;
  }
  function draw(s) {
    unlisten(drawn); // The previous render's buttons stop working before new ones exist.
    render++;
    const token = render, now = s.generatedAt;
    const out = node("div", "nav-quotas-snapshot");
    out.setAttribute("data-stale", "false");
    const poll = [];
    poll.push(s.poll.lastAt === null ? "aiq has not polled yet" : `aiq last polled ${ago(now, s.poll.lastAt)}`);
    if (s.poll.intervalS > 0) poll.push(`every ${quotaSpan(0, s.poll.intervalS)}`);
    if (s.poll.polling) poll.push("polling now");
    out.append(node("p", "nav-quotas-poll", `${poll.join(" · ")}. Read ${local(now)}.`));
    if (s.poll.error) out.append(problem("nav-quotas-poll-error", "The last poll reported an error.", s.poll.error));

    if (!s.providers.length) out.append(node("p", "nav-quotas-empty", "No accounts. Add one with: aiq account add <provider> <name>"));
    for (const p of s.providers) {
      const group = node("section", "nav-quotas-provider");
      const h = node("h2", "nav-quotas-provider-heading", p.provider);
      h.id = `nav-quotas-provider-${token}-${s.providers.indexOf(p)}`;
      group.setAttribute("aria-labelledby", h.id);
      const cards = node("div", "nav-quotas-cards");
      for (const a of p.accounts) cards.append(accountCard(a, now, token));
      group.append(h, cards);
      out.append(group);
    }
    if (s.rankings.length) {
      const picks = node("section", "nav-quotas-rankings"), h = node("h2", "", "Next pick");
      h.id = `nav-quotas-rankings-${token}`;
      picks.setAttribute("aria-labelledby", h.id);
      picks.append(h);
      for (const r of s.rankings) picks.append(rankingTable(r));
      out.append(picks);
    }
    const tech = node("details", "nav-quotas-technical"), sum = node("summary", "", "Technical details");
    const facts = node("ul", "nav-quotas-terms");
    facts.append(node("li", "", `Machine identity used for sessions: ${s.hostname}`));
    for (const m of s.modelScope) facts.append(node("li", "", `${m.provider} model scope: ${m.scope || "none"}`));
    tech.append(sum, facts);
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
      shown = snapshot;
      draw(snapshot);
      say(`Quotas updated, read ${local(snapshot.generatedAt)}`);
      return;
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
