import test from "node:test";
import assert from "node:assert/strict";
import {quotaSnapshot} from "./quota_data.js";

// Invented accounts only.
const limits = {arrayEntries: 1000, stringUnits: 100000, rows: 100};
const win = (extra = {}) => ({key: "five_hour", label: "5h", kind: "short", scope: "", binding: true, used_pct: 42.5,
  resets_at: 1800000000, window_seconds: 18000, source: "probe", observed_at: 1790000000, ...extra});
const account = (id, provider, extra = {}) => ({id, provider, plan: "max", label: "", identity: id + "@invented.test", enabled: true,
  login: {has_credential: true, cli: "unknown", cli_identity: "", cli_mismatch: false, checked_at: null, problem: ""},
  eligible: true, ineligible: "", exhausted: false, reason: "", cooldown_until: null, rank: 1, score: 3.5, rung: 0, terms: ["5h 42%"],
  reset_credits: 0, reset_credit_expires_at: null, extra_usage: null, poll_error: "", observed_at: 1790000000,
  windows: [win(), win({key: "seven_day", label: "7d", kind: "weekly", used_pct: null, resets_at: null})],
  leases: [{mode: "long", drain: "", depth: 0, pane: "%7", pid: 1, workspace: "/invented/w", cwd: "/invented/w", why: "", machine: ""}], ...extra});
const doc = (extra = {}) => ({schema: 2, generated_at: 1790000001, hostname: "invented-host",
  poll: {last_at: null, interval_s: 60, polling: false, error: ""},
  model_scope: [{provider: "claude", scope: "fable"}],
  accounts: [account("claude/a", "claude"), account("claude/b", "claude"), account("codex/c", "codex")],
  rankings: [{key: "claude/interactive", entries: [{id: "claude/a", score: 3.5, rung: 0, eligible: true, reason: "", terms: []}]},
    {key: "claude/worker", entries: []}],
  machines: [{name: "", here: true, status: "ok", seen_at: 1790000001, error: "", sessions: 3, unmatched: 0}], ...extra});

test("admits a document into a frozen grouped model", () => {
  const s = quotaSnapshot(doc(), limits);
  assert.equal(s.hostname, "invented-host");
  assert.equal(s.poll.lastAt, null);
  assert.deepEqual(s.providers.map(p => [p.provider, p.accounts.length]), [["claude", 2], ["codex", 1]]);
  const [known, unknown] = s.providers[0].accounts[0].windows;
  assert.equal(known.used, 42.5);
  assert.equal(known.remaining, 57.5);
  assert.equal(unknown.used, null);
  assert.equal(unknown.remaining, null);
  assert.equal(unknown.resetsAt, null);
  assert.equal(s.rankings[0].provider, "claude");
  assert.equal(s.rankings[0].mode, "interactive");
  assert.ok(Object.isFrozen(s) && Object.isFrozen(s.providers[0].accounts[0].windows[0]) && Object.isFrozen(s.rankings[0].entries));
  assert.throws(() => { "use strict"; s.providers[0].accounts[0].windows[0].used = 0; });
});

test("machines: this one first, then others; sessions name their machine", () => {
  const d = doc();
  d.machines.push({name: "invented-box", here: false, status: "error", seen_at: null, error: "invented: Permission denied", sessions: 1, unmatched: 2});
  d.accounts[1].leases.push({mode: "interactive", drain: "", depth: 0, pane: "", pid: 9, workspace: "", cwd: "/invented/remote", why: "", machine: "invented-box"});
  const s = quotaSnapshot(d, limits);
  assert.deepEqual(s.machines.map(m => [m.name, m.here, m.status, m.seenAt, m.unmatched]), [["", true, "ok", 1790000001, 0], ["invented-box", false, "error", null, 2]]);
  assert.deepEqual(s.providers[0].accounts[1].leases.map(l => l.machine), ["", "invented-box"]);
  assert.ok(Object.isFrozen(s.machines[1]));
});

test("remaining is never below zero", () => {
  const d = doc();
  d.accounts[0].windows[0].used_pct = 130;
  assert.equal(quotaSnapshot(d, limits).providers[0].accounts[0].windows[0].remaining, 0);
});

test("unknown values stay null, they do not become zero", () => {
  const d = doc();
  d.accounts[0].score = null;
  d.accounts[0].extra_usage = {currency: "USD", balance_minor: null, used_minor: 250, limit_minor: 1000, limit_reached: false, note: "invented"};
  const a = quotaSnapshot(d, limits).providers[0].accounts[0];
  assert.equal(a.score, null);
  assert.equal(a.extraUsage.balanceMinor, null);
  assert.equal(a.observedAt, 1790000000);
});

const refusals = {
  "extra top-level field": d => { d.events = []; },
  "extra account field": d => { d.accounts[0].home = "/invented/home"; },
  "missing field": d => { delete d.accounts[0].plan; },
  "wrong schema": d => { d.schema = 1; },
  "no machines": d => { d.machines = []; },
  "other machine first": d => { d.machines[0] = {...d.machines[0], here: false, name: "invented-box"}; },
  "second here": d => { d.machines.push({...d.machines[0]}); },
  "unnamed other machine": d => { d.machines.push({...d.machines[0], here: false}); },
  "repeated machine": d => { const m = {...d.machines[0], here: false, name: "invented-box"}; d.machines.push(m, {...m}); },
  "unknown machine status": d => { d.machines.push({...d.machines[0], here: false, name: "invented-box", status: "maybe"}); },
  "session on an unlisted machine": d => { d.accounts[0].leases[0].machine = "invented-box"; },
  "string number": d => { d.accounts[0].windows[0].used_pct = "42"; },
  "negative used": d => { d.accounts[0].windows[0].used_pct = -1; },
  "zero time": d => { d.accounts[0].observed_at = 0; },
  "unknown cli state": d => { d.accounts[0].login.cli = "maybe"; },
  "empty id": d => { d.accounts[0].id = ""; },
  "providers not contiguous": d => { d.accounts.push(account("claude/z", "claude")); },
  "rankings unsorted": d => { d.rankings.reverse(); },
  "ranking mode": d => { d.rankings[0].key = "claude/batch"; },
  "ranking without provider": d => { d.rankings[0].key = "/worker"; },
  "inherited field": d => { d.accounts[0] = Object.assign(Object.create({plan: "max"}), d.accounts[0]); delete d.accounts[0].plan; },
};
for (const [name, mutate] of Object.entries(refusals)) {
  test("refuses " + name, () => {
    const d = doc();
    mutate(d);
    assert.throws(() => quotaSnapshot(d, limits), {message: "could not capture quota snapshot"});
  });
}

test("refuses over budget and bad limits", () => {
  assert.throws(() => quotaSnapshot(doc(), {...limits, rows: 2}));
  assert.throws(() => quotaSnapshot(doc(), {...limits, stringUnits: 20}));
  assert.throws(() => quotaSnapshot(doc(), {...limits, arrayEntries: 3}));
  assert.throws(() => quotaSnapshot(doc(), {arrayEntries: 1, stringUnits: 1}));
  assert.throws(() => quotaSnapshot(null, limits));
});
