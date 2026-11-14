import {captureLong} from "./controller_data.js";
import {directory,encodeForm} from "./auth_codec.js";
import {createSessionController,navigatorLeaseCommand} from "./session_controller.js";

const refused=()=>new TypeError("Workspace view unavailable");
export function createLongView(document,options){
  const {window,dir,idPrefix,limits,readLong,readConversation,readState,copyText,announce,onWorkspace,onConversation}=options;
  if(!window||window!==document.defaultView||typeof dir!=="string"||!/^nav-long-[0-9]+$/.test(idPrefix)||
    ![readLong,readConversation,readState,copyText,announce,onWorkspace,onConversation].every(fn=>typeof fn==="function"))throw refused();
  if(dir)directory(dir);
  const node=(tag,cls,text)=>{const n=document.createElement(tag);if(cls)n.className=cls;if(text!==undefined)n.textContent=text;return n;};
  const button=(text,cls="")=>{const n=node("button",cls,text);n.type="button";return n;};
  const fragment=document.createDocumentFragment(),root=node("section","nav-long"),heading=node("h2","",dir?"Workspace":"Long workspaces");
  heading.id=idPrefix;root.setAttribute("aria-labelledby",idPrefix);fragment.append(root);
  const refresh=button("Refresh workspace","nav-long-refresh"),meta=node("div","nav-long-meta"),history=node("div","nav-long-history"),
    notice=node("p","nav-long-notice"),newest=button("Jump to current session","nav-long-newest"),feed=node("div","nav-long-feed");
  newest.hidden=true;root.append(heading,refresh,meta,notice,newest,history,feed);
  const actions=node("section","nav-long-actions"),actionTarget=node("p","nav-long-action-target"),actionState=node("p","nav-long-action-state"),
    attach=button("Copy attach command","nav-command-copy"),drain=button("Copy wrap-up command","nav-command-copy"),moves=node("div","nav-long-moves");
  actions.setAttribute("aria-label","Terminal commands");moves.setAttribute("role","group");moves.setAttribute("aria-label","Moves work");
  moves.append(node("p","","Moves work · moves the session to another account at its next turn end and archives its note."),drain);
  actions.append(actionTarget,attach,moves,actionState);actions.hidden=!dir;root.insertBefore(actions,notice);
  const allWorkspaces=button("All workspaces","nav-long-all");allWorkspaces.hidden=!dir;root.insertBefore(allWorkspaces,meta);
  let mounted=false,disposed=false,flight=null,timer=null,wantFull=false,metadata=null,fullMetadata=null,controller=null,scope=null,
    frozen=false,parkedAuto=false,pending=null,leaseKey=null,account=null,version=0,fullKey=null,lastSuccess=null,displayedTarget=null,copyBusy=false;
  const removes=[],linkRemoves={meta:[],history:[]};let deferredHistory=null,deferredMeta=null;
  const alive=()=>mounted&&!disposed&&root.isConnected;
  const say=text=>{if(!alive())return;notice.textContent=text;try{announce(text);}catch{}};
  function listen(target,type,fn,capture=false){target.addEventListener(type,fn,capture);removes.push(()=>target.removeEventListener(type,fn,capture));}
  const now=()=>window.performance?.now()??Date.now();
  const targetKey=value=>value?.lease?JSON.stringify([value.workspace,value.lease.id,value.lease.started,value.lease.provider,value.lease.account,value.lease.pane]):"";
  function fresh(){const age=lastSuccess===null?Infinity:now()-lastSuccess;return alive()&&!document.hidden&&metadata?.state==="live"&&metadata.workspace_known&&age>=0&&age<30000;}
  function updateActions(){
    if(!alive()||!dir)return;
    if(!copyBusy&&!actions.contains(document.activeElement)){
      displayedTarget=metadata?.lease?{key:targetKey(metadata),id:metadata.lease.id,started:metadata.lease.started}:null;
      actionTarget.textContent=metadata?.lease?"Terminal only · "+metadata.workspace+" · "+metadata.lease.account:"Terminal target not verified.";
    }
    const eligible=fresh()&&displayedTarget?.key===targetKey(metadata);
    for(const [verb,control]of [["attach",attach],["drain",drain]]){
      const command=displayedTarget?navigatorLeaseCommand(verb,displayedTarget.id,displayedTarget.started):null;
      control.setAttribute("aria-disabled",eligible&&!copyBusy&&command!==null?"false":"true");control.title=command??"";
    }
    actionState.textContent=eligible?"Copy a command, then paste it in a terminal.":"Attach and wrap-up: waiting for fresh verified data.";
  }
  async function copyLease(verb){
    if(!fresh()||copyBusy||!displayedTarget||displayedTarget.key!==targetKey(metadata))return;
    const captured=displayedTarget,command=navigatorLeaseCommand(verb,captured.id,captured.started);if(command===null)return;
    copyBusy=true;updateActions();
    try{await copyText(command);if(fresh()&&captured.key===targetKey(metadata))say("Terminal command copied. Paste it in a terminal.");}
    catch{if(alive()&&captured.key===targetKey(metadata))say("Terminal command could not be copied.");}
    finally{copyBusy=false;updateActions();}
  }
  function time(value){if(!value.known)return "Unknown";const date=new Date(value.ms);return Number.isFinite(date.getTime())?date.toLocaleString():String(value.ms)+" ms since Unix epoch";}
  function clearLinks(area){for(const remove of linkRemoves[area])remove();linkRemoves[area].length=0;}
  function link(label,path,callback,area){
    const a=node("a","",label);a.href=path;a.rel="noreferrer";
    const click=e=>{if(!alive()||e.button!==0||e.ctrlKey||e.metaKey||e.altKey||e.shiftKey)return;e.preventDefault();callback();};
    a.addEventListener("click",click);linkRemoves[area].push(()=>a.removeEventListener("click",click));return a;
  }
  function spans(values){
    const list=node("ul","nav-long-spans");
    for(const value of values)list.append(node("li","",value.account+" · "+value.launcher+" · recorded "+time(value.from)+(value.until.known?" · next launch "+time(value.until):"")));
    return list;
  }
  function drawMeta(value){
    if(meta.contains(document.activeElement)){deferredMeta=value;return;}deferredMeta=null;
    clearLinks("meta");
    const content=document.createDocumentFragment();
    if(value.kind==="list"){
      content.append(node("p","",value.workspaces.length+" workspaces found"+(value.partial?" · partial history":"")));
      const list=node("ul","nav-controller-rows");
      for(const row of value.workspaces){const item=node("li","nav-controller-row");item.append(link(row.workspace,"/long?dir="+encodeForm(row.workspace),()=>onWorkspace(row.workspace),"meta"),node("p","",row.sessions+" recorded sessions · "+row.recorded_leases+" recorded leases"));list.append(item);}
      if(!value.workspaces.length)content.append(node("p","",value.partial?"Workspace history could not be completely mapped.":"No recorded long workspaces."));
      content.append(list);
    }else{
      content.append(node("p","nav-long-workspace",value.workspace+(value.workspace_known?"":" · workspace mapping unknown")));
      const lease=value.lease;
      const label=value.state==="live"?"Managed session verified running":value.state==="never"?"No long-session history found":value.state==="none"?"No managed session currently verified running":"Managed session state unknown";
      content.append(node("p","nav-long-state",label));
      if(lease){
        content.append(node("p","nav-long-account",lease.provider+" · "+lease.account+" · "+lease.launcher),node("p","nav-long-pane","Terminal: "+(lease.pane||"Unknown")+" · lease "+lease.id+" · started "+lease.started));
        const drain={"":"No drain recorded",requested:"Wrap-up requested",draining:"Finishing wrap-up",ready:"Wrap-up ready",waiting:"Waiting for an account",unknown:"Drain state unknown"};
        content.append(node("p","nav-long-drain",drain[lease.drain]));
        if(lease.working_known)content.append(node("p","",lease.working?"Working since "+time(lease.turn_started):"No active turn recorded"));
        if(!value.current&&lease.live==="live")content.append(node("p","","Waiting for a verified session transcript."));
      }
      if(value.partial)content.append(node("p","","Partial history · "+value.unmapped_launches+" launch records could not be mapped."));
    }
    if(alive())meta.replaceChildren(content);
  }
  function drawHistory(value){
    if(value.kind!=="workspace"||!value.history_present)return;
    if(history.contains(document.activeElement)){deferredHistory=value;return;}
    deferredHistory=null;clearLinks("history");
    const content=document.createDocumentFragment(),notes=value.note;
    content.append(node("p","nav-long-note",!notes.available?"Handoff note metadata unavailable":(notes.exists?"Current handoff note updated "+time(notes.updated):"No current handoff note")+" · "+notes.archived+" archived notes"));
    if(value.current_spans.length){const current=node("details","nav-long-current-spans");current.append(node("summary","","Recorded account spans for the current lease session"),spans(value.current_spans));content.append(current);}
    const details=node("details","nav-long-earlier"),list=node("ul","nav-controller-rows");details.open=true;
    details.append(node("summary","",value.earlier.length+" earlier sessions found"+(value.partial?" · partial":"")),list);
    for(const item of value.earlier){
      const card=node("li","nav-controller-row"),label=item.provider+" · "+(item.session_id||"Session ID not recorded");
      if(item.transcript_available)card.append(link(label,"/sessions/"+item.provider+"/"+item.session_id+"?dir="+encodeForm(value.workspace),()=>onConversation({provider:item.provider,id:item.session_id},value.workspace),"history"));
      else card.append(node("p","",label+" · transcript unavailable"));
      if(!item.workspace_known)card.append(node("p","","Recorded directory retained; workspace mapping unknown."));
      card.append(spans(item.spans));list.append(card);
    }
    content.append(details);if(alive())history.replaceChildren(content);
  }
  const sameScope=(a,b)=>Boolean(a&&b&&a.provider===b.provider&&a.id===b.id);
  const safeToAdvance=()=>!controller||(!root.contains(document.activeElement)&&(controller.canFollow()||frozen&&parkedAuto));
  function park(){
    if(!controller||frozen)return;
    parkedAuto=controller.canFollow();frozen=true;controller.freeze();
  }
  function showSession(next,explicit=false){
    if(!alive()||!next)return;
    const previous=scope,wasDifferent=previous&&!sameScope(previous,next);
    controller?.teardown();controller=null;scope={...next};frozen=false;parkedAuto=false;pending=null;newest.hidden=false;
    const content=document.createDocumentFragment();
    if(wasDifferent){const divider=node("p","nav-long-divider","Session changed · previous session "+previous.provider+" · "+previous.id+" is available in history.");divider.setAttribute("role","separator");content.append(divider);}
    controller=createSessionController(document,{window,idPrefix:idPrefix+"-session-"+(++version),limits,conversationOnly:true,
      detailQuery:options.detailQuery,readingControls:true,readCommands:true,resumeCommands:true,autoFollow:true,shortcutsEnabled:options.shortcutsEnabled,
      readSessions:()=>Promise.reject(refused()),readConversation,readState,copyText,announce,
      onNewest:()=>{if(alive()){showSession(metadata?.current??scope,true);}},onSelection:()=>{}});
    content.append(controller.fragment);feed.replaceChildren(content);
    if(!alive()||!controller.mount()){controller?.teardown();return;}
    controller.selectConversation(next);
    if(explicit)newest.focus({preventScroll:true});
    say(wasDifferent?"Following a new workspace session.":"Following the workspace session.");
  }
  function accept(value,full){
    if(!alive())return;
    const previousMetadata=metadata;
    const key=value.lease?value.lease.id+"/"+value.lease.started+"/"+value.lease.provider:"none";
    const changed=leaseKey!==null&&key!==leaseKey;
    const next=value.current;
    const changedAccount=account!==null&&value.lease&&value.lease.account!==account;
    metadata=value;leaseKey=key;account=value.lease?.account??null;lastSuccess=now();updateActions();
    drawMeta(value);
    if(previousMetadata&&value.kind==="workspace"&&(previousMetadata.state!==value.state||previousMetadata.lease?.drain!==value.lease?.drain))
      say(value.state==="unknown"?"Managed session state unknown.":value.lease?.drain?"Workspace wrap-up state changed: "+value.lease.drain+".":value.state==="live"?"Managed workspace session verified running.":"No managed workspace session currently verified running.");
    if(full){fullMetadata=value;fullKey=key;drawHistory(value);}
    else if(fullKey!==key||fullMetadata?.current?.provider!==next?.provider||fullMetadata?.current?.id!==next?.id||changedAccount)wantFull=true;
    if(value.kind!=="workspace")return;
    if(next){
      if(!sameScope(scope,next)||frozen){
        if(safeToAdvance())showSession(next);
        else{park();pending={...next};newest.hidden=false;say("New session below · jump to the current workspace session when ready.");}
      }else if(changedAccount)say("Account changed to "+value.lease.account+" for the same session.");
    }else if(controller&&(changed||value.lease?.live==="ended"||!value.lease)){
      park();pending=null;newest.hidden=false;say(value.lease?.live==="unknown"?"Workspace selection changed; current session unknown. Showing the previous snapshot.":"Waiting for the next verified workspace session. Previous conversation is frozen.");
    }
  }
  function schedule(){
    if(!alive()||!dir||document.hidden||flight||timer!==null)return;
    timer=window.setTimeout(()=>{timer=null;updateActions();request(false);},2000);
  }
  function request(full){
    if(!alive())return;wantFull=wantFull||full;
    if(flight||document.hidden)return;
    if(timer!==null){window.clearTimeout(timer);timer=null;}
    const job={abort:new window.AbortController(),full:wantFull};wantFull=false;flight=job;
    refresh.disabled=true;
    Promise.resolve().then(()=>{if(!alive()||document.hidden||job.abort.signal.aborted)throw refused();return readLong({dir,history:job.full},job.abort.signal);}).then(envelope=>{
      if(!alive()||flight!==job||document.hidden||job.abort.signal.aborted)return;
      const data=Object.getOwnPropertyDescriptor(envelope,"data");if(!data||!Object.hasOwn(data,"value"))throw refused();
      accept(captureLong(data.value,dir,job.full,limits),job.full);
    }).catch(()=>{if(alive()&&!document.hidden&&!job.abort.signal.aborted)say(metadata?"Workspace updates unavailable · showing the last metadata and conversation.":"Workspace metadata unavailable. Refresh to retry.");}).finally(()=>{
      if(flight!==job)return;flight=null;if(!alive())return;refresh.disabled=false;
      if(wantFull&&!document.hidden)request(true);else schedule();
    });
  }
  function mount(){
    if(disposed)return false;if(mounted)return alive();if(!root.isConnected)return false;mounted=true;
    listen(refresh,"click",()=>request(true));
    listen(attach,"click",()=>copyLease("attach"));listen(drain,"click",()=>copyLease("drain"));
    listen(actions,"focusout",()=>queueMicrotask(updateActions));
    listen(meta,"focusout",()=>queueMicrotask(()=>{if(alive()&&deferredMeta&&!meta.contains(document.activeElement))drawMeta(deferredMeta);}));
    listen(allWorkspaces,"click",()=>onWorkspace(""));
    listen(newest,"click",()=>{if(!alive())return;const next=pending??metadata?.current;if(next){newest.focus({preventScroll:true});showSession(next,true);}else{say("Current session is not yet verified. Checking workspace metadata.");request(true);}});
    const revoke=()=>{if(frozen)parkedAuto=false;};listen(feed,"focusin",revoke);listen(feed,"scroll",revoke,true);
    listen(history,"focusout",()=>queueMicrotask(()=>{if(alive()&&deferredHistory&&!history.contains(document.activeElement))drawHistory(deferredHistory);}));
    listen(document,"visibilitychange",()=>{
      updateActions();
      if(document.hidden){if(timer!==null){window.clearTimeout(timer);timer=null;}wantFull=wantFull||Boolean(flight?.full)||!metadata;try{flight?.abort.abort();}catch{}}
      else if(!flight)request(!metadata||wantFull);
    });
    updateActions();request(true);return alive();
  }
  function teardown(){
    if(disposed)return;disposed=true;mounted=false;if(timer!==null)window.clearTimeout(timer);
    try{flight?.abort.abort();}catch{}controller?.teardown();controller=null;
    for(const remove of removes)try{remove();}catch{}removes.length=0;clearLinks("meta");clearLinks("history");root.remove();metadata=fullMetadata=deferredHistory=deferredMeta=scope=pending=displayedTarget=null;
  }
  return Object.freeze({fragment,mount,teardown});
}
