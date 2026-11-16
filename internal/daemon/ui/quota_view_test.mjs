import test from "node:test";
import assert from "node:assert/strict";
import {createQuotaView, quotaPercent, quotaMoney, quotaSpan, quotaLoginCommand} from "./quota_view.js";

// Synthetic DOM for lifecycle and content checks; NOT browser proof. Invented values only.
class Target {
  listeners = new Map();
  addEventListener(k, f) { if (!this.listeners.has(k)) this.listeners.set(k, new Set()); this.listeners.get(k).add(f); }
  removeEventListener(k, f) { this.listeners.get(k)?.delete(f); }
  count() { return [...this.listeners.values()].reduce((n, s) => n + s.size, 0); }
}
class Element extends Target {
  constructor(document, tag) {
    super();
    Object.assign(this, {ownerDocument: document, tagName: tag, children: [], parentNode: null, attrs: new Map(), className: "", value: "", style: {}, id: "", type: "", colSpan: 1});
  }
  append(...nodes) { for (const n of nodes) { if (n.tagName === "#fragment") { this.append(...n.children); continue; } n.remove(); n.parentNode = this; this.children.push(n); } }
  prepend(n) { n.remove(); n.parentNode = this; this.children.unshift(n); }
  replaceChildren(...nodes) { for (const n of [...this.children]) n.remove(); this.value = ""; this.append(...nodes); }
  remove() { if (this.parentNode) this.parentNode.children = this.parentNode.children.filter(n => n !== this); this.parentNode = null; }
  set textContent(t) { assert.equal(typeof t, "string"); this.replaceChildren(); this.value = t; }
  get textContent() { return this.value + this.children.map(n => n.textContent).join(""); }
  setAttribute(k, v) { this.attrs.set(k, String(v)); }
  getAttribute(k) { return this.attrs.get(k) ?? null; }
  contains(n) { return this === n || this.children.some(c => c.contains(n)); }
  get isConnected() { return this === this.ownerDocument.body || Boolean(this.parentNode?.isConnected); }
  click() { for (const f of [...(this.listeners.get("click") || [])]) f(); }
  focus() { this.ownerDocument.activeElement = this; }
}
const walk = n => [n, ...n.children.flatMap(walk)];
const tick = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return {promise, resolve, reject}; };

const NOW = 1790000000;
const limits = () => ({arrayEntries: 1000, stringUnits: 100000, rows: 100});
const win = (extra = {}) => ({key: "five_hour", label: "5h", kind: "short", scope: "", binding: true, used_pct: 83,
  resets_at: NOW + 2 * 3600 + 5 * 60, window_seconds: 18000, source: "probe", observed_at: NOW - 300, ...extra});
const login = (extra = {}) => ({has_credential: true, cli: "in", cli_identity: "", cli_mismatch: false, checked_at: null, problem: "", ...extra});
const account = (id, provider, extra = {}) => ({id, provider, plan: "max", label: "", identity: id + "@invented.test", enabled: true,
  login: login(), eligible: true, ineligible: "", exhausted: false, reason: "", cooldown_until: null, rank: 1, score: 3.5, rung: 0,
  terms: ["5h 83% used"], reset_credits: 0, reset_credit_expires_at: null, extra_usage: null, poll_error: "", observed_at: NOW - 60,
  windows: [win(), win({key: "seven_day", label: "7d", kind: "weekly", used_pct: null, resets_at: null, binding: false, scope: "invented-model"})],
  leases: [{mode: "long", drain: "", depth: 0, pane: "%7", pid: 11, workspace: "/invented/work", cwd: "/invented/cwd", why: ""}], ...extra});
const doc = (extra = {}) => ({schema: 1, generated_at: NOW, hostname: "invented-host-identity",
  poll: {last_at: NOW - 120, interval_s: 300, polling: false, error: ""},
  model_scope: [{provider: "claude", scope: "invented-model"}],
  accounts: [account("claude/a", "claude"), account("claude/b", "claude", {rank: 2, score: null, rung: 1}), account("codex/c", "codex")],
  rankings: [
    {key: "claude/interactive", entries: [
      {id: "claude/a", score: 3.5, rung: 0, eligible: true, reason: "", terms: ["5h 83% used"]},
      {id: "claude/b", score: null, rung: 1, eligible: true, reason: "", terms: []},
      {id: "claude/z", score: null, rung: 0, eligible: false, reason: "invented: logged out", terms: []}]},
    {key: "claude/worker", entries: []}], ...extra});

