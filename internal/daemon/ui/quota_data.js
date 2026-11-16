// Pure admission of a GET /api/nav/quotas document (schema 2): no reader, DOM,
// defaults or inference. null stays null (unknown); it never becomes zero.
const fail = () => { throw new TypeError("could not capture quota snapshot"); };
const require = value => { if (!value) fail(); };
function object(value) { require(value !== null && typeof value === "object" && !Array.isArray(value)); }
function own(value, key) {
  const d = Object.getOwnPropertyDescriptor(value, key);
  require(d && Object.hasOwn(d, "value"));
  return d.value;
}
function field(value, key) { object(value); return own(value, key); }
function debit(budget, key, length) {
  require(Number.isSafeInteger(length) && length >= 0 && length <= budget[key]);
  budget[key] -= length;
}
function credits(limits) {
  const out = {};
  for (const key of ["arrayEntries", "stringUnits", "rows"]) {
    const value = field(limits, key);
    require(Number.isSafeInteger(value) && value > 0);
    out[key] = value;
  }
  return out;
}
// "time?" is unix seconds or null; "num?" a finite number or null.
const poll = {last_at:"time?", interval_s:"uint", polling:"bool", error:"str"};
const scope = {provider:"str", scope:"str"};
const login = {has_credential:"bool", cli:"str", cli_identity:"str", cli_mismatch:"bool", checked_at:"time?", problem:"str"};
const extra = {currency:"str", balance_minor:"int?", used_minor:"int", limit_minor:"int", limit_reached:"bool", note:"str"};
const win = {key:"str", label:"str", kind:"str", scope:"str", binding:"bool", used_pct:"num?", resets_at:"time?",
  window_seconds:"uint", source:"str", observed_at:"time?"};
const lease = {mode:"str", drain:"str", depth:"uint", pane:"str", pid:"uint", workspace:"str", cwd:"str", why:"str", machine:"str"};
const machine = {name:"str", here:"bool", status:"str", seen_at:"time?", error:"str", sessions:"uint", unmatched:"uint"};
const account = {id:"str", provider:"str", plan:"str", label:"str", identity:"str", enabled:"bool", login,
  eligible:"bool", ineligible:"str", exhausted:"bool", reason:"str", cooldown_until:"time?",
  rank:"uint", score:"num?", rung:"uint", terms:["str"], reset_credits:"uint", reset_credit_expires_at:"time?",
  extra_usage:{nullable:extra}, poll_error:"str", observed_at:"time?", windows:[win], leases:[lease]};
const rank = {id:"str", score:"num?", rung:"uint", eligible:"bool", reason:"str", terms:["str"]};
const ranking = {key:"str", entries:[rank]};
const document = {schema:"uint", generated_at:"time?", hostname:"str", poll, model_scope:[scope], accounts:[account], rankings:[ranking], machines:[machine]};

function capture(value, schema, budget, rows = false) {
  if (Array.isArray(schema)) {
    require(Array.isArray(value));
    const length = own(value, "length");
    if (rows) debit(budget, "rows", length);
    debit(budget, "arrayEntries", length);
    const out = [];
    for (let i = 0; i < length; i++) out.push(capture(own(value, i), schema[0], budget));
    return out;
  }
  if (typeof schema === "object" && Object.hasOwn(schema, "nullable")) {
    return value === null ? null : capture(value, schema.nullable, budget);
  }
  if (typeof schema === "object") {
    object(value);
    const keys = Object.keys(schema);
    require(Object.keys(value).length === keys.length); // Closed: no extra field.
    const out = {};
    for (const key of keys) out[key] = capture(own(value, key), schema[key], budget, schema === document && key === "accounts");
    return out;
  }
  if (schema === "str") {
    require(typeof value === "string");
    debit(budget, "stringUnits", value.length);
  } else if (schema === "bool") require(typeof value === "boolean");
  else if (schema === "num?") require(value === null || typeof value === "number" && Number.isFinite(value));
  else if (schema === "time?") require(value === null || Number.isSafeInteger(value) && value > 0);
  else if (schema === "int?") require(value === null || Number.isSafeInteger(value));
  else if (schema === "int") require(Number.isSafeInteger(value));
  else require(Number.isSafeInteger(value) && value >= 0);
  return value;
}
function freeze(value) {
  if (value && typeof value === "object") {
    for (const child of Object.values(value)) freeze(child);
    Object.freeze(value);
  }
  return value;
}
const clis = new Set(["in", "out", "unreadable", "unknown"]);
const modes = new Set(["interactive", "worker"]);
const statuses = new Set(["ok", "error", "pending"]);

