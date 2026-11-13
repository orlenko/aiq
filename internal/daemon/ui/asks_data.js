import {lineClassNames} from "./dom.js";

// Pure admission of supplied facts: no reader, DOM, defaults or live-state inference.
const fail = () => { throw new TypeError("could not capture asks snapshot"); };
const require = value => { if (!value) fail(); };
const sectionKeys = ["asks", "warnings", "other_open", "probably_done", "closed_dismissed"];
const kinds = new Set(["text", "table", "rule", "quote", "code"]);
const states = new Set(["open", "probably-done", "closed", "dismissed"]);
const uuid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
function object(value) {
  require(value !== null && typeof value === "object" && !Array.isArray(value));
}
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
const time = {ms:"int", known:"bool"};
const span = {c:["style"], d:"bool", t:"str"};
const line = {kind:"str", table_rows:"uint", spans:[span]};
const wording = {lines:[line], copy:"str"};
const item = {id:"str", provider:"str", session_id:"str", workspace:"str", state:"str",
  raised_at:time, state_at:time, repeats:"uint", state_reason:"str", state_text:"str",
  refs:["str"], first:wording, latest:wording};
const section = {key:"str", items:[item]};
const scope = {mode:"str", dir:"str"};
const report = {pass_at:time, ok_at:time, error_at:time, next_try_at:time,
  watched:"uint", waiting:"uint", skipped:"uint", calls_today:"uint",
  last_error:"str", not_watched:["str"]};
const totals = {utc_day:"str", skipped:"uint", calls_today:"uint"};
const moved = {local_day:"str", start_at:time, ids:["str"]};

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
  if (typeof schema === "object") {
    const out = {};
    for (const [key, child] of Object.entries(schema)) {
      out[key] = capture(field(value, key), child, budget, schema === section && key === "items");
    }
    return out;
  }
  if (schema === "str" || schema === "style") {
    require(typeof value === "string");
    debit(budget, "stringUnits", value.length);
    if (schema === "style") require(lineClassNames([value]).length === 1);
  } else if (schema === "bool") require(typeof value === "boolean");
  else require(Number.isSafeInteger(value) && (schema === "int" || value >= 0));
  return value;
}
function validTime(value) { require(value.known || value.ms === 0); }
function validScope(value) {
  require(value.mode === "all" && value.dir === "" ||
    value.mode === "directory" && typeof value.dir === "string" && value.dir !== "");
}
function validCopy(value) {
  require(value.endsWith("\n\n"));
  require(!/[\x00-\x08\x0b-\x1f\x7f-\x9f\u061c\u200e\u200f\u2028-\u202e\u2066-\u2069]/.test(value));
  for (const text of value.slice(0, -2).split("\n")) require(text === ">" || text.startsWith("> "));
}
function validWording(value) {
  validCopy(value.copy);
  for (const row of value.lines) require(kinds.has(row.kind));
}

// Structural quoting cannot establish paint/copy or stored-incarnation provenance.
// The later trusted producer supplies both variants using the legacy recipe.
export function captureAsks(data, expectedScope, limits) {
  try {
    const expected = {mode:field(expectedScope, "mode"), dir:field(expectedScope, "dir")};
    validScope(expected);
    const budget = credits(limits);
    const out = {scope:capture(field(data, "scope"), scope, budget),
      sections:capture(field(data, "sections"), [section], budget)};
    validScope(out.scope);
    require(out.scope.mode === expected.mode && out.scope.dir === expected.dir);
    require(out.sections.length === sectionKeys.length);
    out.sections.forEach((group, index) => {
      require(group.key === sectionKeys[index]);
      for (const row of group.items) {
        require(/^[1-9][0-9]{0,18}$/.test(row.id) &&
          (row.id.length < 19 || row.id <= "9223372036854775807"));
        require(states.has(row.state) && (index < 3 ? row.state === "open" :
          index === 3 ? row.state === "probably-done" : row.state === "closed" || row.state === "dismissed"));
        validTime(row.raised_at); validTime(row.state_at);
        validWording(row.first); validWording(row.latest);
        row.selectable = ["claude", "codex", "agy"].includes(row.provider) && uuid.test(row.session_id);
      }
    });
    const d = Object.getOwnPropertyDescriptor(data, "report");
    out.report = {known:false};
    if (d) {
      require(Object.hasOwn(d, "value"));
      const known = field(d.value, "known");
      require(typeof known === "boolean");
      if (known) {
        out.report = {known:true, ...capture(d.value, report, budget)};
        for (const key of ["pass_at", "ok_at", "error_at", "next_try_at"]) validTime(out.report[key]);
      }
    }
    const t = Object.getOwnPropertyDescriptor(data, "totals");
    if (t) {
      require(Object.hasOwn(t, "value"));
      out.totals = capture(t.value, totals, budget);
      require(out.totals.utc_day !== "");
      if (out.report.known) require(out.totals.skipped === out.report.skipped &&
        out.totals.calls_today === out.report.calls_today);
    }
    const m = Object.getOwnPropertyDescriptor(data, "moved_today");
    if (m) {
      require(Object.hasOwn(m, "value"));
      out.moved_today = capture(m.value, moved, budget);
      const overview = out.moved_today;
      require(overview.local_day !== "" && overview.start_at.known && overview.start_at.ms % 1000 === 0);
      // Consistency of the complete supplied snapshot, not proof of local-zone provenance.
      const counts = new Map(), eligible = [];
      for (const group of out.sections) for (const row of group.items) {
        counts.set(row.id, (counts.get(row.id) || 0) + 1);
        if ((row.state === "closed" || row.state === "probably-done") && row.state_reason !== "person") {
          require(row.state_at.known); // Unknown cannot establish absence or zero.
          if (row.state_at.ms >= overview.start_at.ms) eligible.push(row);
        }
      }
      for (const row of eligible) require(counts.get(row.id) === 1);
      eligible.sort((a, b) => {
        if (a.state_at.ms !== b.state_at.ms) return a.state_at.ms > b.state_at.ms ? -1 : 1;
        if (a.id.length !== b.id.length) return a.id.length > b.id.length ? -1 : 1;
        return a.id === b.id ? 0 : a.id > b.id ? -1 : 1;
      });
      require(overview.ids.length === eligible.length);
      eligible.forEach((row, i) => require(overview.ids[i] === row.id));
    }
    return out;
  } catch { fail(); }
}