function fixture(extra = {}) {
  const window = new Target(), calls = [], copies = [], announcements = [];
  const document = {defaultView: window, activeElement: null,
    createElement(tag) { return new Element(document, tag); }, createDocumentFragment() { return new Element(document, "#fragment"); }};
  document.body = new Element(document, "body");
  document.activeElement = document.body;
  window.AbortController = AbortController;
  const options = {window, limits: limits(),
    readQuotas: (query, signal) => { const d = deferred(); calls.push({query, signal, ...d}); return d.promise; },
    announce: text => announcements.push(text), copyText: text => { copies.push(text); }, ...extra};
  const view = createQuotaView(document, options);
  const all = () => walk(document.body);
  return {view, document, window, calls, copies, announcements,
    find: c => all().filter(n => n.className.split(" ").includes(c)),
    text: () => document.body.textContent,
    status: () => all().find(n => n.className === "nav-quotas-status").textContent,
    mount() { document.body.append(view.fragment); assert.equal(view.mount(), true); },
    async resolve(i = 0, data = doc()) { calls[i].resolve({data}); await tick(); },
    async reject(i = 0) { calls[i].reject(new Error("invented failure")); await tick(); },
    listeners: () => all().reduce((n, e) => n + e.count(), 0)};
}

test("helpers: percent, money in minor units, spans and quoted login commands", () => {
  assert.equal(quotaPercent(83), "83%");
  assert.equal(quotaPercent(42.54), "42.5%");
  assert.equal(quotaPercent(null), "");
  assert.equal(quotaMoney("USD", 1234), "USD 12.34");
  assert.equal(quotaMoney("USD", 5), "USD 0.05");
  assert.equal(quotaMoney("USD", -250), "-USD 2.50");
  assert.equal(quotaMoney("JPY", 500), "JPY 500");
  assert.equal(quotaMoney("BHD", 1234), "BHD 1.234");
  assert.equal(quotaMoney("", 700), "700 minor units, currency unknown");
  assert.equal(quotaMoney("usd", 700), "700 minor units of usd");
  // Well-formed but unsupported: Intl would assume two decimals; keep raw units.
  assert.equal(quotaMoney("ZZZ", 1234), "1234 minor units of ZZZ");
  assert.equal(quotaMoney("XQQ", 5), "5 minor units of XQQ");
  assert.equal(quotaMoney("USD", 9007199254740991), "USD 90071992547409.91");
  assert.equal(quotaSpan(0, 45), "45s");
  assert.equal(quotaSpan(0, 7500), "2h 05m");
  assert.equal(quotaSpan(0, 90000), "1d 1h");
  assert.equal(quotaSpan(null, 5), "");
  assert.equal(quotaLoginCommand("claude/work", "claude"), "aiq account login claude/work");
  assert.equal(quotaLoginCommand("claude/it's $x", "claude"), null); // whitespace
  assert.equal(quotaLoginCommand("claude/it's$x", "claude"), "aiq account login 'claude/it'\\''s$x'");
  assert.equal(quotaLoginCommand("claude/a\nb", "claude"), null);
  assert.equal(quotaLoginCommand("codex/a", "claude"), null);
  assert.equal(quotaLoginCommand("claude/-x", "claude"), null);
  assert.equal(quotaLoginCommand("claude/a/b", "claude"), null);
});

test("refuses incomplete configuration", () => {
  const document = {defaultView: {}, createElement() {}, createDocumentFragment() {}};
  const ok = {window: document.defaultView, limits: limits(), readQuotas() {}, announce() {}, copyText() {}};
  assert.throws(() => createQuotaView(document, {...ok, readQuotas: undefined}));
  assert.throws(() => createQuotaView(document, {...ok, window: {}}));
  assert.throws(() => createQuotaView(document, {...ok, limits: {arrayEntries: 1, stringUnits: 1}}));
  assert.throws(() => createQuotaView(document, Object.create(ok)));
});

