import {canonical32, credential, directory, encodeForm, fragment, origin} from "./auth_codec.js";
import {createAppHost} from "./app.js";
import {prepareTabRead2} from "./tab_proof.js";

const LINK = "Link invalid, expired or already used; run aiq ui again.";
const LOCKED = "This tab is locked; run aiq ui again.";
const UNAVAILABLE = "Navigator request unavailable";
const UNKNOWN = "This page uses memory only. A previously saved tab session could not be cleared or checked and may return on reload. Use your browser’s clear site data controls before restoring this tab.";
const MEMORY = "Memory-only tab session; reload requires aiq ui.";
const TERMINAL = "This navigator is no longer live; run aiq ui again.";
const owners = new WeakMap(); // DOM ownership only; no credential or fragment.
const apply = Reflect.apply;
const bad = () => { throw new TypeError(UNAVAILABLE); };
function own(value, key) {
  if (!value || typeof value !== "object" || Array.isArray(value)) bad();
  const d = Object.getOwnPropertyDescriptor(value,key);
  if (!d || !Object.hasOwn(d,"value")) bad();
  return d.value;
}
function integer(v,min,max) { if (!Number.isSafeInteger(v) || v < min || v > max) bad(); return v; }
function requestPath(kind, query) {
  if(kind==="long"){
    const dir=own(query,"dir"),history=own(query,"history");
    if(typeof history!=="boolean"||typeof dir!=="string"||dir===""&&!history)bad();
    return "/api/nav/long"+(dir===""?"":"?dir="+encodeForm(directory(dir))+"&history="+(history?"1":"0"));
  }
  if(kind==="directories"){
    const catalog=Object.getOwnPropertyDescriptor(query??{},"catalog");
    if(catalog&&(!Object.hasOwn(catalog,"value")||typeof catalog.value!=="boolean"))bad();
    return "/api/nav/directories"+(catalog?.value?"?catalog=1":"");
  }
  if (kind === "sessions") {
    const all=Object.getOwnPropertyDescriptor(query,"all");
    if(all&&(!Object.hasOwn(all,"value")||typeof all.value!=="boolean"))bad();
    if(all?.value){const workers=own(query,"workers");if(own(query,"dir")!==""||typeof workers!=="boolean")bad();return "/api/nav/sessions?all=1&workers="+(workers?"1":"0");}
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
  if (kind === "placement") {
    const id=own(query,"askId");
    if (typeof id!=="string" || !/^[1-9][0-9]{0,18}$/.test(id) || id.length===19 && id>"9223372036854775807") bad();
    return "/api/nav/asks/"+id+"/turn";
  }
  if (kind !== "conversation" && kind !== "state") bad();
  const provider = own(query,"provider"), id = own(query,"id");
  if (!["claude","codex","agy"].includes(provider) || typeof id !== "string" || !/^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/.test(id)) bad();
  if(kind==="state"){
    const selection=own(query,"selection");if(!canonical32(selection))bad();
    return "/api/nav/sessions/"+provider+"/"+id+"/state?selection="+selection;
  }
  const rawLast=own(query,"last"), last=rawLast==="all"?"all":integer(rawLast,1,10000), markup = own(query,"markup"), anchor = integer(own(query,"anchor"),0,9999999999999);
  if (typeof markup !== "boolean") bad();
  if (last==="all"&&anchor!==0)bad();
  const from=Object.getOwnPropertyDescriptor(query,"fromTurn"),key=Object.getOwnPropertyDescriptor(query,"fromKey");let delta="";
  if(from||key){
    if(!from||!key||!Object.hasOwn(from,"value")||!Object.hasOwn(key,"value")||anchor!==0||typeof key.value!=="string"||!/^[0-9a-f]{64}$/.test(key.value))bad();
    delta="from_key="+key.value+"&from_turn="+integer(from.value,0,Number.MAX_SAFE_INTEGER)+"&";
  }
  return "/api/nav/sessions/"+provider+"/"+id+"?"+(anchor===0?"":"at="+anchor+"&")+delta+"last="+last+"&markup="+(markup?"1":"0");
}

// No global session and no exposed bearer. The host must supply finite immutable
// response allowances before any data reader is usable. Every operation owns
// its physical fetch/body lifetime and retains its original session epoch.
export function createTabSession(options) {
  const base = origin(options.base), build = options.build;
  if (typeof build !== "string" || !/^[A-Za-z0-9._+-]{1,128}$/.test(build)) bad();
  const isCurrent = options.isCurrent, onState = options.onState ?? (() => {}), onReadFailure = options.onReadFailure ?? (() => {});
  const fetcher = options.fetcher ?? globalThis.fetch.bind(globalThis);
  const getStorage = options.getStorage ?? (() => globalThis.sessionStorage);
  const now = options.now ?? (() => performance.now());
  const setTimer = options.setTimer ?? setTimeout, clearTimer = options.clearTimer ?? clearTimeout;
  if (![isCurrent,onState,onReadFailure,fetcher,getStorage,now,setTimer,clearTimer].every(v => typeof v === "function")) bad();
  const signal = options.signal, key = "aiq.navigator.v1:"+build, limits = Object.create(null);
  if (options.limits !== undefined) {
    for (const kind of ["sessions","conversation","asks"]) limits[kind] = integer(own(options.limits,kind),1,Number.MAX_SAFE_INTEGER);
    limits.directories=limits.sessions;
    limits.long=limits.sessions;
    limits.state=limits.conversation;
    const placement=Object.getOwnPropertyDescriptor(options.limits,"placement");
    if (placement) { if(!Object.hasOwn(placement,"value"))bad();limits.placement=integer(placement.value,1,Number.MAX_SAFE_INTEGER); }
  }
  let input = options.fragment, epoch = {}, token = null, disposed = false, kind = "locked", message = LOCKED, notice = "", selected = null;
  let clearing = false, deferredDispose = false, hostCleanup = null;
  const failures = new WeakMap();
  function failure(category) {
    const error = new TypeError(UNAVAILABLE);failures.set(error,category);return error;
  }
  const pending = new Set();
  const owned = e => {
    if (epoch !== e || signal?.aborted) return false;
    try { return isCurrent() && epoch === e && !signal?.aborted; }
    catch { return false; }
  };
  const live = e => !disposed && owned(e) && !disposed && epoch === e;
  const snapshot = () => Object.freeze({kind,message,notice});
  function emit() { if (!owned(epoch)) return; const value=snapshot(); try { onState(value); } catch { /* A host failure cannot regain authority. */ } }
  function retire(text, final = false, phase = "locked") {
    const next = {}; epoch = next; token = null; selected = null; kind = final ? "disposed" : phase; message = text; notice = "";
    if (final) disposed = true;
    const old = [...pending], cleanup = hostCleanup; pending.clear();hostCleanup = null;
    // Detach the captured sensitive host before abort listeners can reenter.
    try { cleanup?.(); } catch { /* Independent physical retirement continues. */ }
    for (const op of old) { try { op.abort(); } catch { /* Continue independent cleanup. */ } }
    return next;
  }
  function attachHost(cleanup) {
    const e = epoch;
    if (typeof cleanup !== "function" || hostCleanup !== null || !["unlocked","restoring"].includes(kind) || !live(e)) return false;
    if (hostCleanup !== null || epoch !== e || !["unlocked","restoring"].includes(kind)) return false;
    hostCleanup = cleanup;return true;
  }
  function storageCall(e, method, ...args) {
    if (!live(e)) return {ok:false,stale:true};
    try {
      const storage=getStorage();
      if (!live(e)) return {ok:false,stale:true};
      const action=storage[method];
      if (!live(e)) return {ok:false,stale:true};
      if (typeof action !== "function") throw new TypeError(UNAVAILABLE);
      const value=apply(action,storage,args);
      return live(e)?{ok:true,value}:{ok:false,stale:true};
    }
    catch { return {ok:false,stale:!live(e)}; }
  }
  function clearStored(e) {
    storageCall(e,"removeItem",key);
    let read = storageCall(e,"getItem",key);
    if (read.stale) return false;
    if (read.ok && (read.value === null || read.value === "")) return true;
    storageCall(e,"setItem",key,"");
    read = storageCall(e,"getItem",key);
    return !read.stale && read.ok && (read.value === null || read.value === "");
  }
  async function send(e,path,method,body,byteLimit,bearer,parent,decode,condition="") {
    if (!live(e)) throw failure("cancelled");
    const controller = new AbortController(), started=now();
    const valid = () => {
      try { const elapsed=now()-started;return live(e) && !controller.signal.aborted && elapsed >= 0 && elapsed < 10000; }
      catch { return false; }
    };
    let response=null, reader=null, complete=false, timer=null, relay=null, rejected=null;
    pending.add(controller);
    try {
      if (parent?.aborted) throw failure("cancelled");
      if (parent) { relay=()=>controller.abort();parent.addEventListener("abort",relay,{once:true}); }
      timer=setTimer(()=>controller.abort(),10000);
      if (!valid()) bad();
      const headers = method === "POST" ? {"Content-Type":"application/json"} : {};
      let proof=null;
      if (bearer !== null) {
        proof=await prepareTabRead2(bearer,base,build,path,condition,valid,options.crypto ?? globalThis.crypto);bearer=null;
        if(!valid())bad();headers.Authorization=proof.authorization;
        if(condition)headers["If-None-Match"]=condition;
      }
      response=await fetcher(base+path,{method,headers,body,signal:controller.signal,redirect:"error",credentials:"omit",cache:"no-store"});
      const status = response.status;
      if (status === 401 && live(e)) {
        lockAndClear(false); // BEFORE error body or build interpretation.
        throw failure("cancelled");
      }
      if (!valid()) throw failure(parent?.aborted || !live(e) ? "cancelled" : "terminal");
      const notModified=status===304,mime=response.headers.get("Content-Type"),etag=response.headers.get("ETag")??"";
      if (response.headers.get("X-Aiq-Build") !== build || response.headers.has("Content-Encoding"))bad();
      if(notModified){if(!proof||!condition||mime!==null)bad();if(response.body===null)complete=true;}
      else if(!/^application\/json(?:;\s*charset=utf-8)?$/i.test(mime??"")||!response.body)bad();
      if (status !== 200 && !notModified && !(proof && [400,404,409,413,500,503].includes(status))) throw failure("terminal");
      if(response.body!==null)reader=response.body.getReader();
      const chunks=[];let total=0;
      while(reader) {
        const next=await reader.read();
        if (!valid()) bad();
        if (next.done) { complete=true;break; }
        if (!(next.value instanceof Uint8Array) || next.value.byteLength > (notModified?0:byteLimit)-total) bad();
        chunks.push(next.value.slice()); total+=next.value.byteLength;
      }
      const raw=new Uint8Array(total);let offset=0;
      for (const chunk of chunks) { raw.set(chunk,offset);offset+=chunk.byteLength; }
      if(proof) { await proof.verify(response.headers.get("X-Aiq-Tab-Proof"),status,mime??"",response.headers.get("X-Aiq-Build"),raw,etag);if(!valid())bad(); }
      if(status!==200&&!notModified)throw failure("request");
      const value=decode(raw,{status,etag});
      if (!valid()) bad();
      return value;
    } catch (error) {
      rejected = failure(failures.get(error) ?? (parent?.aborted || !live(e) ? "cancelled" : "terminal"));
      throw rejected;
    }
    finally {
      const validBeforeCleanup = valid();
      let cleanupFailed = false;
      try { if (timer !== null) clearTimer(timer); } catch { cleanupFailed = true; }
      try { if (parent && relay) parent.removeEventListener("abort",relay); } catch { cleanupFailed = true; }
      if (!complete) {
        try { controller.abort(); } catch { /* Reader cleanup remains owned. */ }
        try { if(reader)await reader.cancel();else if(response?.body)await response.body.cancel(); } catch { /* Native cancellation failures remain fixed. */ }
      }
      try { reader?.releaseLock(); } catch { cleanupFailed = true; }
      pending.delete(controller);
      if (cleanupFailed) throw failure("terminal");
      // Our own cancellation during failed-body cleanup is not a new failure.
      if (!rejected && (!validBeforeCleanup || !live(e))) throw failure(parent?.aborted || !live(e) ? "cancelled" : "terminal");
    }
  }
  function dispose() {
    if (disposed) return;
    if (clearing) { deferredDispose=true;return; }
    try { signal?.removeEventListener("abort",dispose); } catch { /* Host/physical cleanup must continue. */ }
    input=null;const outcome=notice;retire(LOCKED,true);notice=outcome;emit();
  }
  function lockAndClear(explicit) {
    if (clearing || disposed) return;
    clearing=true;
    try {
      if (!live(epoch)) return;
      const e=retire(LOCKED), cleared=clearStored(e);
      if (!live(e)) return;
      notice=explicit?(cleared?"Saved session removed from this tab.":"This page is locked, but saved tab state could not be cleared; it may return on reload. Use your browser’s clear site data controls."):(cleared?"":UNKNOWN);
      emit(); // Storage outcome is complete; reentrant Forget is now a no-op.
    } finally {
      clearing=false;
      if (deferredDispose) { deferredDispose=false;dispose(); }
    }
  }
  function forget() { lockAndClear(true); }
  async function begin() {
    const e=epoch;
    try {
      if (!live(e)) return false;
      let parsed=fragment(input);input=null;selected=parsed.next;
      if (parsed.code === null) {
        const stored=storageCall(e,"getItem",key);
        if (stored.stale) bad();
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
      const written=storageCall(e,"setItem",key,value);
      if (written.stale) bad();
      const saved=storageCall(e,"getItem",key);
      if (saved.stale) bad();
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
    const e=epoch;
    try {
      if (!live(e) || !["unlocked","restoring"].includes(kind) || token === null || limits[route] === undefined) throw failure("cancelled");
      let path;
      try { path=requestPath(route,query); } catch { throw failure("request"); }
      if (!live(e)) throw failure("cancelled");
      let condition="";
      try{if(route==="conversation"){
        const d=Object.getOwnPropertyDescriptor(query,"etag");if(d){if(!Object.hasOwn(d,"value")||typeof d.value!=="string"||!/^"[0-9a-f]{64}"$/.test(d.value))bad();condition=d.value;}
      }}catch{throw failure("request");}
      const result=await send(e,path,"GET",undefined,limits[route],token,abortSignal,(raw,meta)=>{
        if(meta.status===304)return {notModified:true,etag:meta.etag};
        const data=JSON.parse(new TextDecoder("utf-8",{fatal:true,ignoreBOM:true}).decode(raw));
        return route==="conversation"&&meta.etag?{data,etag:meta.etag}:{data};
      },condition);
      if (!live(e)) bad();
      if (kind === "restoring") { kind="unlocked";message="Navigator unlocked.";emit();if(!live(e))bad(); }
      return result;
    } catch (error) {
      const category=failures.get(error) ?? "terminal";
      if (live(e)) {
        if (category === "terminal") {
          const next=retire(TERMINAL,false,"terminal");
          if (live(next)) emit();
        }
        // Only a code-owned enum crosses this boundary, never a raw error.
        if (category !== "terminal" && live(e)) { try { onReadFailure(category); } catch { /* No new authority. */ } }
      }
      throw new TypeError(UNAVAILABLE);
    }
  }
  signal?.addEventListener("abort",dispose,{once:true});
  if (!live(epoch)) dispose();
  const ready=disposed?Promise.resolve(false):begin();
  return Object.freeze({ready,snapshot,forget,dispose,attachHost,
    selection:()=>["unlocked","restoring"].includes(kind)&&live(epoch)?selected:null,
    directories:(q,s)=>read("directories",q,s),long:(q,s)=>read("long",q,s),sessions:(q,s)=>read("sessions",q,s),conversation:(q,s)=>read("conversation",q,s),state:(q,s)=>read("state",q,s),asks:(q,s)=>read("asks",q,s),placement:(q,s)=>read("placement",q,s)});
}

// The only production DOM host. All body nodes are static and precede the
// first classic script; this module itself has no initialization side effects.
export function start(options) {
  const {window,document,build,signal,isCurrent}=options;
  let input=options.fragment;options=null;
  if (!window || document?.defaultView !== window || typeof isCurrent !== "function") bad();
  const root=document.getElementById("nav-auth-root"), heading=document.getElementById("nav-auth-heading"),
    status=document.getElementById("nav-auth-status"), storage=document.getElementById("nav-auth-storage"), button=document.getElementById("nav-auth-forget");
  if (![root,heading,status,storage,button].every(Boolean)) bad();
  const owner={};owners.set(root,owner);
  let session,host=null,disposed=false,disposing=false,pageLimits=null,retiredFocus=null;
  const current=()=>{
    if(disposed||signal?.aborted||owners.get(root)!==owner)return false;
    try{return Boolean(isCurrent())&&!disposed&&owners.get(root)===owner;}catch{return false;}
  };
  const onState=value=>{
    if (!current()) return;
    let focus=false;
    try { focus=document.hasFocus()&&(root.contains(document.activeElement)||retiredFocus&&(document.activeElement===retiredFocus||document.activeElement===document.body)); }catch{}
    if(!current())return;status.textContent=value.message;
    if(!current())return;storage.textContent=value.notice;
    if(!current())return;button.hidden=!["unlocked","restoring","terminal"].includes(value.kind);
    if(!current())return;
    if(["locked","terminal","disposed"].includes(value.kind)){retiredFocus=null;if(focus)heading.focus({preventScroll:true});}
  };
  const click=()=>session?.forget();button.addEventListener("click",click);
  try {
    const raw=document.documentElement?.dataset?.aiqLimits;
    if(typeof raw==="string"&&raw!==""){
      const parts=raw.split(":");
      if(parts.length!==5||parts.some(v=>!(/^[1-9][0-9]{0,15}$/).test(v)||!Number.isSafeInteger(Number(v))))bad();
      const [body,placement,arrayEntries,stringUnits,rows]=parts.map(Number);pageLimits={body,placement,arrayEntries,stringUnits,rows};
    }
    session=createTabSession({base:window.location.origin,build,signal,isCurrent:current,fragment:input,onState,
      limits:pageLimits?{sessions:pageLimits.body,conversation:pageLimits.body,asks:pageLimits.body,placement:pageLimits.placement}:undefined,
      fetcher:window.fetch.bind(window),getStorage:()=>window.sessionStorage});
  } catch { button.removeEventListener("click",click);if(owners.get(root)===owner)owners.delete(root);throw new TypeError(UNAVAILABLE); }
  finally {input=null;}
  // Empty metadata retains the standalone login host used by existing callers.
  // Production construction supplies positive policy; this module chooses none.
  if(pageLimits){
    const {arrayEntries,stringUnits,rows}=pageLimits;
    session.ready.then(async ready=>{
      if(!ready||!current()||!["unlocked","restoring"].includes(session.snapshot().kind))return;
      if(!current()||!["unlocked","restoring"].includes(session.snapshot().kind))return;
      // A separate reader session is unnecessary: all fixed capabilities below
      // use the original private credential and its epoch.
      const app=createAppHost(document,{window,root:document.getElementById("nav-app-root"),isCurrent:current,
        limits:{arrayEntries,stringUnits,rows},initialNext:session.selection()?.next,
        readers:{directories:session.directories,long:session.long,sessions:session.sessions,conversation:session.conversation,state:session.state,asks:session.asks,placement:session.placement}});
      if(!current()||!session.attachHost(()=>{
        const old=host;host=null;
        try{const focused=document.activeElement;if(document.hasFocus()&&document.getElementById("nav-app-root")?.contains(focused))retiredFocus=focused;}catch{}
        old?.dispose();
      })){app.dispose();return;}
      host=app;
      if(!current()||!app.mount()){host=null;app.dispose();session.dispose();}
    }).catch(()=>{if(current()){const old=host;host=null;old?.dispose();session.dispose();}});
  }
  const dispose=()=>{
    if(disposed||disposing)return;disposing=true;
    try{button.removeEventListener("click",click);}catch{}
    try{session.dispose();}finally{
      disposed=true;disposing=false;const old=host;host=null;try{old?.dispose();}catch{}
      if(owners.get(root)===owner)owners.delete(root);
    }
  };
  return Object.freeze({dispose});
}
