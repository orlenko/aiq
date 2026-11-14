import {captureConversation} from "./feed_data.js";
import {lineClassNames} from "./dom.js";
import {directory,route,encodeForm} from "./auth_codec.js";

// Admission units describe consumed input, not encoded bytes or total heap.
const fail = () => { throw new TypeError("could not capture navigator response"); };
const require = condition => { if (!condition) fail(); };
const uuid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
function field(value, key) {
  require(value !== null && typeof value === "object" && !Array.isArray(value));
  const descriptor = Object.getOwnPropertyDescriptor(value, key);
  require(descriptor && Object.hasOwn(descriptor, "value"));
  return descriptor.value;
}
function positive(value) {
  require(Number.isSafeInteger(value) && value > 0);
  return value;
}
function credits(limits) {
  return {arrayEntries:positive(field(limits, "arrayEntries")),
    stringUnits:positive(field(limits, "stringUnits")), rows:positive(field(limits, "rows"))};
}
function debit(budget, unit, length) {
  require(Number.isSafeInteger(length) && length >= 0 && length <= budget[unit]);
  budget[unit] -= length;
}

// These schemas have fixed depth. Unknown properties and methods are not read.
const time = {ms:"int", known:"bool"};
const session = {provider:"str", id:"str", cwd:"str", title:"str", model:"str", branch:"str",
  started:time, updated:time, worker:"bool", bypass:"bool"};
const head = {mode:"str", who:"str", clock_mode:"str", clock_text:"str", at:time,
  duration_known:"bool", duration_ms:"int", tools:"uint"};
const span = {c:["style"], d:"bool", t:"str"};
const line = {kind:"str", table_rows:"uint", spans:[span]};
const group = {ordinal:"uint", visible:"bool", leading_break:"bool", empty_body_line:"bool",
  head, base:["style"], lines:[line]};
const message = {id:"str", key:"str", kind:"str", sub:"str", turn:"int", at:time,
  source_known:"bool", source:"str", account_known:"bool", account:"str", raw:"str", groups:[group]};
const detail = {build:"str", session, earlier_hidden:"uint", later_hidden:"uint", window_start:"uint",
  reset:"bool", markup:"bool", messages:[message]};
const row = {provider:"str", id:"str", cwd:"str", label:"str", title:"str", model:"str", branch:"str",
  updated:"str", turns:"uint", tools:"uint", worker:"bool", bypass:"bool"};
const list = {dir:"str", roots_searched:["str"], partial:"bool", scanned:"uint", of:"uint",
  unreadable_count:"uint", sessions:[row]};
const catalog={source:"str",directories:[{path:"str",updated_ms:"uint"}]};

export function captureDirectories(data,limits){
  try{
    const out=capture(data,catalog,credits(limits));require(out.source==="launch metadata");
    const seen=new Set();let previous=null;
    for(const item of out.directories){
      directory(item.path);route("/sessions?dir="+encodeForm(item.path));require(item.updated_ms>0&&!seen.has(item.path));seen.add(item.path);
      require(previous===null||previous.updated_ms>=item.updated_ms);previous=item;
    }
    return out;
  }catch{fail();}
}

function capture(value, schema, budget, rowArray = false) {
  if (Array.isArray(schema)) {
    require(Array.isArray(value));
    const descriptor = Object.getOwnPropertyDescriptor(value, "length");
    require(descriptor && Object.hasOwn(descriptor, "value"));
    const length = descriptor.value;
    require(!rowArray || Number.isSafeInteger(length) && length <= budget.rows);
    // Refuse the entire length BEFORE reading any indexed descriptor or copying.
    debit(budget, "arrayEntries", length);
    const out = [];
    for (let i = 0; i < length; i++) {
      const item = Object.getOwnPropertyDescriptor(value, i);
      require(item && Object.hasOwn(item, "value"));
      out.push(capture(item.value, schema[0], budget));
    }
    return out;
  }
  if (typeof schema === "object") {
    const out = {};
    for (const [key, child] of Object.entries(schema)) {
      out[key] = capture(field(value, key), child, budget, schema === list && key === "sessions");
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
function validTime(value) {
  require(value.known || value.ms === 0);
}

// The host supplies the server-native canonical directory independently of data.
export function captureSessions(data, expectedCanonicalDir, limits) {
  try {
    require(typeof expectedCanonicalDir === "string" && expectedCanonicalDir !== "");
    const out = capture(data, list, credits(limits));
    require(out.dir === expectedCanonicalDir);
    for (const item of out.sessions) {
      item.selectable = ["claude", "codex", "agy"].includes(item.provider) && uuid.test(item.id);
    }
    return out;
  } catch { fail(); }
}

// Full header and paint/raw facts are captured once from the same caller graph.
export function captureDetail(data, expectedScope, limits) {
  try {
    const owned = capture(data, detail, credits(limits));
    validTime(owned.session.started); validTime(owned.session.updated);
    const body = captureConversation(owned, expectedScope);
    // The existing feed validator intentionally returns an identity-only session.
    return {...body, session:owned.session};
  } catch { fail(); }
}