test("mounts only when connected, then reads once with an empty query", async () => {
  const f = fixture();
  assert.ok(Object.isFrozen(f.view));
  assert.equal(f.view.mount(), false);
  assert.equal(f.view.refresh(), false);
  assert.equal(f.calls.length, 0);
  f.mount();
  assert.equal(f.view.mount(), true);
  assert.equal(f.calls.length, 1);
  assert.deepEqual(Object.keys(f.calls[0].query), []);
  assert.ok(f.calls[0].signal instanceof AbortSignal);
  assert.equal(f.status(), "Loading quotas");
  await f.resolve();
  assert.match(f.status(), /^Quotas updated/);
  assert.equal(f.find("nav-quotas-provider").length, 2);
  assert.deepEqual(f.find("nav-quotas-provider-heading").map(n => n.textContent), ["claude", "codex"]);
});

test("used bars, unknown windows, resets, binding, source and freshness", async () => {
  const f = fixture(); f.mount(); await f.resolve();
  const [known, unknown] = f.find("nav-quotas-window");
  assert.match(known.textContent, /83% used · 17% left/);
  assert.match(known.textContent, /resets in 2h 05m/);
  assert.match(known.textContent, /limits this account/);
  assert.match(known.textContent, /from probe/);
  assert.match(known.textContent, /read 5m ago/);
  const fill = walk(known).find(n => n.className === "nav-quotas-fill");
  assert.equal(fill.style.width, "83%");
  assert.equal(fill.getAttribute("data-level"), "warn");
  // Unknown is never 0% used or free capacity.
  assert.match(unknown.textContent, /usage unknown/);
  assert.doesNotMatch(unknown.textContent, /\d+% (used|left)/);
  assert.equal(walk(unknown).some(n => n.className === "nav-quotas-fill"), false);
  assert.equal(walk(unknown).find(n => n.className === "nav-quotas-bar").getAttribute("data-unknown"), "true");
  assert.match(unknown.textContent, /reset time unknown/);
  assert.match(unknown.textContent, /does not limit this account/);
  assert.match(unknown.textContent, /model scope invented-model/);
  assert.match(f.text(), /aiq last polled 2m ago · every 5m/);
});

test("used over 100 clamps the bar and shows 0 left", async () => {
  const f = fixture(); f.mount();
  const d = doc(); d.accounts[0].windows[0].used_pct = 130;
  await f.resolve(0, d);
  const row = f.find("nav-quotas-window")[0];
  assert.match(row.textContent, /130% used · 0% left/);
  assert.equal(walk(row).find(n => n.className === "nav-quotas-fill").style.width, "100%");
});

test("paid extra usage keeps minor-unit precision and unknown balance", async () => {
  const f = fixture(); f.mount();
  const d = doc();
  d.accounts[0].extra_usage = {currency: "USD", balance_minor: 1234, used_minor: 250, limit_minor: 1000, limit_reached: false, note: "invented note"};
  d.accounts[1].extra_usage = {currency: "JPY", balance_minor: null, used_minor: 300, limit_minor: 0, limit_reached: true, note: ""};
  await f.resolve(0, d);
  const [a, b] = f.find("nav-quotas-extra").map(n => n.textContent);
  assert.equal(a, "Paid extra usage: USD 12.34 prepaid, USD 2.50 of USD 10.00 used this month (invented note)");
  assert.equal(b, "Paid extra usage: balance unknown, JPY 300 used this month, monthly limit reached");
});