// quotaSnapshot admits data, the {data} of a quotas read, within the page's
// limits, and returns a frozen model. Accounts keep the server's order (by
// provider, then [display] order) and are grouped by provider in that order.
// A window's used and remaining are percentages, both null when unknown;
// remaining is 100 - used, never below 0.
export function quotaSnapshot(data, limits) {
  try {
    const budget = credits(limits);
    const d = capture(data, document, budget);
    require(d.schema === 2);
    const providers = [], byProvider = new Map();
    for (const a of d.accounts) {
      require(a.id !== "" && a.provider !== "" && clis.has(a.login.cli));
      let group = byProvider.get(a.provider);
      if (!group) {
        group = {provider: a.provider, accounts: []};
        byProvider.set(a.provider, group);
        providers.push(group);
      } else require(providers[providers.length - 1] === group); // Contiguous.
      group.accounts.push({
        id: a.id, provider: a.provider, plan: a.plan, label: a.label, identity: a.identity, enabled: a.enabled,
        login: {hasCredential: a.login.has_credential, cli: a.login.cli, cliIdentity: a.login.cli_identity,
          cliMismatch: a.login.cli_mismatch, checkedAt: a.login.checked_at, problem: a.login.problem},
        eligible: a.eligible, ineligible: a.ineligible, exhausted: a.exhausted, reason: a.reason, cooldownUntil: a.cooldown_until,
        rank: a.rank, score: a.score, rung: a.rung, terms: a.terms,
        resetCredits: a.reset_credits, resetCreditExpiresAt: a.reset_credit_expires_at,
        extraUsage: a.extra_usage === null ? null : {currency: a.extra_usage.currency, balanceMinor: a.extra_usage.balance_minor,
          usedMinor: a.extra_usage.used_minor, limitMinor: a.extra_usage.limit_minor, limitReached: a.extra_usage.limit_reached, note: a.extra_usage.note},
        pollError: a.poll_error, observedAt: a.observed_at,
        windows: a.windows.map(w => {
          require(w.used_pct === null || w.used_pct >= 0);
          return {key: w.key, label: w.label, kind: w.kind, scope: w.scope, binding: w.binding,
            used: w.used_pct, remaining: w.used_pct === null ? null : Math.max(0, 100 - w.used_pct),
            resetsAt: w.resets_at, windowSeconds: w.window_seconds, source: w.source, observedAt: w.observed_at};
        }),
        leases: a.leases.map(l => ({mode: l.mode, drain: l.drain, depth: l.depth, pane: l.pane, pid: l.pid, workspace: l.workspace, cwd: l.cwd, why: l.why, machine: l.machine})),
      });
    }
    let previous = "";
    const rankings = d.rankings.map(r => {
      const at = r.key.indexOf("/");
      require(at > 0 && r.key > previous && modes.has(r.key.slice(at + 1)));
      previous = r.key;
      return {key: r.key, provider: r.key.slice(0, at), mode: r.key.slice(at + 1),
        entries: r.entries.map(e => ({id: e.id, score: e.score, rung: e.rung, eligible: e.eligible, reason: e.reason, terms: e.terms}))};
    });
    // This machine first, exactly once, then other machines by distinct name.
    const names = new Set();
    const machines = d.machines.map((m, i) => {
      require(statuses.has(m.status) && m.here === (i === 0) && (m.here ? m.name === "" : m.name !== "" && !names.has(m.name)));
      names.add(m.name);
      return {name: m.name, here: m.here, status: m.status, seenAt: m.seen_at, error: m.error, sessions: m.sessions, unmatched: m.unmatched};
    });
    require(machines.length > 0);
    for (const group of providers) for (const a of group.accounts) for (const l of a.leases) require(l.machine === "" || names.has(l.machine));
    return freeze({generatedAt: d.generated_at, hostname: d.hostname, machines,
      poll: {lastAt: d.poll.last_at, intervalS: d.poll.interval_s, polling: d.poll.polling, error: d.poll.error},
      modelScope: d.model_scope.map(s => ({provider: s.provider, scope: s.scope})), providers, rankings});
  } catch { fail(); }
}
