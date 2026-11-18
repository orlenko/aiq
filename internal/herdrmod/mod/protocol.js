export const MAX_REPLY = 4194304;
const COMMANDS=["hello","asks","ask-detail","ask-action","target","quotas","sessions","conversation","ask-context"];
const PROVIDERS = new Set(["claude","codex","agy"]);
const ERRORS = new Set(["busy","unavailable","not_set_up","too_large","invalid_request","not_found","conflict","unknown_outcome","preparing","internal"]);
const HEX64 = /^[a-f0-9]{64}$/;
const ID = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/;
const object = x => !!x && typeof x === "object" && !Array.isArray(x);
const str = (x,n=262144) => typeof x === "string" && x.length <= n;
export class BridgeError extends Error { constructor(code) { super(code); this.code=code; } }
function require(ok) { if (!ok) throw new BridgeError("internal"); }
function messages(rows) {
  require(Array.isArray(rows) && rows.length <= 40);
  for (const row of rows) require(object(row) && str(row.key,512) && str(row.text) && ["user","assistant","system","tool"].includes(row.role) && Number.isSafeInteger(row.at_ms));
}
export function decode(stdout, kind, expectedHost=null) {
  require(typeof stdout === "string" && stdout.length <= MAX_REPLY+1 && utf8Bytes(stdout) <= MAX_REPLY+1);
  let e; try { e=JSON.parse(stdout); } catch { throw new BridgeError("internal"); }
  require(object(e) && e.schema === 1 && e.kind === kind && typeof e.ok === "boolean" && object(e.host) && /^[a-f0-9]{32}$/.test(e.host.id) && str(e.host.name,512) && Number.isSafeInteger(e.generated_at_ms));
  if (expectedHost && e.host.id !== expectedHost) throw new BridgeError("source_changed");
  if (!e.ok) { require(object(e.error) && ERRORS.has(e.error.code)); throw new BridgeError(e.error.code); }
  require(object(e.data) && typeof e.partial === "boolean");
  const d=e.data;
  if (kind === "asks") {
    require(["ready","not_set_up"].includes(d.setup) && Array.isArray(d.rows) && d.rows.length<=10000 && object(d.counts));
    for (const r of d.rows) {
      require(object(r));
      if(object(r.latest)&&r.latest.preview===undefined)r.latest.preview="";
      if(object(r.first)&&r.first.preview===undefined)r.first.preview="";
      require(object(r) && /^\d+$/.test(r.id) && HEX64.test(r.revision) && PROVIDERS.has(r.provider) && str(r.conversation_id,128) && str(r.workspace,8192) && object(r.latest) && str(r.latest.preview,2048) && object(r.first) && str(r.first.preview,2048) && ["open","probably-done","dismissed","closed"].includes(r.state) && Number.isSafeInteger(r.raised_at_ms));
    }
  } else if (["ask-detail","ask-context"].includes(kind)) {
    if(object(d.latest)&&d.latest.text===undefined)d.latest.text="";
    if(object(d.first)&&d.first.text===undefined)d.first.text="";
    require(/^\d+$/.test(d.id) && HEX64.test(d.revision) && object(d.latest) && str(d.latest.text) && object(d.first) && str(d.first.text));
    if (kind === "ask-context") { messages(d.messages); require(object(d.placement)); }
  } else if (kind === "conversation") { messages(d.messages); }
  else if (kind === "sessions") {
    require(Array.isArray(d.rows) && d.rows.length<=10000 && object(d.window));
    for (const r of d.rows) require(object(r) && PROVIDERS.has(r.provider) && str(r.id,128) && str(r.cwd,8192) && str(r.name,2048) && Number.isSafeInteger(r.updated_ms));
  } else if (kind === "quotas") {
    require(Array.isArray(d.accounts) && d.accounts.length<=10000);
    for (const a of d.accounts) {
      require(object(a) && str(a.id,512) && (PROVIDERS.has(a.provider)||a.provider==="copilot") && Array.isArray(a.windows) && a.windows.length<=128 && Array.isArray(a.leases) && a.leases.length<=10000);
      if(a.observed_at_ms===undefined)a.observed_at_ms=wireSeconds(a.observed_at);
      a.reset_credit_expires_at_ms=wireSeconds(a.reset_credit_expires_at??null);
      for(const w of a.windows) {
        require(object(w) && str(w.label,512) && (w.used_pct===null || typeof w.used_pct === "number" && Number.isFinite(w.used_pct)));
        if(w.resets_at_ms===undefined)w.resets_at_ms=wireSeconds(w.resets_at);
      }
    }
  } else if (kind === "ask-action") require(/^\d+$/.test(d.id) && HEX64.test(d.revision) && ["open","probably-done","dismissed","closed"].includes(d.state));
  else if(kind === "target") {
    require(d.terminal===null || validTarget(d.terminal));
  }
  return e;
}
function wireSeconds(value) { require(value===null || Number.isSafeInteger(value)&&Number.isSafeInteger(value*1000));return value===null?0:value*1000; }
// QuickJS and Node agree on this without TextEncoder or a browser global.
export function utf8Bytes(s) { let n=0; for(const ch of s) { const c=ch.codePointAt(0); n+=c<128?1:c<2048?2:c<65536?3:4; } return n; }
export function argsFor(kind, options={}) {
  const out=["__app",kind];
  const push=(key,value,valid)=>{ if(!valid) throw new BridgeError("invalid_request"); out.push("--"+key,String(value)); };
  if (["hello","asks","quotas"].includes(kind)) return out;
  if(kind==="target") {
    if(options.revision) {push("ask",options.id,/^\d+$/.test(options.id));push("revision",options.revision,HEX64.test(options.revision));}
    else if(options.lease_id && !options.id)push("lease",options.lease_id,/^[1-9][0-9]{0,17}$/.test(options.lease_id));
    else {push("provider",options.provider,PROVIDERS.has(options.provider));push("conversation",options.id,ID.test(options.id));}
    return out;
  }
  if (["ask-detail","ask-context","ask-action"].includes(kind)) {
    push("id",options.id,/^\d+$/.test(options.id));
    if(kind!=="ask-detail")push("revision",options.revision,HEX64.test(options.revision));
    if(kind==="ask-action") { push("action",options.action,["dismiss","restore"].includes(options.action)); push("host",options.host,/^[a-f0-9]{32}$/.test(options.host)); }
    return out;
  }
  if (kind === "sessions") {
    if(options.before) push("before",options.before,str(options.before,512)&&/^[a-zA-Z0-9._:-]+$/.test(options.before));
    return out;
  }
  if(kind === "conversation") {
    push("provider",options.provider,PROVIDERS.has(options.provider)); push("id",options.id,ID.test(options.id));
    for(const key of ["before","after"]) if(options[key]) push(key,options[key],str(options[key],512)&&/^[a-zA-Z0-9._:-]+$/.test(options[key]));
    if(options.anchor)push("anchor",options.anchor,/^m[0-9]+-[0-9]+-[a-f0-9]{16}$/.test(options.anchor));
    else if(options.at) push("at",options.at,Number.isSafeInteger(options.at)&&options.at>0);
    return out;
  }
  throw new BridgeError("invalid_request");
}
export function validTarget(t) {
  if (!object(t) || !["origin","workspace_current"].includes(t.relation) ||
      !/^t[1-9][0-9bcdfghjkmnpqrstvwxyz]*$/.test(t.address) || !str(t.core_id,128) || !object(t.witness)) return false;
  const w=t.witness;
  if (!Number.isSafeInteger(w.terminal_pid) || w.terminal_pid<=0) return false;
  if (w.terminal_created_ms !== undefined) {
    return t.relation==="workspace_current" && w.label==="" &&
      Number.isSafeInteger(w.terminal_created_ms) && w.terminal_created_ms>0;
  }
  return /^[a-f0-9]{32}$/.test(w.label) || t.relation==="origin" && w.label==="";
}
export function matchesTarget(target,live,machine=null) {
  if (!validTarget(target) || !object(live) || live.state!=="live" ||
      live.pid!==target.witness.terminal_pid) return false;
  const address=machine ? machine+"/"+target.address : target.address;
  if (live.address!==target.address && live.address!==address) return false;
  const w=target.witness, label=live.labels?.["aiq.target"];
  if (w.terminal_created_ms) return live.created_ms===w.terminal_created_ms && label===undefined;
  return !w.label || label===w.label;
}
export class Bridge {
  constructor(exec) { this.exec=exec; this.sources=new Map(); this.active=0; this.waiters=[]; }
  async slot(fn) {
    if(this.waiters.length>=64) throw new BridgeError("busy");
    if(this.active>=4) await new Promise(resolve=>this.waiters.push(resolve));
    else this.active++;
    try { return await fn(); } finally { const next=this.waiters.shift();if(next)next();else this.active--; }
  }
  source(machine) { const key=machine??""; if(!this.sources.has(key)) this.sources.set(key,{machine,path:"aiq",host:null,hello:null}); return this.sources.get(key); }
  async hello(source) {
    if(source.host) return source.host;
    if(source.hello) return source.hello;
    source.hello=this.slot(async()=>{
      let r;
      try { r=await this.exec(source.path,["__app","hello"],{on:source.machine,timeout:12000}); }
      catch {
        // Fixed read-only discovery handles remote GUI services with a small PATH.
        let p; try { p=await this.exec("sh",["-lc","command -v aiq"],{on:source.machine,timeout:8000}); } catch { throw new BridgeError("unavailable"); }
        const path=p.stdout?.trim();
        if(p.code!==0 || !str(path,4096) || !path.startsWith("/") || /[\r\n\0]/.test(path)) throw new BridgeError("not_installed");
        source.path=path;
        try { r=await this.exec(path,["__app","hello"],{on:source.machine,timeout:12000}); } catch { throw new BridgeError("unavailable"); }
      }
      if(r.code!==0) throw new BridgeError(r.code===2?"update_required":"unavailable");
      const e=decode(r.stdout,"hello");
      if(!Array.isArray(e.data.commands)||COMMANDS.some(command=>!e.data.commands.includes(command)))throw new BridgeError("update_required");
      source.host=e.host.id; return source.host;
    });
    try{return await source.hello;} finally{source.hello=null;}
  }
  async read(machine,kind,options={},current=()=>true) {
    if(!current())throw new BridgeError("superseded");
    const source=this.source(machine); await this.hello(source);
    if(options.host && options.host!==source.host)throw new BridgeError("source_changed");
    return this.slot(async()=>{ if(!current())throw new BridgeError("superseded");let r; try { r=await this.exec(source.path,argsFor(kind,options),{on:machine,timeout:12000}); } catch { throw new BridgeError("unavailable"); }
      if(r.code!==0) throw new BridgeError("unavailable");
      try{return decode(r.stdout,kind,source.host);}catch(e){if(e.code==="source_changed")source.host=null;throw e;}
    });
  }
  // A mutation is attempted once. The caller resolves uncertainty by reading,
  // never by discovering a new binary, re-handshaking or retrying the action.
  async action(machine,row,action) {
    const source=this.source(machine);
    if(!source.host || source.host!==row.host) throw new BridgeError("source_changed");
    return this.slot(async()=>{let r;try{r=await this.exec(source.path,argsFor("ask-action",{...row,action}),{on:machine,timeout:9000});}catch{throw new BridgeError("unknown_outcome");}
      if(r.code!==0) throw new BridgeError("unknown_outcome"); return decode(r.stdout,"ask-action",row.host);
    });
  }
}