test("reset credits, cooldown, exhaustion and leases", async () => {
  const f = fixture(); f.mount();
  const d = doc();
  d.accounts[0].reset_credits = 2; d.accounts[0].reset_credit_expires_at = NOW + 86400;
  d.accounts[1].exhausted = true; d.accounts[1].eligible = false; d.accounts[1].reason = "invented: 5h at 100%";
  d.accounts[2].cooldown_until = NOW + 600;
  d.accounts[2].leases = [{mode: "worker", drain: "requested", depth: 2, pane: "", pid: 4242, workspace: "", cwd: "/invented/cwd", why: "invented why"}];
  await f.resolve(0, d);
  assert.match(f.find("nav-quotas-credits")[0].textContent, /^2 reset credits, next expires /);
  assert.equal(f.find("nav-quotas-credits")[1].textContent, "No reset credit");
  const cards = f.find("nav-quotas-card");
  assert.equal(cards[1].getAttribute("data-standing"), "bad");
  assert.match(cards[1].textContent, /Exhausted: invented: 5h at 100%/);
  assert.match(cards[2].textContent, /Cooling down, usable again in 10m/);
  const leases = f.find("nav-quotas-lease");
  assert.equal(leases[0].textContent, "long%7/invented/work");
  assert.equal(leases.at(-1).textContent, "worker · wrap-up requested · nested depth 2process 4242/invented/cwdinvented why");
});

test("login problems: copy a quoted command, keep raw errors behind a disclosure", async () => {
  const f = fixture(); f.mount();
  const d = doc();
  d.accounts[0].login = login({cli: "out"});
  d.accounts[1].login = login({cli: "unreadable", problem: "invented keychain text 0xdead"});
  d.accounts[2].login = login({cli_mismatch: true, cli_identity: "other@invented.test"});
  d.accounts[2].poll_error = "invented HTTP 500 body";
  await f.resolve(0, d);
  const cards = f.find("nav-quotas-card");
  assert.match(cards[0].textContent, /Not logged in/);
  assert.match(cards[2].textContent, /CLI is logged in as other@invented\.test/);
  assert.equal(walk(cards[1]).some(n => n.className === "nav-quotas-copy"), false);
  // Raw text only inside <details>; the visible line is a summary.
  for (const raw of ["invented keychain text 0xdead", "invented HTTP 500 body"]) {
    const holder = walk(f.document.body).find(n => n.tagName === "pre" && n.textContent === raw);
    assert.ok(holder);
    let p = holder; while (p && p.tagName !== "details") p = p.parentNode;
    assert.ok(p, raw + " is not in a disclosure");
  }
  const buttons = f.find("nav-quotas-copy");
  assert.equal(buttons.length, 2);
  buttons[0].click();
  await tick();
  assert.deepEqual(f.copies, ["aiq account login claude/a"]);
  assert.equal(f.announcements.at(-1), "Copied the login command");
  buttons[1].click();
  assert.deepEqual(f.copies, ["aiq account login claude/a", "aiq account login codex/c"]);
});

test("a copy that fails is reported; a disabled account offers no command", async () => {
  const f = fixture({copyText: () => Promise.reject(new Error("invented"))}); f.mount();
  const d = doc();
  d.accounts[0].login = login({has_credential: false});
  d.accounts[1].enabled = false; d.accounts[1].login = login({cli: "out"});
  await f.resolve(0, d);
  assert.equal(f.find("nav-quotas-copy").length, 1);
  assert.equal(f.find("nav-quotas-card")[1].getAttribute("data-standing"), "off");
  f.find("nav-quotas-copy")[0].click();
  await tick();
  assert.equal(f.announcements.at(-1), "Could not copy the login command");
});

test("ranking tables keep order, score, tier and reason", async () => {
  const f = fixture(); f.mount(); await f.resolve();
  const tables = f.find("nav-quotas-table");
  assert.equal(tables.length, 2);
  const rows = walk(tables[0]).filter(n => n.tagName === "tr").slice(1).map(r => r.children.map(c => c.textContent));
  assert.deepEqual(rows, [
    ["1", "claude/a", "3.5/h", "5h 83% used"],
    ["2", "claude/b", "unknown", "uses paid extra usage"],
    ["3", "claude/z", "not eligible", "invented: logged out"]]);
  assert.match(tables[1].textContent, /claude · workers.*No account/);
});

