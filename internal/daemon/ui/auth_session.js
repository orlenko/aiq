import {canonical32, credential, directory, encodeForm, fragment, origin} from "./auth_codec.js";

const LINK = "Link invalid, expired or already used; run aiq ui again.";
const LOCKED = "This tab is locked; run aiq ui again.";
const UNAVAILABLE = "Navigator request unavailable";
const UNKNOWN = "This page uses memory only. A previously saved tab session could not be cleared or checked and may return on reload. Use your browser’s clear site data controls before restoring this tab.";
const MEMORY = "Memory-only tab session; reload requires aiq ui.";
const owners = new WeakMap(); // DOM ownership only; no credential or fragment.
const bad = () => { throw new TypeError(UNAVAILABLE); };
function own(value, key) {
  if (!value || typeof value !== "object" || Array.isArray(value)) bad();
  const d = Object.getOwnPropertyDescriptor(value,key);
  if (!d || !Object.hasOwn(d,"value")) bad();
  return d.value;
}
function integer(v,min,max) { if (!Number.isSafeInteger(v) || v < min || v > max) bad(); return v; }
function requestPath(kind, query) {
  if (kind === "sessions") {
    const dir = directory(own(query,"dir")), workers = own(query,"workers");
    if (typeof workers !== "boolean") bad();
    return "/api/nav/sessions?dir="+encodeForm(dir)+"&workers="+(workers?"1":"0");
  }
  if (kind === "asks") {
    const mode = own(query,"mode");
    if (mode === "all") return "/api/nav/asks";
    if (mode !== "directory") bad();
    return "/api/nav/asks?dir="+encodeForm(directory(own(query,"dir")));
  }
  if (kind !== "conversation") bad();
  const provider = own(query,"provider"), id = own(query,"id");
  if (!["claude","codex","agy"].includes(provider) || typeof id !== "string" || !/^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/.test(id)) bad();
  const last = integer(own(query,"last"),1,10000), markup = own(query,"markup"), at = own(query,"at");
  if (typeof markup !== "boolean" || typeof at !== "string" || !/^(0|[1-9][0-9]{0,18})$/.test(at) || BigInt(at)>9223372036854775807n) bad();
  return "/api/nav/sessions/"+provider+"/"+id+"?at="+at+"&last="+last+"&markup="+(markup?"1":"0");
}

