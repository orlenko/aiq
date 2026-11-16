import {captureSessions, captureDetail, captureDetailResponse, captureState} from "./controller_data.js";
import {createConversationFeed} from "./feed.js";
import {directory,encodeForm} from "./auth_codec.js";

const fail = () => { throw new TypeError("could not configure session controller"); };
const uuid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
export function navigatorReadCommand(provider, id, follow = false) {
  if (!["claude", "codex", "agy"].includes(provider) || typeof id !== "string" || id.length!==36 || !uuid.test(id) ||
      typeof follow !== "boolean" || follow && provider === "agy") return null;
  return `aiq convo ${id}${follow ? " --follow" : ""}`;
}
export function navigatorResumeCommand(provider,id,variant="resume") {
  if(navigatorReadCommand(provider,id)===null)return null;
  const templates={resume:`aiq resume ${id}`,bare:`aiq resume --bare ${id}`,auto:`aiq run auto resume ${id}`,long:`aiq long auto resume ${id}`};
  return typeof variant==="string"&&Object.hasOwn(templates,variant)?templates[variant]:null;
}
export function navigatorLeaseCommand(verb,id,started) {
  if(!["attach","drain"].includes(verb)||typeof id!=="string"||typeof started!=="string"||
    id.length<1||id.length>10||started.length<1||started.length>12||/[^0-9]/.test(id)||/[^0-9]/.test(started)||Number(id)<=0||Number(started)<=0)return null;
  return `aiq long ${verb} ${id} --started ${started}`;
}
export function navigatorLauncherResumeCommand(provider,id,launcher){
  if(navigatorReadCommand(provider,id)===null||typeof launcher!=="string"||launcher.length<1||launcher.length>64||/[^a-z0-9._-]/.test(launcher)||!/^[a-z0-9]/.test(launcher))return null;
  return `aiq resume --launcher ${launcher} ${id}`;
}
function field(value, key) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail();
  const d = Object.getOwnPropertyDescriptor(value, key);
  if (!d || !Object.hasOwn(d, "value")) fail();
  return d.value;
}
function text(value) { if (typeof value !== "string") fail(); return value; }
function bool(value) { if (typeof value !== "boolean") fail(); return value; }
function integer(value, min, max) {
  if (!Number.isSafeInteger(value) || value < min || value > max) fail();
  return value;
}
function scope(value) {
  const provider = text(field(value, "provider")), id = text(field(value, "id"));
  if (!["claude", "codex", "agy"].includes(provider) || !uuid.test(id)) fail();
  return {provider, id};
}