test("machine identity and scoring terms stay in technical disclosures", async () => {
  const f = fixture(); f.mount(); await f.resolve();
  const holders = walk(f.document.body).filter(n => n.children.length === 0 && n.value.includes("invented-host-identity"));
  assert.equal(holders.length, 1);
  let p = holders[0]; while (p && p.tagName !== "details") p = p.parentNode;
  assert.ok(p);
  for (const hidden of ["/invented/home", "worker_capacity", "transcript"]) assert.equal(f.text().includes(hidden), false);
});

test("a newer refresh retires the older read and aborts it", async () => {
  const f = fixture(); f.mount();
  assert.equal(f.view.refresh(), true);
  assert.equal(f.calls.length, 2);
  assert.equal(f.calls[0].signal.aborted, true);
  assert.equal(f.calls[1].signal.aborted, false);
  const d = doc(); d.hostname = "invented-stale";
  await f.resolve(0, d);
  assert.equal(f.find("nav-quotas-card").length, 0);
  assert.equal(f.status(), "Loading quotas");
  await f.resolve(1);
  assert.equal(f.find("nav-quotas-card").length, 3);
  assert.equal(f.text().includes("invented-stale"), false);
});

test("a failed refresh keeps the last display and labels it as old", async () => {
  const f = fixture(); f.mount(); await f.resolve();
  f.view.refresh();
  assert.equal(f.status(), "Refreshing quotas");
  await f.reject(1);
  assert.match(f.status(), /^Could not refresh quotas\. Showing the read from /);
  assert.equal(f.find("nav-quotas-card").length, 3);
  assert.equal(f.find("nav-quotas-snapshot")[0].getAttribute("data-stale"), "true");
  assert.match(f.find("nav-quotas-stale")[0].textContent, /^Refresh failed\./);
  // A refused document is a failure too; the stale notice is not repeated.
  f.view.refresh();
  await f.resolve(2, {...doc(), schema: 2});
  assert.equal(f.find("nav-quotas-stale").length, 1);
  f.view.refresh();
  await f.resolve(3);
  assert.equal(f.find("nav-quotas-stale").length, 0);
  assert.equal(f.find("nav-quotas-snapshot")[0].getAttribute("data-stale"), "false");
});

test("a first read that fails or is refused shows no quotas", async () => {
  const f = fixture(); f.mount();
  await f.resolve(0, {...doc(), extra: 1});
  assert.equal(f.status(), "Quotas unavailable");
  assert.equal(f.find("nav-quotas-card").length, 0);
  const g = fixture({readQuotas: () => { throw new Error("invented"); }});
  g.mount();
  assert.equal(g.status(), "Quotas unavailable");
});

test("teardown retires the read, listeners and callbacks", async () => {
  const f = fixture(); f.mount(); await f.resolve();
  const d = doc(); d.accounts[0].login = login({cli: "out"});
  f.view.refresh(); await f.resolve(1, d);
  const copyButton = f.find("nav-quotas-copy")[0];
  f.view.refresh();
  const before = f.announcements.length;
  f.view.teardown();
  assert.equal(f.calls[2].signal.aborted, true);
  assert.equal(f.listeners(), 0);
  assert.equal(copyButton.count(), 0);
  assert.equal(f.document.body.children.length, 0);
  await f.resolve(2);
  copyButton.click();
  assert.equal(f.view.refresh(), false);
  assert.equal(f.view.mount(), false);
  assert.equal(f.copies.length, 0);
  assert.equal(f.announcements.length, before);
  f.view.teardown();
});

test("redraws drop the previous render's listeners and move focus out of replaced content", async () => {
  const f = fixture(); f.mount();
  const d = doc(); d.accounts[0].login = login({cli: "out"});
  await f.resolve(0, d);
  const old = f.find("nav-quotas-copy")[0];
  old.focus();
  f.view.refresh(); await f.resolve(1, d);
  assert.equal(old.count(), 0);
  assert.equal(f.document.activeElement.className, "nav-quotas-refresh");
  assert.equal(f.listeners(), 2); // Refresh plus one copy button.
  old.click();
  assert.equal(f.copies.length, 0);
});