// No global session and no exposed bearer. The host must supply finite immutable
// response allowances before any data reader is usable. Every operation owns
// its physical fetch/body lifetime and retains its original session epoch.
export function createTabSession(options) {
  const base = origin(options.base), build = options.build;
  if (typeof build !== "string" || !/^[A-Za-z0-9._+-]{1,128}$/.test(build)) bad();
  const isCurrent = options.isCurrent, onState = options.onState ?? (() => {});
  const fetcher = options.fetcher ?? globalThis.fetch.bind(globalThis);
  const getStorage = options.getStorage ?? (() => globalThis.sessionStorage);
  const now = options.now ?? (() => performance.now());
  const setTimer = options.setTimer ?? setTimeout, clearTimer = options.clearTimer ?? clearTimeout;
  if (![isCurrent,onState,fetcher,getStorage,now,setTimer,clearTimer].every(v => typeof v === "function")) bad();
  const signal = options.signal, key = "aiq.navigator.v1:"+build, limits = Object.create(null);
  if (options.limits !== undefined) {
    for (const kind of ["sessions","conversation","asks"]) limits[kind] = integer(own(options.limits,kind),1,Number.MAX_SAFE_INTEGER);
  }
  let input = options.fragment, epoch = {}, token = null, disposed = false, kind = "locked", message = LOCKED, notice = "", selected = null;
  const pending = new Set();
  const live = e => !disposed && epoch === e && isCurrent() && !signal?.aborted;
  const snapshot = () => Object.freeze({kind,message,notice});
  function emit() { const value=snapshot(); try { onState(value); } catch { /* A host failure cannot regain authority. */ } }
  function retire(text, final = false) {
    const next = {}; epoch = next; token = null; selected = null; kind = final ? "disposed" : "locked"; message = text; notice = "";
    if (final) disposed = true;
    const old = [...pending]; pending.clear();
    for (const op of old) { try { op.abort(); } catch { /* Continue independent cleanup. */ } }
    emit();
    return next;
  }
  function storageCall(e, action) {
    if (!live(e)) return {ok:false,stale:true};
    try { const value=action(getStorage()); return live(e)?{ok:true,value}:{ok:false,stale:true}; }
    catch { return {ok:false,stale:!live(e)}; }
  }
  function clearStored(e) {
    storageCall(e, s => s.removeItem(key));
    let read = storageCall(e, s => s.getItem(key));
    if (read.stale) return false;
    if (read.ok && (read.value === null || read.value === "")) return true;
    storageCall(e, s => s.setItem(key,""));
    read = storageCall(e, s => s.getItem(key));
    return !read.stale && read.ok && (read.value === null || read.value === "");
  }
  async function send(e,path,method,body,byteLimit,bearer,parent,decode) {
    if (!live(e)) bad();
    const controller = new AbortController(), started=now();
    const valid = () => live(e) && !controller.signal.aborted && now()-started >= 0 && now()-started < 10000;
    let response=null, reader=null, complete=false, timer=null, relay=null;
    pending.add(controller);
    try {
      if (parent?.aborted) bad();
      if (parent) { relay=()=>controller.abort();parent.addEventListener("abort",relay,{once:true}); }
      timer=setTimer(()=>controller.abort(),10000);
      if (!valid()) bad();
      const headers = method === "POST" ? {"Content-Type":"application/json"} : {};
      if (bearer !== null) headers.Authorization="Bearer "+bearer;
      response=await fetcher(base+path,{method,headers,body,signal:controller.signal,redirect:"error",credentials:"omit",cache:"no-store"});
      if (!valid()) bad();
      if (response.status === 401) {
        const locked = retire(LOCKED); // BEFORE error body or build interpretation.
        const cleared = clearStored(locked);
        if (live(locked)) { notice = cleared ? "" : UNKNOWN; emit(); }
        bad();
      }
      if (response.status !== 200 || response.headers.get("X-Aiq-Build") !== build ||
          !/^application\/json(?:;\s*charset=utf-8)?$/i.test(response.headers.get("Content-Type") ?? "") || response.headers.has("Content-Encoding") || !response.body) bad();
      reader=response.body.getReader();
      const chunks=[];let total=0;
      for (;;) {
        const next=await reader.read();
        if (!valid()) bad();
        if (next.done) { complete=true;break; }
        if (!(next.value instanceof Uint8Array) || next.value.byteLength > byteLimit-total) bad();
        chunks.push(next.value.slice()); total+=next.value.byteLength;
      }
      const raw=new Uint8Array(total);let offset=0;
      for (const chunk of chunks) { raw.set(chunk,offset);offset+=chunk.byteLength; }
      const value=decode(raw);
      if (!valid()) bad();
      return value;
    } catch { throw new TypeError(UNAVAILABLE); }
    finally {
      let cleanupFailed = false;
      try { if (timer !== null) clearTimer(timer); } catch { cleanupFailed = true; }
      try { if (parent && relay) parent.removeEventListener("abort",relay); } catch { cleanupFailed = true; }
      if (!complete) {
        try { controller.abort(); } catch { /* Reader cleanup remains owned. */ }
        try { if(reader)await reader.cancel();else if(response?.body)await response.body.cancel(); } catch { /* Native cancellation failures remain fixed. */ }
      }
      try { reader?.releaseLock(); } catch { cleanupFailed = true; }
      pending.delete(controller);
      if (cleanupFailed || !valid()) throw new TypeError(UNAVAILABLE);
    }
  }
  function dispose() {
    if (disposed) return;
    signal?.removeEventListener("abort",dispose);
    input=null; retire(LOCKED,true);
  }
  function forget() {
    if (disposed || !isCurrent() || signal?.aborted) return;
    const e=retire(LOCKED), cleared=clearStored(e);
    if (!live(e)) return;
    notice=cleared?"Saved session removed from this tab.":"This page is locked, but saved tab state could not be cleared; it may return on reload. Use your browser’s clear site data controls.";
    emit();
  }
  async function begin() {
    const e=epoch;
    try {
      if (!live(e)) return false;
      let parsed=fragment(input);input=null;selected=parsed.next;
      if (parsed.code === null) {
        const stored=storageCall(e,s=>s.getItem(key));
        if (!live(e)) return false;
        if (!stored.ok || !canonical32(stored.value)) {
          notice=stored.ok?"":UNKNOWN;
          if (stored.ok && stored.value !== null && stored.value !== "") { clearStored(e);if(!live(e))return false; }
          emit();return false;
        }
        token=stored.value;kind="restoring";message="Saved tab session found; waiting for a successful request.";emit();return live(e);
      }
      const cleared=clearStored(e);
      if (!live(e)) return false;
      notice=cleared?"":UNKNOWN;kind="opening";message="Opening this tab…";emit();
      if (!live(e)) return false;
      let body=JSON.stringify({action:"exchange",code:parsed.code});parsed.code=null;
      let value;
      try { value=await send(e,"/api/nav/login","POST",body,65536,null,null,credential); }
      finally { body=null; }
      if (!live(e)) return false;
      storageCall(e,s=>s.setItem(key,value));
      const saved=storageCall(e,s=>s.getItem(key));
      if (!live(e)) return false;
      const persisted=saved.ok&&saved.value===value;
      notice=persisted?"":saved.ok&&(saved.value===null||saved.value==="")?MEMORY:UNKNOWN;
      token=value;value=null;kind="unlocked";message="Navigator unlocked.";emit();return live(e);
    } catch {
      input=null;
      if (live(e)) { token=null;selected=null;kind="locked";message=LINK;emit(); }
      return false;
    }
  }
  async function read(route,query,abortSignal) {
    try {
      const e=epoch;
      if (!live(e) || !["unlocked","restoring"].includes(kind) || token === null || limits[route] === undefined) bad();
      const path=requestPath(route,query);
      if (!live(e)) bad();
      const data=await send(e,path,"GET",undefined,limits[route],token,abortSignal,raw=>JSON.parse(new TextDecoder("utf-8",{fatal:true,ignoreBOM:true}).decode(raw)));
      if (!live(e)) bad();
      if (kind === "restoring") { kind="unlocked";message="Navigator unlocked.";emit();if(!live(e))bad(); }
      return {data};
    } catch { throw new TypeError(UNAVAILABLE); }
  }
  signal?.addEventListener("abort",dispose,{once:true});
  if (signal?.aborted || !isCurrent()) dispose();
  const ready=disposed?Promise.resolve(false):begin();
  return Object.freeze({ready,snapshot,forget,dispose,
    selection:()=>["unlocked","restoring"].includes(kind)&&live(epoch)?selected:null,
    sessions:(q,s)=>read("sessions",q,s),conversation:(q,s)=>read("conversation",q,s),asks:(q,s)=>read("asks",q,s)});
}

