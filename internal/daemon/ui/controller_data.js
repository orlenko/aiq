import {captureConversation} from "./feed_data.js";
import {lineClassNames} from "./dom.js";
import {canonical32,directory,route,encodeForm} from "./auth_codec.js";

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
function ownDetail(data,limits){
    const budget=credits(limits),owned = capture(data, detail, budget);
    const selection=Object.getOwnPropertyDescriptor(data,"selection");
    if(selection){
      require(Object.hasOwn(selection,"value"));
      owned.selection=capture(selection.value,"str",budget);
      require(owned.selection===""||canonical32(owned.selection));
    }
    const tail=["tail_turn","tail_key","from_turn"].map(key=>Object.getOwnPropertyDescriptor(data,key));
    if(tail.some(Boolean)){
      require(tail.every(d=>d&&Object.hasOwn(d,"value")));
      owned.tail_turn=capture(tail[0].value,"uint",budget);owned.tail_key=capture(tail[1].value,"str",budget);owned.from_turn=capture(tail[2].value,"uint",budget);
      require(owned.tail_key===""||/^[0-9a-f]{64}$/.test(owned.tail_key));
    }
    return owned;
}
export function captureDetail(data, expectedScope, limits) {
  try {
    const owned=ownDetail(data,limits);
    validTime(owned.session.started); validTime(owned.session.updated);
    const body = captureConversation(owned, expectedScope);
    // The existing feed validator intentionally returns an identity-only session.
    return {...body, session:owned.session,...(Object.hasOwn(owned,"selection")?{selection:owned.selection}:{}),
      ...(Object.hasOwn(owned,"tail_key")?{tail_turn:owned.tail_turn,tail_key:owned.tail_key,from_turn:owned.from_turn}:{})};
  } catch { fail(); }
}

export function captureDetailResponse(data,expectedScope,limits,previous,request){
  try{
    if(field(data,"reset")===true)return captureDetail(data,expectedScope,limits);
    const tail=ownDetail(data,limits),base=captureDetail(previous,expectedScope,limits);
    const from=field(request,"fromTurn"),key=field(request,"fromKey");
    require(Number.isSafeInteger(from)&&from>=0&&typeof key==="string"&&/^[0-9a-f]{64}$/.test(key));
    require(tail.reset===false&&tail.from_turn===from&&base.tail_turn===from&&base.tail_key===key);
    require(tail.window_start===base.window_start&&tail.markup===base.markup&&tail.build===base.build&&tail.tail_turn>=from&&tail.later_hidden===0);
    const prefix=base.messages.filter(message=>message.turn<from);
    require(tail.messages.every(message=>message.turn>=from));
    // Re-admit the COMPLETE candidate, including retained raw text and arrays.
    // A small delta cannot bypass the ordinary complete-window allowances.
    return captureDetail({...tail,reset:true,from_turn:0,messages:prefix.concat(tail.messages)},expectedScope,limits);
  }catch{fail();}
}

const stateBase={build:"str",provider:"str",id:"str",selection:"str",match:"bool"};
const stateReady={...stateBase,cached:"bool",cached_turns:"uint",cached_updated:time,
  disk_changed:"bool",live:"str",working_known:"bool",working:"bool",elapsed_ms:"uint"};
export function captureState(data,expectedScope,limits){
  try{
    const match=field(data,"match");require(typeof match==="boolean");
    const out=capture(data,match?stateReady:{...stateBase,reason:"str"},credits(limits));
    require(["claude","codex","agy"].includes(out.provider)&&uuid.test(out.id)&&canonical32(out.selection));
    for(const key of ["provider","id","selection"])require(out[key]===field(expectedScope,key));
    if(!match){require(out.reason==="selection_changed");return out;}
    validTime(out.cached_updated);require(out.cached&&["unknown","live","ended"].includes(out.live));
    require(out.working_known||!out.working&&out.elapsed_ms===0);
    require(!out.working||out.working_known&&out.live==="live");
    require(out.working||out.elapsed_ms===0);
    require(out.provider!=="agy"||!out.working_known);
    return out;
  }catch{fail();}
}