// An unmounted host component: readers, query policy and limits are injected.
// Readers return a trusted promise/thenable of an inert {data: DTO} envelope.
export function createSessionController(document, options) {
  const {window, idPrefix} = options || {};
  let {readSessions, readConversation, copyText, announce} = options || {};
  const followOption=Object.getOwnPropertyDescriptor(options??{},"autoFollow");
  const autoFollow=followOption?.value??false;
  let readState=options?.readState,onNewest=options?.onNewest??(()=>{});
  if(followOption&&(!Object.hasOwn(followOption,"value")||typeof autoFollow!=="boolean")||autoFollow&&
    (typeof readState!=="function"||typeof onNewest!=="function"||typeof window?.setTimeout!=="function"||typeof window?.clearTimeout!=="function"))fail();
  const selectedCallback=Object.getOwnPropertyDescriptor(options ?? {},"onSelection");
  let onSelection=selectedCallback?.value ?? (()=>{});
  if (selectedCallback && (!Object.hasOwn(selectedCallback,"value") || typeof selectedCallback.value!=="function")) fail();
  const readyCallback=Object.getOwnPropertyDescriptor(options??{},"onDetailReady");
  let onDetailReady=readyCallback?.value??(()=>{});
  if(readyCallback&&(!Object.hasOwn(readyCallback,"value")||typeof onDetailReady!=="function"))fail();
  const readingOption=Object.getOwnPropertyDescriptor(options ?? {},"readingControls"), shortcutOption=Object.getOwnPropertyDescriptor(options ?? {},"shortcutsEnabled");
  const readingControls=readingOption ? readingOption.value : false;
  const prose=options.prose??false;if(typeof prose!=="boolean")fail();
  let shortcutsEnabled=shortcutOption ? shortcutOption.value : ()=>true;
  const commandOption=Object.getOwnPropertyDescriptor(options ?? {},"readCommands");
  const readCommands=commandOption ? commandOption.value : false;
  const resumeOption=Object.getOwnPropertyDescriptor(options??{},"resumeCommands"),resumeCommands=resumeOption?.value??false;
  if(resumeOption&&(!Object.hasOwn(resumeOption,"value")||typeof resumeCommands!=="boolean"))fail();
  if(commandOption&&(!Object.hasOwn(commandOption,"value")||typeof readCommands!=="boolean"))fail();
  if (readingOption && (!Object.hasOwn(readingOption,"value") || typeof readingControls!=="boolean") ||
      shortcutOption && (!Object.hasOwn(shortcutOption,"value") || typeof shortcutsEnabled!=="function")) fail();
  if (!window || window !== document.defaultView || typeof idPrefix !== "string" ||
      !/^[A-Za-z][A-Za-z0-9_-]*$/.test(idPrefix) ||
      ![readSessions, readConversation, copyText, announce].every(f => typeof f === "function")) fail();
  const suppliedLimits = field(options, "limits"), query = field(options, "detailQuery");
  const limits = {};
  for (const key of ["arrayEntries", "stringUnits", "rows"]) {
    limits[key] = integer(field(suppliedLimits, key), 1, Number.MAX_SAFE_INTEGER);
  }
  const rawLast=field(query,"last");
  const detailQuery = {last:rawLast==="all"?"all":integer(rawLast, 1, 10000),
    markup:bool(field(query, "markup")), anchor:integer(field(query, "anchor"), 0, 9999999999999)};
  if(detailQuery.last==="all"&&detailQuery.anchor!==0)fail();
  const fragment = document.createDocumentFragment();
  const root = node("div", "nav-controller"); fragment.append(root);
  const listPane = pane("Sessions", "sessions"), detailPane = pane("Conversation", "conversation");
  const conversationOnly=options?.conversationOnly??false;if(typeof conversationOnly!=="boolean")fail();
  listPane.root.hidden=conversationOnly;
  detailPane.root.hidden=!conversationOnly;
  root.setAttribute("data-reading",String(conversationOnly));root.setAttribute("data-conversation-only",String(conversationOnly));
  root.append(listPane.root, detailPane.root);
  let mounted = false, disposed = false, frozen=false, activeScope = null, listQuery = null, selection = null, listView = null;
  const current = {list:null, detail:null}, serial = {list:0n, detail:0n}, removes = [];
  let railVisible=!(window.matchMedia?.("(max-width: 48rem)")?.matches??false);
  const railToggle=node("button","nav-session-rail-toggle","Hide sessions");railToggle.type="button";
  if(prose&&!conversationOnly){const toolbar=node("div","nav-controller-pane-toolbar");toolbar.append(detailPane.heading,railToggle,detailPane.refresh);detailPane.root.prepend(toolbar);const toggle=()=>{if(!mounted||disposed)return;railVisible=!railVisible;syncRail();};railToggle.addEventListener("click",toggle);removes.push(()=>railToggle.removeEventListener("click",toggle));}
  function syncRail(){if(!prose||conversationOnly)return;const reading=root.getAttribute("data-reading")==="true";listPane.root.hidden=reading&&!railVisible;root.setAttribute("data-rail-hidden",String(reading&&!railVisible));railToggle.textContent=railVisible?"Hide sessions":"Show sessions";railToggle.setAttribute("aria-expanded",String(railVisible));}
  function node(tag, className, value) {
    const n = document.createElement(tag); n.className = className;
    if (value !== undefined) n.textContent = value;
    return n;
  }
  function pane(label, suffix) {
    const section = node("section", "nav-controller-pane");
    const heading = node("h2", "nav-controller-heading", label);
    heading.id = `${idPrefix}-${suffix}`; heading.setAttribute("tabindex", "-1");
    section.setAttribute("aria-labelledby", heading.id);
    const refresh = node("button", "nav-controller-refresh", `Refresh ${suffix}`);
    refresh.type = "button";
    const status = node("p", "nav-controller-status", ""); status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    const content = node("div", "nav-controller-content");
    section.append(heading, refresh, status, content);
    return {root:section, heading, refresh, status, content};
  }
  function alive(op) {
    return Boolean(op) && mounted && !disposed && !op.retired && current[op.channel] === op && op.scope === activeScope;
  }
  function cancelFocus(op) {
    if (op?.intent) { op.intent.remove(); op.intent = null; }
  }
  function cleanCandidate(candidate) {
    if (!candidate) return;
    candidate.retired = true; candidate.root.remove();
    // A paused activation may install more resources after reentry returns.
    if (candidate.busy || candidate.cleaned) return;
    candidate.cleaned = true;
    for(const remove of candidate.removes ?? [])remove();
    if(candidate.removes)candidate.removes.length=0;
    try { candidate.child?.teardown(); } catch { /* Trusted cleanup failure stays local. */ }
    candidate.child = null;
  }
  function retireMany(...operations) {
    const old = operations.filter(Boolean).map(op => {
      op.retired = true;
      op.baseline=null;
      if(op.timer!==undefined){window.clearTimeout(op.timer);op.timer=undefined;}
      if (op.candidate) op.candidate.retired = true;
      const abort = op.abort; op.abort = null;
      return {op, abort};
    });
    // Detach BOTH channels before any cleanup or abort can synchronously reenter.
    for (const {op} of old) { op.candidate?.root.remove(); cancelFocus(op); }
    for (const {op, abort} of old) {
      cleanCandidate(op.candidate);
      try { abort?.abort(); } catch { /* Only captured old resources are retired. */ }
    }
  }
  function retire(op) { retireMany(op); }
  function reserve(channel) {
    const previous = current[channel];
    const op = {channel, version:++serial[channel], scope:activeScope, retired:false, abort:null,
      candidate:null, intent:null};
    current[channel] = op;
    return {op, previous};
  }
  function status(op, pane, value, busy) {
    if (!alive(op)) return;
    pane.status.textContent = value;
    pane.root.setAttribute("aria-busy", busy ? "true" : "false");
    try { announce(value); } catch { /* Fixed status sinks do not control requests. */ }
  }
  function clearList() {
    if (listView) {
      if (current.detail?.intent?.view === listView) cancelFocus(current.detail);
      if (selection) selection.row = null;
      for (const remove of listView.removes) remove();
      listView.root.remove(); listView = null;
    }
  }
  function markers() {
    for (const item of listView?.rows || []) {
      if (!item.button) continue;
      if (selection?.row === item) item.button.setAttribute("aria-current", "true");
      else item.button.removeAttribute("aria-current");
    }
  }
  function commandControls(parent, value, current, cleanup) {
    if(!readCommands&&!resumeCommands)return;
    const group=node("div","nav-read-commands"), buttons=[];
    let busy=false;
    const commands=[];
    if(readCommands)for(const [follow,label]of [[false,"Copy read command"],[true,"Copy follow command"]])commands.push([navigatorReadCommand(value.provider,value.id,follow),label,"Read"]);
    if(resumeCommands)for(const [variant,label]of [["resume","Copy resume command"],["bare","Copy bare resume command"],["auto","Copy auto resume command"],["long","Copy long auto resume command"]])commands.push([navigatorResumeCommand(value.provider,value.id,variant),label,"Resume"]);
    if(resumeCommands&&value.activity?.launcher)commands.push([navigatorLauncherResumeCommand(value.provider,value.id,value.activity.launcher),"Copy recorded launcher resume command","Resume"]);
    for(const [command,label,kind]of commands) {
      if(command===null)continue;
      const button=node("button","nav-command-copy",label);button.type="button";button.title=command;buttons.push(button);group.append(button);
      const click=async()=>{
        if(!current()||busy)return;
        busy=true;for(const item of buttons)item.setAttribute("aria-disabled","true");
        try { await copyText(command); if(current())announce(kind+" command copied. Paste it in a terminal."); }
        catch { if(current())announce(kind+" command could not be copied."); }
        finally { busy=false;if(current())for(const item of buttons)item.removeAttribute("aria-disabled"); }
      };
      button.addEventListener("click",click);cleanup.push(()=>button.removeEventListener("click",click));
    }
    if(buttons.length){group.append(node("p","nav-controller-facts",`Terminal only · Directory: ${value.cwd}`));if(resumeCommands)group.append(node("p","nav-controller-facts","Resume may open another copy of a running session. Auto variants allow the agent to run without permission prompts."));parent.append(group);}
  }
  function listState(error, stale) {
    for(const target of [listPane.root,listPane.status]){
      if(error)target.setAttribute("data-error",error);else target.removeAttribute("data-error");
      target.setAttribute("data-stale",String(stale));
    }
    listPane.refresh.textContent=error?"Retry sessions":"Refresh sessions";
  }
  function failed(op, target, error) {
    if (!alive(op)) return;
    cleanCandidate(op.candidate); cancelFocus(op);
    let reason="",code="unavailable";
    if(op.channel==="list"){
      try{const d=Object.getOwnPropertyDescriptor(error,"code");if(d&&Object.hasOwn(d,"value")&&["request","timeout","cancelled","terminal"].includes(d.value))code=d.value;}catch{}
      const reasons={request:"The server could not complete the request.",timeout:"The request timed out.",cancelled:"The request was cancelled.",terminal:"The connection could not be verified. Reopen the UI to reconnect.",unavailable:"The request could not be completed."};
      reason=" "+reasons[code];
      try{const d=Object.getOwnPropertyDescriptor(error,"status");if(code==="request"&&d&&Object.hasOwn(d,"value")&&[400,404,409,413,500,503].includes(d.value))reason=` The server returned HTTP ${d.value}.`;}catch{}
      try{const d=Object.getOwnPropertyDescriptor(error,"reason"),messages={list_too_large:" The session list exceeds its size limit. Choose a project to narrow it.",list_busy:" The session index is busy. Try again.",request_cancelled:" The session scan was interrupted. Try again."};if(code==="request"&&d&&Object.hasOwn(d,"value")&&typeof d.value==="string"&&Object.hasOwn(messages,d.value))reason=messages[d.value];}catch{}
      listState(code,Boolean(listView));
    }
    status(op, target, op.channel === "list" ? (listView ? "Sessions unavailable; showing previous results." : "Sessions unavailable.")+reason :
      "Conversation unavailable.", false);
  }
  function read(op, reader, request, target, commit) {
    if (!alive(op)) return;
    let promise;
    try {
      op.abort = new window.AbortController();
      if (!alive(op)) { retire(op); return; }
      const signal = op.abort.signal;
      if (!alive(op)) { retire(op); return; }
      const result = reader(request, signal);
      // Read/invoke the trusted outer then exactly once. Never resolve a DTO.
      const then = result?.then;
      if (typeof then !== "function") throw new TypeError("reader result");
      promise = new Promise((resolve, reject) => Reflect.apply(then, result, [resolve, reject]));
    } catch(error) { failed(op, target,error); return; }
    // Even synchronous reader/thenable reentry must consume the returned rejection.
    promise.then(envelope => {
      if (!alive(op)||frozen) return;
      try {
        const data = field(envelope, "data");
        const owned = op.channel === "list" ? captureSessions(data, op.query.canonicalDir, limits,op.query.all) : captureDetail(data, op.query, limits);
        if (op.channel === "detail" && owned.markup !== detailQuery.markup) throw new TypeError("response mode");
        if(op.channel==="detail")op.etag=readETag(envelope);
        if (alive(op)) commit(op, owned);
      } catch { failed(op, target); }
    }, error => failed(op, target,error));
  }
  function incompleteList(op,busy){
    if(!alive(op)||frozen)return;
    listState("incomplete",Boolean(listView));
    status(op,listPane,`Session scan incomplete${listView?"; showing previous results":""}. ${busy?"Preparing the index; retrying shortly…":"Refresh sessions to try again."}`,busy);
  }
  function listReceived(op,data){
    if(!alive(op)||frozen)return;
    if(!data.partial||data.sessions.length){listCommit(op,data);return;}
    // An interrupted empty scan cannot establish that known sessions vanished.
    // Only live unscanned responses get bounded retries; cache seeds never do.
    const retry=data.scanned===0&&(op.emptyRetries??0)<2&&!document.hidden&&root.isConnected;
    incompleteList(op,retry);
    if(!retry||!alive(op)||frozen)return;
    if(document.hidden||!root.isConnected){incompleteList(op,false);return;}
    op.emptyRetries=(op.emptyRetries??0)+1;
    const timer=window.setTimeout(()=>{
      op.timer=undefined;
      if(!alive(op)||frozen)return;
      if(document.hidden||!root.isConnected){incompleteList(op,false);return;}
      readList(op);
    },1200);
    if(alive(op)&&!frozen)op.timer=timer;else window.clearTimeout(timer);
  }
  function readList(op){
    if(alive(op)&&!frozen)read(op,readSessions,{dir:op.query.dir,workers:op.query.workers,...(op.query.all?{all:true}:{})},listPane,listReceived);
  }
  function listCommit(op, data) {
    if(data.partial&&!data.sessions.length)return;
    const view = {root:node("div", "nav-controller-list"), rows:[], removes:[],data,query:{...op.query}};
    try {
      const diagnostics=node("details","nav-session-diagnostics"),diagnosticTitle=node("summary","","Snapshot details");
      diagnostics.append(diagnosticTitle,
        node("p", "nav-controller-facts", op.query.all?"All directories":`Directory: ${data.dir}`),
        node("p", "nav-controller-facts", `Scanned ${data.scanned} of ${data.of}; unreadable ${data.unreadable_count}; partial: ${data.partial ? "yes" : "no"}`));
      view.root.append(node("p","nav-session-count",`${data.sessions.length} ${data.sessions.length===1?"session":"sessions"}${data.partial?" · partial results":""}`),diagnostics);
      if(data.activity){
        const value=data.activity,date=new Date(value.observed.ms),stamp=value.observed.known&&Number.isFinite(date.getTime())?date.toLocaleString():"unknown time";
        diagnostics.append(node("p","nav-controller-facts",`Activity snapshot: ${stamp}. Refresh to check again.`),node("p","nav-controller-elsewhere",`${value.known?"":"At least "}${value.live} verified managed sessions · ${value.elsewhere} elsewhere${value.known?"":" · other managed state unknown"}${value.claude_open_known?"":" · Claude registrations incomplete"}`));
      }
      for (const rootPath of data.roots_searched) diagnostics.append(node("p", "nav-controller-facts", `Root searched: ${rootPath}`));
      const rows = node("ul", "nav-controller-rows"); view.root.append(rows);
      for (const value of data.sessions) {
        const li = node("li", "nav-controller-row"), item = {button:null}; view.rows.push(item); rows.append(li);
        const activity=value.activity,live=activity?.live??"unknown";li.setAttribute("data-nav-live",live);
        const labels={unknown:"Live state unknown",recent:"Recent activity (not verified live)",open:`Registered open in pid ${activity?.pid??0} (process exists; session not verified)`,live:"Verified running · aiq long",ended:"Managed session verified ended"};
        const brief={unknown:"◌ Unknown",recent:"◔ Recent write",open:"○ Open, registered",live:"● Live",ended:"Ended, verified"};
        const activityLabel=node("p","nav-controller-activity",brief[live]??brief.unknown);activityLabel.title=labels[live]??labels.unknown;li.append(activityLabel);
        const label = `${value.provider} · ${value.label} · ${value.id}`;
        if (value.selectable) {
          item.button = node("button", "nav-controller-select", value.label||value.title||"Untitled conversation"); item.button.type = "button";item.button.title=label;li.append(item.button);
          const selectedScope = {provider:value.provider, id:value.id};
          const click = () => { if (mounted && !disposed && listView === view) select(selectedScope, {item, view}); };
          item.button.addEventListener("click", click);
          view.removes.push(() => item.button.removeEventListener("click", click));
        } else li.append(node("p", "nav-controller-unavailable", `${label} · Unavailable for selection`));
        const summary=node("div","nav-session-summary"),project=node("span","nav-session-project",value.cwd?value.cwd.split("/").filter(Boolean).at(-1)||"/":"Unknown project"),agent=node("span","nav-session-agent",`${value.provider}${value.model?" · "+value.model:""}`),size=node("span","nav-session-size",`${value.turns} ${value.turns===1?"turn":"turns"}`),updated=node("span","nav-session-updated",value.updated);
        project.title=value.cwd||"Directory unknown";updated.title=value.updated;summary.append(project,agent,size,updated);li.append(summary);
        const details=node("details","nav-session-commands"),toggle=node("summary","","Copy commands and details"),panel=node("div","nav-session-command-panel");
        details.append(toggle,panel);li.append(details);
        panel.append(node("p","nav-controller-facts",labels[live]??labels.unknown));
        commandControls(panel,value,()=>mounted&&!disposed&&listView===view,view.removes);
        const drainLabels={"":"no wrap-up recorded",requested:"wrapping up · moves at turn end",draining:"wrapping up · moves at turn end",ready:"wrap-up ready",waiting:"waiting for an account"};
        if(activity?.managed)panel.append(node("p","nav-controller-facts",`Recorded account: ${activity.account} · launcher: ${activity.launcher||"bare"} · terminal: ${activity.pane} · ${Object.hasOwn(drainLabels,activity.drain)?drainLabels[activity.drain]:"drain state unknown"}${activity.drain_manual?" · requested by you":""}`));
        if(activity?.workspace){const link=node("a","nav-controller-workspace",activity.continued?"Current session in workspace →":"Workspace history and terminal controls →");link.href="/long?dir="+encodeForm(activity.workspace);link.rel="noreferrer";panel.append(link);}
        panel.append(node("p", "nav-controller-facts", `${value.provider} · ${value.id}`),node("p", "nav-controller-facts", `Title: ${value.title} · Directory: ${value.cwd||"Unknown"}`),
          node("p", "nav-controller-facts", `Model: ${value.model} · Branch: ${value.branch} · Updated: ${value.updated}`),
          node("p", "nav-controller-facts", `Turns: ${value.turns} · Tools: ${value.tools} · Worker: ${value.worker ? "yes" : "no"} · Bypass: ${value.bypass ? "yes" : "no"}`));
      }
      if (!alive(op)) return;
      clearList(); if (!alive(op)) return;
      listView = view; listPane.content.append(view.root); markers();
      listState(null,false);
      status(op, listPane, data.sessions.length ? "Sessions ready." : "No sessions.", false);
    } finally {
      if (listView !== view) { for (const remove of view.removes) remove(); view.root.remove(); }
    }
  }
  function timeText(value) {
    if (!value.known) return "Unknown";
    const date = new Date(value.ms);
    return Number.isFinite(date.getTime()) ? date.toISOString() : `${value.ms} ms since Unix epoch`;
  }
  function detailHeaderText(data){
    const s=data.session;
    return [`${s.provider} · ${s.id}`,`Title: ${s.title} · Directory: ${s.cwd||"Unknown"}`,`Model: ${s.model} · Branch: ${s.branch}`,
      `Started: ${timeText(s.started)} · Updated: ${timeText(s.updated)}`,`Worker: ${s.worker ? "yes" : "no"} · Bypass: ${s.bypass ? "yes" : "no"}`,
      `Earlier hidden: ${data.earlier_hidden} · Later hidden: ${data.later_hidden} · Window start: ${data.window_start}`];
  }
  function detailCommit(op, data) {
    const candidate = {root:node("div", "nav-controller-conversation"), child:null, busy:true, retired:false, cleaned:false, removes:[]};
    op.candidate = candidate;
    try {
      const s = data.session;
      candidate.headers=detailHeaderText(data).map(text=>node("p","nav-controller-facts",text));
      const metadata=prose?node("details","nav-conversation-details"):candidate.root;
      if(prose){metadata.append(node("summary","",readCommands||resumeCommands?"Terminal commands and details":"Session details"));candidate.root.append(node("p","nav-conversation-meta",`${s.cwd.split("/").filter(Boolean).at(-1)||"Unknown project"} · ${s.provider}${s.model?" · "+s.model:""}`),metadata);}
      metadata.append(...candidate.headers);
      commandControls(metadata,s,()=>alive(op)&&!candidate.retired,candidate.removes);
      if(!conversationOnly&&s.cwd){try{directory(s.cwd);const link=node("a","nav-controller-workspace","Workspace history and terminal controls →");link.href="/long?dir="+encodeForm(s.cwd);link.rel="noreferrer";metadata.append(link);}catch{}}
      candidate.child = createConversationFeed(document, data, {window,
        idPrefix:`${idPrefix}-feed-${op.version}`, expectedScope:op.query,
        readingControls,prose, shortcutsEnabled:()=>alive(op)&&!candidate.retired&&shortcutsEnabled(),
        copyText:raw => { if (alive(op) && !candidate.retired) return copyText(raw); },
        announce:value => { if (alive(op) && !candidate.retired) announce(value); }});
      candidate.root.append(candidate.child.fragment);
      if(prose&&conversationOnly&&!readingControls&&!readCommands&&!resumeCommands)candidate.root.append(metadata);
      if (!alive(op)) return;
      detailPane.content.append(candidate.root);
      if (!alive(op)) return;
      if (!candidate.child.mount()) throw new Error("feed activation");
    } finally {
      candidate.busy = false;
      if (!alive(op) || candidate.retired) cleanCandidate(candidate);
    }
    if (!alive(op)) return;
    if(autoFollow)startFollowing(op,data);
    status(op, detailPane, data.messages.length ? "Conversation ready." : "No messages in this window.", false);
    if (!alive(op)) return;
    const intent = op.intent;
    if (intent && !intent.revoked && listView === intent.view && intent.button.isConnected && document.activeElement === intent.button) {
      cancelFocus(op);
      detailPane.heading.focus();
    } else cancelFocus(op);
    if(alive(op)){try{onDetailReady();}catch{/* A host callback cannot regain a retired selection. */}}
  }
  function readETag(envelope){
    const value=Object.getOwnPropertyDescriptor(envelope,"etag");
    if(!value)return "";
    if(!Object.hasOwn(value,"value")||typeof value.value!=="string"||!/^"[0-9a-f]{64}"$/.test(value.value))fail();
    return value.value;
  }
  function atBottom(){
    const el=prose?document.scrollingElement:detailPane.content;
    return el.scrollHeight-el.scrollTop-(prose?window.innerHeight:el.clientHeight)<=48;
  }
  function scrollNewest(){
    if(prose)window.scrollTo({top:Math.max(0,document.scrollingElement.scrollHeight-window.innerHeight),behavior:"instant"});
    else detailPane.content.scrollTop=detailPane.content.scrollHeight;
  }
  function followText(op,text,announceChange=false){
    if(!alive(op)||!op.followLabel)return;
    if(op.followLabel.textContent!==text){op.followLabel.textContent=text;if(announceChange)try{announce(text);}catch{}}
  }
  function scheduleFollow(op){
    if(!alive(op)||frozen||!op.selectionToken||op.timer!==undefined||op.followFlight||document.hidden)return;
    op.timer=window.setTimeout(()=>{op.timer=undefined;void pollFollow(op);},2000);
  }
  function startFollowing(op,data){
    op.baseline=data;
    op.selectionToken=data.selection;op.older=data.later_hidden>0||detailQuery.anchor!==0;
    op.shownTurns=data.window_start;for(const message of data.messages)op.shownTurns=Math.max(op.shownTurns,message.turn+1);
    op.following=!op.older;op.changed=false;
    detailPane.content.classList.add(prose?"nav-controller-page-follow":"nav-controller-follow-content");
    const label=node("p","nav-follow-status",op.query.provider==="agy"?"Snapshot · no live turn tracking for Antigravity.":op.older?`${data.later_hidden} newer prompts not shown as of ${new Date().toLocaleTimeString()} · jump to newest`:"Following.");
    // The timer is visual text, deliberately outside the polite live region.
    const jump=node("button","nav-follow-newest","Jump to newest");jump.type="button";
    op.followLabel=label;op.candidate.root.prepend(label);op.candidate.root.append(jump);
    const resume=()=>{
      if(!alive(op)||frozen)return;
      if(op.older){onNewest(Object.freeze({...op.query}));return;}
      // Explicit resume leaves the reading/copy focus guard. Keep focus on a
      // stable control at the newest content; poll completions never move it.
      jump.focus({preventScroll:true});
      if(!alive(op))return;
      op.following=true;scrollNewest();
      if(!op.followFlight){if(op.timer!==undefined){window.clearTimeout(op.timer);op.timer=undefined;}void pollFollow(op);}
    };
    const scroll=()=>{if(!alive(op))return;op.following=!op.older&&atBottom();if(!op.following)followText(op,"Following paused · jump to newest.");};
    const keys=e=>{
      if(!alive(op)||e.defaultPrevented||e.ctrlKey||e.metaKey||e.altKey||e.shiftKey&&e.key!=="G"||!["End","G"].includes(e.key)||
        ["INPUT","TEXTAREA","SELECT"].includes(e.target?.tagName?.toUpperCase())||e.target?.isContentEditable)return;
      if(!shortcutsEnabled())return;e.preventDefault();resume();
    };
    const scrollTarget=prose?window:detailPane.content;
    jump.addEventListener("click",resume);scrollTarget.addEventListener("scroll",scroll);detailPane.root.addEventListener("keydown",keys);
    op.candidate.removes.push(()=>jump.removeEventListener("click",resume),()=>scrollTarget.removeEventListener("scroll",scroll),()=>detailPane.root.removeEventListener("keydown",keys));
    if(!op.older)scrollNewest();
    scheduleFollow(op);
  }
  async function pollFollow(op){
    if(!alive(op)||frozen||document.hidden||op.followFlight)return;
    op.followFlight=true;
    try{
      op.abort=new window.AbortController();
      const paused=op.older||!op.following||!atBottom()||op.candidate.child.hasFocus();
      if(op.selectionToken){
        const request={...op.query,selection:op.selectionToken};
        const envelope=await readState(request,op.abort.signal);if(!alive(op)||frozen||document.hidden||op.abort.signal.aborted)return;
        const value=captureState(field(envelope,"data"),request,limits);
        if(!value.match){
          op.changed=true;op.etag="";
          if(paused){followText(op,"Conversation selection changed · jump to newest to refresh.",true);return;}
        }else{
          op.clock=value.working_known&&value.working?`Working · ${Math.floor(value.elapsed_ms/1000)}s.`:value.live==="ended"?"Ended.":value.live==="unknown"?"Following transcript · live state unknown.":"Following.";
          op.changed=op.changed||value.disk_changed;
          const newer=Math.max(0,value.cached_turns-op.shownTurns);
          if(paused){
            if(newer||op.changed)followText(op,newer?`${newer} new prompts below · jump to newest.`:"New activity below · jump to newest.",true);
            return;
          }
          if(op.query.provider!=="agy")followText(op,op.clock);
        }
      }
      if(op.older||!op.following||!atBottom()||op.candidate.child.hasFocus())return;
      const request={...op.query,...detailQuery};if(op.etag)request.etag=op.etag;
      if(op.query.provider!=="agy"&&op.baseline?.tail_key){request.fromTurn=op.baseline.tail_turn;request.fromKey=op.baseline.tail_key;}
      const envelope=await readConversation(request,op.abort.signal);if(!alive(op)||frozen||document.hidden||op.abort.signal.aborted)return;
      const notModified=Object.getOwnPropertyDescriptor(envelope,"notModified");
      if(notModified){if(!Object.hasOwn(notModified,"value")||notModified.value!==true||readETag(envelope)!==op.etag)fail();return;}
      const value=captureDetailResponse(field(envelope,"data"),op.query,limits,op.baseline,request);
      if(value.markup!==detailQuery.markup)fail();
      // User scrolling or focus movement during the fetch wins over completion.
      if(op.older||!op.following||!atBottom()||op.candidate.child.hasFocus()){op.changed=true;return;}
      const etag=readETag(envelope);
      if(!op.candidate.child.update(value)){op.changed=true;return;}
      op.baseline=value;
      const headers=detailHeaderText(value);for(let i=0;i<headers.length;i++)if(op.candidate.headers[i].textContent!==headers[i])op.candidate.headers[i].textContent=headers[i];
      op.etag=etag;op.selectionToken=value.selection;op.changed=false;
      op.shownTurns=value.window_start;for(const message of value.messages)op.shownTurns=Math.max(op.shownTurns,message.turn+1);
      scrollNewest();
      followText(op,op.query.provider==="agy"?"Snapshot refreshed · no live turn tracking for Antigravity.":op.clock??"Following.");
    }catch{if(alive(op)&&!document.hidden)followText(op,"Updates unavailable · showing the last conversation.",true);}
    finally{op.followFlight=false;if(alive(op))scheduleFollow(op);}
  }
  function snapshotSessions(){
    if(!mounted||disposed||!listView)return null;
    return {query:{...listView.query},data:captureSessions(listView.data,listView.query.canonicalDir,limits,listView.query.all)};
  }
  function loadSessions(value, options={}) {
    if (!mounted || disposed || frozen) return;
    const {op, previous} = reserve("list"); let oldDetail = null;
    try {
      op.query = {dir:text(field(value, "dir")), canonicalDir:text(field(value, "canonicalDir")), workers:bool(field(value, "workers"))};
      const all=Object.getOwnPropertyDescriptor(value,"all");op.query.all=all?bool(field(value,"all")):false;
      if (op.query.all?(op.query.dir!==""||op.query.canonicalDir!==""):(!op.query.dir||!op.query.canonicalDir)) fail();
      if (!alive(op)) return;
      if (!activeScope || activeScope.dir !== op.query.canonicalDir || activeScope.workers !== op.query.workers || activeScope.all!==op.query.all) {
        oldDetail = current.detail; current.detail = null; ++serial.detail;
        activeScope = {dir:op.query.canonicalDir, workers:op.query.workers,all:op.query.all}; op.scope = activeScope;
        selection = null; clearList();root.setAttribute("data-reading",String(conversationOnly));detailPane.root.hidden=!conversationOnly;syncRail();
        detailPane.status.textContent = ""; detailPane.root.setAttribute("aria-busy", "false");
      }
      listQuery = {...op.query};
    } catch { failed(op, listPane); return; }
    finally { retireMany(previous, oldDetail); }
    if (!alive(op)) return;
    // Only the same explicitly captured scope can seed this new controller.
    // Admission copies inert DTO fields again; cache objects never become views.
    if(!listView){
      let cached=null;
      try{
        const snapshot=field(options,"snapshot"),saved=field(snapshot,"query");
        if(["dir","canonicalDir","workers","all"].every(key=>field(saved,key)===op.query[key]))cached=captureSessions(field(snapshot,"data"),op.query.canonicalDir,limits,op.query.all);
      }catch{/* A missing, stale or malformed cache cannot block the live read. */}
      if(cached&&alive(op))listCommit(op,cached);
    }
    if(!alive(op))return;
    listState(null,Boolean(listView));
    status(op, listPane, listView ? "Refreshing sessions; showing previous results." : "Loading sessions.", true);
    readList(op);
  }
  function select(value, trigger = null) {
    if (!mounted || disposed || frozen) return;
    const {op, previous} = reserve("detail");
    root.setAttribute("data-reading","true");detailPane.root.hidden=false;syncRail();
    selection = null; markers();
    try {
      op.query = scope(value);
      if (!alive(op)) return;
      selection = {scope:{...op.query}, row:trigger?.item || null}; markers();
      if (trigger) {
        const button = trigger.item.button, intent = {button, view:trigger.view, revoked:false};
        const leave = () => { intent.revoked = true; };
        button.addEventListener("focusout", leave);
        intent.remove = () => button.removeEventListener("focusout", leave); op.intent = intent;
      }
    } catch { failed(op, detailPane); return; }
    finally { retire(previous); }
    if (!alive(op)) return;
    try { onSelection(Object.freeze({...op.query})); }
    catch { failed(op,detailPane);return; }
    if (!alive(op)) return;
    status(op, detailPane, "Loading conversation.", true);
    if (alive(op)) read(op, readConversation, {...op.query, ...detailQuery}, detailPane, detailCommit);
  }
  function refresh(which = "all") {
    if (!mounted || disposed || frozen) return;
    const list = listQuery && {...listQuery}, selected = selection, beforeScope = activeScope;
    const listVersion = serial.list + ((which === "all" || which === "list") && list ? 1n : 0n);
    if ((which === "all" || which === "list") && list) loadSessions(list);
    if ((which === "all" || which === "detail") && selected && mounted && !disposed &&
        serial.list === listVersion && activeScope === beforeScope && selection === selected) select({...selected.scope});
  }
  function teardown() {
    if (disposed) return;
    disposed = true; mounted = false;
    const oldList = current.list, oldDetail = current.detail;
    current.list = current.detail = null; ++serial.list; ++serial.detail;
    selection = listQuery = activeScope = null;
    clearList(); for (const remove of removes) remove(); removes.length = 0;
    retireMany(oldList, oldDetail); root.remove();
    readSessions = readConversation = readState = onNewest = copyText = announce = onSelection = onDetailReady = shortcutsEnabled = null;
  }
  // Workspace rollover preserves the last good feed and its copy controls.
  // A frozen controller can never publish a late initial read or follow read.
  function freeze(){
    if(disposed||frozen)return;frozen=true;
    detailPane.refresh.disabled=true;listPane.refresh.disabled=true;
    const op=current.detail;
    for(const pending of [current.list,op]){
      if(pending?.timer!==undefined){window.clearTimeout(pending.timer);pending.timer=undefined;}
      try{pending?.abort?.abort();}catch{}
    }
    if(op?.followLabel)op.followLabel.textContent="Frozen session snapshot.";
    for(const button of root.querySelectorAll(".nav-follow-newest"))button.disabled=true;
  }
  function canFollow(){
    const op=current.detail;
    return Boolean(op&&alive(op)&&!frozen&&op.candidate?.child&&op.following&&!op.older&&atBottom()&&!root.contains(document.activeElement));
  }
  function mount() {
    if (disposed) return false;
    if (mounted) return true;
    if (!root.isConnected || root.ownerDocument !== document) return false;
    try {
      mounted = true;
      if(autoFollow){
        const visibility=()=>{
          const op=current.detail;if(!alive(op))return;
          if(document.hidden){if(op.timer!==undefined){window.clearTimeout(op.timer);op.timer=undefined;}if(op.followFlight)op.abort?.abort();}
          else scheduleFollow(op);
        };
        document.addEventListener("visibilitychange",visibility);removes.push(()=>document.removeEventListener("visibilitychange",visibility));
      }
      for (const [button, callback] of [[listPane.refresh, () => refresh("list")], [detailPane.refresh, () => refresh("detail")]]) {
        button.addEventListener("click", callback); removes.push(() => button.removeEventListener("click", callback));
      }
      return true;
    } catch { teardown(); return false; }
  }
  return {fragment, mount, loadSessions, snapshotSessions, setMarkup:value=>{if(typeof value!=="boolean"||disposed)return false;detailQuery.markup=value;if(selection)select({...selection.scope});return true;},selectConversation:value => select(value), refresh, freeze, canFollow, teardown};
}