// The only production DOM host. All body nodes are static and precede the
// first classic script; this module itself has no initialization side effects.
export function start(options) {
  const {window,document,build,signal,isCurrent}=options;
  if (!window || document?.defaultView !== window || typeof isCurrent !== "function") bad();
  const root=document.getElementById("nav-auth-root"), heading=document.getElementById("nav-auth-heading"),
    status=document.getElementById("nav-auth-status"), storage=document.getElementById("nav-auth-storage"), button=document.getElementById("nav-auth-forget");
  if (![root,heading,status,storage,button].every(Boolean)) bad();
  const owner={};owners.set(root,owner);
  let session;
  const onState=value=>{
    if (owners.get(root)!==owner) return;
    const focus=document.hasFocus()&&root.contains(document.activeElement);
    status.textContent=value.message;storage.textContent=value.notice;button.hidden=!["unlocked","restoring"].includes(value.kind);
    if (value.kind==="locked"&&isCurrent()&&focus) heading.focus({preventScroll:true});
  };
  const click=()=>session?.forget();button.addEventListener("click",click);
  try {
    session=createTabSession({base:window.location.origin,build,signal,isCurrent,fragment:options.fragment,onState,
      fetcher:window.fetch.bind(window),getStorage:()=>window.sessionStorage});
  } catch { button.removeEventListener("click",click);if(owners.get(root)===owner)owners.delete(root);throw new TypeError(UNAVAILABLE); }
  const dispose=()=>{
    button.removeEventListener("click",click);session.dispose();
    if(owners.get(root)===owner)owners.delete(root);
  };
  return Object.freeze({dispose});
}
