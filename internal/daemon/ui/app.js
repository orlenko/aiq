import {directory,encodeForm,route} from "./auth_codec.js";
import {createReadBroker} from "./read_broker.js";
import {createSessionController} from "./session_controller.js";
import {createAsksController} from "./asks_controller.js";
import {createLongView} from "./long_view.js";
import {createQuotaView} from "./quota_view.js";
import {captureDirectories} from "./controller_data.js";

const owners=new WeakMap();
const copyFlights=new WeakMap(); // One physical clipboard operation per document.
const refused=()=>new TypeError("Navigator view unavailable");
// Unavailable rows must not consume the list's only keyboard entry point.
// Native visibility also excludes controls inside closed/hidden disclosures.
export function setNavigatorRoving(group,active) {
  // ARIA busy controls remain focusable, including after async re-enable.
  const rows=[...group.querySelectorAll(".nav-controller-row,.nav-asks-item,.nav-asks-session")].filter(n=>!n.hidden&&(!n.closest||n.closest(".nav-controller-rows,.nav-asks-region,.nav-asks-session-list")===group));
  const controls=row=>[...row.querySelectorAll("button,a,summary")].filter(c=>!c.closest||c.closest(".nav-controller-row,.nav-asks-item,.nav-asks-session")===row);
  const entries=rows.map(row=>({row,controls:controls(row).filter(c=>!c.disabled&&!c.hidden&&
    (c.tagName!=="A"||c.hasAttribute("href"))&&c.getClientRects().length>0)})).filter(e=>e.controls.length>0);
  const chosen=entries.find(e=>e.row.contains(active))??entries.find(e=>e.row.dataset.current==="true")??entries[0];
  for(const row of rows){row.dataset.current=String(row===chosen?.row);for(const c of controls(row))c.tabIndex=row===chosen?.row&&chosen.controls.includes(c)?0:-1;}
  return entries;
}
export function bindNavigatorRoving(content,isCurrent) {
  const update=()=>{if(!isCurrent())return;for(const group of content.querySelectorAll(".nav-controller-rows,.nav-asks-region,.nav-asks-session-list"))setNavigatorRoving(group,content.ownerDocument.activeElement);};
  // Native Enter/Space activation emits a bubbling click after disclosure state
  // changes. Refresh tab stops before the next Tab, even if focus stays put.
  content.addEventListener("focusin",update);content.addEventListener("click",update);
  return ()=>{content.removeEventListener("focusin",update);content.removeEventListener("click",update);};
}
export function navigateNavigatorRows(event,active,singleKeys=true){
  if(!singleKeys&&!["ArrowDown","ArrowUp","Home","End"].includes(event.key))return false;
  const group=event.target.closest(".nav-controller-rows,.nav-asks-region,.nav-asks-session-list");if(!group)return false;
  const entries=setNavigatorRoving(group,active),index=entries.findIndex(e=>e.row.contains(active));
  let next=index;
  if(["j","ArrowDown"].includes(event.key))next=Math.min(entries.length-1,index+1);
  else if(["k","ArrowUp"].includes(event.key))next=Math.max(0,index-1);
  else if(["g","Home"].includes(event.key))next=0;
  else if(["G","End"].includes(event.key))next=entries.length-1;
  else return false;
  const target=entries[next]?.controls[0];if(!target)return false;
  event.preventDefault();target.focus();setNavigatorRoving(group,target);return true;
}
function address(path,dir="",all=false,anchor=0) {
  const values={};if(dir)values.dir=directory(dir);if(all)values.all="true";if(anchor)values.at=String(anchor);
  const query=Object.keys(values).sort().map(k=>encodeForm(k)+"="+encodeForm(values[k])).join("&");
  return route(path+(query?"?"+query:"")).next;
}
function selected(value) {
  const parts=value.path.split("/");
  return parts.length===4?{provider:parts[2],id:parts[3]}:null;
}

// All private data arrives through fixed reader capabilities after unlock.
// Construction has no DOM publication, storage, network or clipboard effects.
export function createAppHost(document,options) {
  const {window,root,isCurrent,readers}=options;
  const actions=options.actions;let actionFlight=null,reopenContext=null;
  if (!window || document.defaultView!==window || root?.ownerDocument!==document || typeof isCurrent!=="function") throw refused();
  const limits={};for(const key of ["rows","arrayEntries","stringUnits"]){const n=options.limits?.[key];if(!Number.isSafeInteger(n)||n<=0)throw refused();limits[key]=n;}
  const initial=options.initialNext,broker=createReadBroker(readers),owner={},removes=[];
  const askViews=new Map();
  const askKey=parsed=>parsed.all?"all":"dir:"+parsed.dir;
  let mounted=false,disposed=false,current=null,generation=0n,placementEpoch=0n,lastDirectory="",currentAddress=null,shortcuts=true,lastPrompts=50,scopeReason="",sharedScope={all:true,dir:""},askOrigin=null;
  const node=(tag,cls,text)=>{const n=document.createElement(tag);if(cls)n.className=cls;if(text!==undefined)n.textContent=text;return n;};
  const button=(text,cls="")=>{const n=node("button",cls,text);n.type="button";return n;};
  const shell=node("div","nav-shell"),header=node("header","nav-header"),nav=node("nav","nav-tabs");nav.setAttribute("aria-label","Navigator pages");
  const title=node("h2","nav-page-title","Sessions");title.tabIndex=-1;
  const links=new Map();for(const [path,label]of [["/asks","Asks"],["/sessions","Sessions"],["/long","Workspaces"],["/quotas","Quotas"],["/help","Help"]]){const a=node("a","nav-tab",label);a.href=path;a.rel="noreferrer";links.set(path,a);nav.append(a);}
  const brand=node("a","nav-brand","aiq");brand.href="/asks?all=true";
  const back=button("← Back","nav-back");const machine=node("span","nav-machine-scope","This machine");machine.title="Sessions and asks on this computer. Cross-machine aggregation is not available yet.";header.append(brand,nav,machine);
  const toolbar=node("form","nav-toolbar"),dirLabel=node("label","nav-directory-label","Directory"),dirInput=node("input","nav-directory"),apply=button("Show directory");
  dirInput.type="text";dirInput.autocomplete="off";dirInput.spellcheck=false;dirInput.placeholder="/absolute/project/path";dirLabel.append(dirInput);apply.type="submit";
  const allLabel=node("label","nav-toggle"),allInput=node("input");allInput.type="checkbox";allLabel.append(allInput,document.createTextNode(" All Asks"));
  toolbar.append(dirLabel,apply,allLabel);
  const recentLabel=node("label","nav-directory-label","Recent directories"),recent=node("select","nav-recent-directories"),catalogStatus=node("p","nav-catalog-status");
  recentLabel.append(recent);toolbar.append(recentLabel,catalogStatus);
  const allSessions=button("All projects","nav-all-sessions"),directoryTree=node("details","nav-directory-tree");
  toolbar.append(allSessions);
  const scopeMenu=node("details","nav-scope-menu"),scopeSummary=node("summary","nav-scope-summary","All projects"),scopePopup=node("div","nav-scope-popup");scopePopup.append(toolbar,directoryTree);scopeMenu.append(scopeSummary,scopePopup);header.append(scopeMenu);
  const controls=node("div","nav-controls"),searchLabel=node("label","nav-search-label","Search loaded results"),search=node("input","nav-search");search.type="search";search.autocomplete="off";searchLabel.append(search);
  const workersLabel=node("label","nav-toggle"),workers=node("input");workers.type="checkbox";workersLabel.append(workers,document.createTextNode(" Workers"));
  const liveLabel=node("label","nav-toggle"),liveOnly=node("input","nav-live-only");liveOnly.type="checkbox";liveLabel.append(liveOnly,document.createTextNode(" Live or registered open only"));
  const marksLabel=node("label","nav-toggle nav-markdown-toggle"),marks=node("input");marks.type="checkbox";marksLabel.append(marks,document.createTextNode(" Markdown symbols"));marksLabel.title="Toggle Markdown symbols (m)";
  const windowLabel=node("label","nav-window-label","Conversation window"),windowSelect=node("select","nav-window");
  for(const [value,label]of [["50","Last 50 prompts"],["100","Last 100 prompts"],["500","Last 500 prompts"],["all","Show all"]]){const option=node("option","",label);option.value=value;windowSelect.append(option);}windowLabel.append(windowSelect);
  const themeLabel=node("label","nav-theme-label","Theme"),theme=node("select","nav-theme");for(const [value,label]of [["system","System"],["light","Light"],["dark","Dark"]]){const o=node("option","",label);o.value=value;theme.append(o);}themeLabel.append(theme);
  search.placeholder="Filter loaded asks";header.append(searchLabel);
  const readingOptions=node("div","nav-reading-options");readingOptions.append(windowLabel);header.append(marksLabel);
  controls.append(back,liveLabel,workersLabel,readingOptions);
  const tabMenu=node("details","nav-tab-menu"),tabSummary=node("summary","","Tab unlocked"),forget=button("Forget this tab"),tabFacts=node("p","","Forgetting this tab does not revoke other tabs or server sessions. Login expires after 30 minutes of inactivity or eight hours total.");
  const tabPopup=node("div","nav-tab-popup");tabPopup.append(forget,tabFacts,themeLabel);tabMenu.append(tabSummary,tabPopup);header.append(tabMenu);
  const hasAuth=typeof options.auth?.forget==="function"&&typeof options.auth?.state==="function";tabMenu.hidden=!hasAuth;root.setAttribute("data-auth-controls",String(hasAuth));
  const status=node("p","nav-app-status");status.setAttribute("role","status");status.setAttribute("aria-live","polite");
  const note=node("p","nav-snapshot-note","Claude and Codex conversations update while visible. Scroll up to pause; End or Jump to newest resumes. Antigravity is a refreshing snapshot. Unverified live state stays unknown.");
  const placement=node("aside","nav-placement");placement.hidden=true;
  const askReturn=node("aside","nav-ask-return"),backToAsks=button("← Back to Asks"),askContext=node("span");askReturn.hidden=true;askReturn.append(backToAsks,askContext);
  const copyFallback=node("aside","nav-copy-fallback");copyFallback.hidden=true;
  const content=node("div","nav-app-content");
  const actionNotice=node("aside","nav-ask-action-notice");actionNotice.hidden=true;
  const filterEmpty=node("div","nav-filter-empty"),filterMessage=node("p"),clearFilter=button("Clear filter");filterEmpty.hidden=true;filterEmpty.append(filterMessage,clearFilter);
  const helpDialog=node("dialog","nav-key-dialog"),closeHelp=button("Close keyboard help"),keyText=node("p","","Tab stays with your browser. In a list: j/k or arrows move, Home/End move to an edge, Enter opens. / searches loaded results; Esc clears search or goes back. In a focused conversation: n/p move between prompt headings. Press m anywhere outside a text field to toggle Markdown symbols.");
  const shortcutLabel=node("label","nav-toggle"),shortcutInput=node("input");shortcutInput.type="checkbox";shortcutInput.checked=true;shortcutLabel.append(shortcutInput,document.createTextNode(" Enable single-key shortcuts"));helpDialog.append(node("h2","","Keyboard help"),keyText,shortcutLabel,closeHelp);
  const helpButton=button("Keyboard help","nav-key-help");tabPopup.append(helpButton);
  shell.append(header,title,controls,status,actionNotice,placement,askReturn,copyFallback,filterEmpty,content,helpDialog);
  function alive(){if(!mounted||disposed||owners.get(root)!==owner||!root.isConnected)return false;try{return Boolean(isCurrent())&&!disposed&&owners.get(root)===owner;}catch{return false;}}
  function active(op){return alive()&&current===op&&!op.retired;}
  function listen(target,type,fn){const wrapped=e=>{if(alive())fn(e);};removes.push(()=>target.removeEventListener(type,wrapped));target.addEventListener(type,wrapped);}
  function normalizeStatuses(){for(const n of content.querySelectorAll('[role="status"]')){n.removeAttribute("role");n.setAttribute("aria-live","off");}}
  function announce(text){if(!alive())return;normalizeStatuses();filter();status.textContent=text;}
  function filter(){
    if(!alive())return;if(current)current.filter=search.value;const q=search.value.toLocaleLowerCase();
    if(currentAddress?.path==="/asks"){
      const has=current?.controller?.setFilter(search.value);filterEmpty.hidden=!q||has;filterMessage.textContent="No loaded asks match.";roving();return;
    }
    const rows=[...content.querySelectorAll(".nav-controller-row")];
    for(const row of rows)row.hidden=q!==""&&!(row.getAttribute("data-filter-text")??row.textContent).toLocaleLowerCase().includes(q)||liveOnly.checked&&currentAddress?.path.startsWith("/sessions")&&!["live","open"].includes(row.getAttribute("data-nav-live"));
    filterEmpty.hidden=!(rows.length&&q&&!rows.some(row=>!row.hidden));filterMessage.textContent="No loaded sessions match.";
    roving();
  }
  function roving(){
    for(const group of content.querySelectorAll(".nav-controller-rows,.nav-asks-region,.nav-asks-session-list")){
      setNavigatorRoving(group,document.activeElement);
    }
  }
  async function copy(text,op){
    if(typeof text!=="string"||!active(op))throw refused();
    const flight={};
    if(copyFlights.has(document))throw refused();
    copyFlights.set(document,flight);
    try{
      const clipboard=window.navigator.clipboard;if(!active(op))throw refused();const write=clipboard?.writeText;if(!active(op)||typeof write!=="function")throw refused();
      await Reflect.apply(write,clipboard,[text]);if(!active(op))throw refused();
    }catch{
      if(active(op)){
        const label=node("label","","Copy this text manually"),area=node("textarea"),close=button("Close manual copy");area.readOnly=true;area.value=text;
        label.append(area);copyFallback.replaceChildren(label,close);copyFallback.hidden=false;area.focus();area.select();
        close.addEventListener("click",()=>{if(active(op)){copyFallback.replaceChildren();copyFallback.hidden=true;}},{once:true});
        announce("Copy unavailable; the requested text is selected for manual copying.");
      }
      throw refused();
    }finally{if(copyFlights.get(document)===flight)copyFlights.delete(document);}
  }
  function clearRoute(){
    const old=current;
    if(old?.asksKey){const value=old.controller?.viewState();if(value){askViews.delete(old.asksKey);askViews.set(old.asksKey,{value,filter:old.filter??"",scroll:window.scrollY});while(askViews.size>4)askViews.delete(askViews.keys().next().value);}}
    if(old)old.retired=true;current=null;++placementEpoch;
    content.replaceChildren();placement.replaceChildren();placement.hidden=true;copyFallback.replaceChildren();copyFallback.hidden=true;
    closeContext(old);
    actionNotice.replaceChildren();actionNotice.hidden=true;
    try{old?.controller?.teardown();}catch{/* Captured route cleanup only. */}
    try{old?.lease?.dispose();}catch{/* Continue independent cleanup. */}
  }
  function navHistory(next,replace=false){
    if(!alive())return false;
    try{const marked=!replace||window.history.state?.aiqNavigator===1;window.history[replace?"replaceState":"pushState"]({aiqNavigator:marked?1:0},"",next);return alive();}
    catch{announce("The page address could not be updated.");return false;}
  }
  function navigate(next,{replace=false,pop=false,focus=true,automatic=false,reason=""}={}){
    let parsed;try{parsed=route(next);}catch{announce("Choose a navigator page and an absolute directory.");return;}
    if(parsed.path==="/asks"&&!parsed.dir&&!parsed.all)parsed=route("/asks?all=true");
    if(!alive())return;if(!pop&&!navHistory(parsed.next,replace))return;
    scopeReason=automatic?(reason==="directory fallback"?"directory fallback":reason==="most recent found"?"most recent found":"most recent"):"";render(parsed,focus);
  }
  function selectionChanged(op,value){
    if(!active(op))return;
    title.hidden=true;
    workersLabel.hidden=true;liveLabel.hidden=true;
    showAskOrigin(value);
    const next=address("/sessions/"+value.provider+"/"+value.id,currentAddress.dir,currentAddress.all);
    const previous=selected(currentAddress);
    if(previous&&previous.provider===value.provider&&previous.id===value.id)return;
    if(currentAddress.anchor&&(!previous||previous.provider!==value.provider||previous.id!==value.id)){navigate(next);return;}
    if(next!==currentAddress.next){if(!navHistory(next))throw refused();if(active(op)){currentAddress=route(next);title.textContent="Conversation";windowLabel.hidden=false;document.title=value.provider+" · "+value.id+" — aiq";}}
  }
  async function loadDirectories(op,parsed){
    try{
      const envelope=await op.lease.read("directories",{catalog:true});if(!active(op))return;
      const value=captureDirectories(envelope.data,limits);if(!active(op))return;
      const fragment=document.createDocumentFragment(),placeholder=node("option","","Choose a recent directory");placeholder.value="";fragment.append(placeholder);
      for(const item of value.directories){const option=node("option","",item.path);option.value=item.path;fragment.append(option);}
      if(!active(op))return;recent.replaceChildren(fragment);recent.disabled=value.directories.length===0;
      recent.value=value.directories.some(item=>item.path===parsed.dir)?parsed.dir:"";
      const full=value.source!=="launch metadata";
      catalogStatus.textContent=full?`Directory catalog · ${value.scanned} of ${value.of} candidates scanned${value.partial?" · partial results":""}${value.metadata_known?"":" · launch metadata unavailable"}${value.unknown_directories?` · ${value.unknown_directories} sessions without a known directory`:""}`:
        value.directories.length?"Recent directories · launch metadata":"No recent launch directories. Enter an absolute directory.";
      const summary=node("summary","",`Known directories (${value.directories.length})`),list=node("ul","nav-directory-branches"),stack=[];
      for(const item of value.directories.slice().sort((a,b)=>a.path<b.path?-1:a.path>b.path?1:0)){
        while(stack.length&&!item.path.startsWith(stack.at(-1).path==="/"?"/":stack.at(-1).path+"/"))stack.pop();
        const li=node("li","nav-directory-branch"),link=node("a","",item.path),children=node("ul","nav-directory-branches");
        li.classList.add("nav-directory-depth-"+Math.min(stack.length,4));
        const next=address(parsed.path==="/asks"?"/asks":"/sessions",item.path);link.href=next;link.rel="noreferrer";
        link.addEventListener("click",event=>{if(!active(op)||event.button!==0||event.ctrlKey||event.metaKey||event.altKey||event.shiftKey)return;event.preventDefault();op.scopeTouched=true;navigate(next);});
        li.append(link);if(full)li.append(node("span","nav-directory-count",` · ${item.sessions} sessions found · ${item.workers} workers found`));
        li.append(children);(stack.at(-1)?.children??list).append(li);stack.push({path:item.path,children});
      }
      if(!active(op))return;directoryTree.replaceChildren(summary,list);directoryTree.hidden=false;
      const defaultDir=full?value.default_dir:value.directories[0]?.path;
      if(parsed.path==="/sessions"&&!parsed.all&&!parsed.dir&&!selected(parsed)&&!op.scopeTouched&&defaultDir){
        navigate(address("/sessions",defaultDir),{replace:true,focus:false,automatic:true,reason:full?(value.default_reason==="directory fallback"?"directory fallback":value.partial?"most recent found":value.default_reason):""});
      }
    }catch{if(active(op)){recent.disabled=true;catalogStatus.textContent="Directory catalog unavailable. Enter an absolute directory.";}}
  }
  function sameAsk(a,b){return Boolean(a&&b&&a.askId===b.askId&&a.provider===b.provider&&a.id===b.id);}
  function closeContext(op){
    if(!op)return;op.contextValue=null;try{op.contextController?.teardown();}catch{}op.contextController=null;
    if(op.contextRoot){op.contextRoot.hidden=true;op.contextRoot.replaceChildren();op.contextRoot.remove();}op.asksLayout?.setAttribute("data-context","false");
  }
  function askSelected(op,value){
    if(!active(op))return;
    const changed=!sameAsk(op.selectedAsk,value);if(changed){++placementEpoch;closeContext(op);}op.selectedAsk=value;
    if(value&&sameAsk(op.reopenContext,value)){const requested=op.reopenContext;op.reopenContext=null;void place(op,requested);}else if(changed&&value&&["claude","codex","agy"].includes(value.provider)&&/^[0-9a-fA-F-]{36}$/.test(value.id))void place(op,value);
  }
  async function place(op,value){
    if(!active(op)||!sameAsk(op.selectedAsk,value)||!op.contextRoot)return;
    closeContext(op);op.contextValue=value;const epoch=++placementEpoch;
    const valid=()=>active(op)&&epoch===placementEpoch&&op.contextValue===value&&sameAsk(op.selectedAsk,value);
    const heading=node("h2","nav-ask-context-heading","Conversation context"),close=button("Close context"),contextStatus=node("p","nav-ask-context-status","Locating the ask’s original turn…"),body=node("div","nav-ask-context-body");
    const tools=node("div","nav-ask-context-tools");tools.append(heading,close);op.contextRoot.replaceChildren(tools,contextStatus,body);const host=content.querySelector(".nav-asks-inbox-detail");if(!host)return;host.append(op.contextRoot);op.contextRoot.hidden=false;op.asksLayout.setAttribute("data-context","true");
    close.addEventListener("click",()=>{if(active(op)){++placementEpoch;closeContext(op);op.controller.focusSelected();}});
    let anchor=null;
    try{
      const response=await op.lease.read("placement",{askId:value.askId});if(!valid())return;
      const d=response.data;
      if(!d||d.provider!==value.provider||d.session_id!==value.id||!["none","time","time+text"].includes(d.confidence)||
        (d.anchor!==null&&(!Number.isSafeInteger(d.anchor)||d.anchor<1||d.anchor>9999999999999))||((d.anchor===null)!==(d.confidence==="none")))throw refused();
      anchor=d.anchor;
    }catch{}
    if(!valid())return;
    contextStatus.textContent=anchor===null?"Original turn not located. Showing the session’s latest conversation.":"Showing the ask’s original conversation turn.";
    const full=node("a","nav-ask-full-conversation","Open full conversation");full.href=address("/sessions/"+value.provider+"/"+value.id,currentAddress.dir,currentAddress.all,anchor??0);full.rel="noreferrer";
    full.addEventListener("click",event=>{if(!valid()||event.button!==0||event.ctrlKey||event.metaKey||event.altKey||event.shiftKey)return;event.preventDefault();askOrigin={...value,next:currentAddress.next,located:anchor!==null};navigate(full.getAttribute("href"));});tools.append(full);
    try{
      const child=createSessionController(document,{window,idPrefix:"nav-ask-context-"+(++generation),limits,conversationOnly:true,prose:true,autoFollow:false,readingControls:false,readCommands:false,resumeCommands:false,
        detailQuery:{last:5,markup:marks.checked,anchor:anchor??0},readSessions:()=>Promise.reject(refused()),readConversation:(q,s)=>op.lease.read("conversation",q,s),copyText:t=>copy(t,op),announce:text=>{if(valid())announce(text);},onSelection:()=>{}});
      if(!valid()){child.teardown();return;}op.contextController=child;body.append(child.fragment);const contextRefresh=body.querySelectorAll(".nav-controller-refresh")[1];if(contextRefresh){contextRefresh.textContent="Refresh context";tools.append(contextRefresh);}if(!child.mount()){child.teardown();return;}child.selectConversation({provider:value.provider,id:value.id});
    }catch{if(valid())contextStatus.textContent="Conversation context unavailable. Try opening the full conversation.";}
  }
  async function performAskAction(op,value){
    if(!active(op)||typeof actions?.ask!=="function"||actionFlight)throw refused();
    const token={};actionFlight=token;actionNotice.hidden=false;actionNotice.textContent=value.action==="dismiss"?"Dismissing ask…":"Restoring ask…";
    try{
      const response=await actions.ask(value),d=response?.data;
      if(!d||d.id!==value.id||d.state!==(value.action==="dismiss"?"dismissed":"open")||typeof d.revision!=="string"||!/^[0-9a-f]{64}$/.test(d.revision))throw refused();
      if(!active(op))return;
      const undo=button("Undo"),message=node("span","",value.action==="dismiss"?"Ask dismissed.":"Ask restored.");actionNotice.replaceChildren(message);if(value.action==="dismiss")actionNotice.append(undo);
      const inverse={id:d.id,revision:d.revision,action:value.action==="dismiss"?"restore":"dismiss"};
      undo.addEventListener("click",()=>{if(!active(op)||actionFlight)return;undo.disabled=true;void performAskAction(op,inverse).catch(()=>{});});
      op.controller.refresh();
    }catch(error){
      if(active(op)){
        actionNotice.textContent=error?.code==="conflict"?"This ask changed. Asks refreshed; review it before trying again.":"The update could not be confirmed. Refreshing Asks to check; no retry was sent.";
        op.controller.refresh();
      }
    }finally{if(actionFlight===token)actionFlight=null;}
  }
  function toggleMarks(){
    if(!alive())return;reopenContext=current?.contextValue??null;render(currentAddress,false);announce(marks.checked?"Markdown symbols shown. Shortcut: m.":"Markdown symbols hidden. Shortcut: m.");
  }
  function showAskOrigin(value){const matches=value&&askOrigin&&value.id===askOrigin.id&&value.provider===askOrigin.provider;askReturn.hidden=!matches;if(matches)askContext.textContent=`Ask ${askOrigin.askId} · ${askOrigin.located?"Showing the located turn.":"Turn not located; showing the session."}`;}
  function help(){
    const section=node("section","nav-help");section.append(node("h3","","Read your sessions"),node("p","","Choose a directory to list its sessions, then open a conversation. Search filters only the results already loaded. Refresh reads a new snapshot; it never types into or resumes a session."),node("h3","","Asks"),node("p","","Choose a directory or explicitly select All Asks. Open a question’s conversation to locate its turn. When placement is uncertain, open the session yourself."),node("h3","","Login and privacy"),node("p","","Open this tab with aiq ui. Its login expires after 30 minutes without successful requests or eight hours total. Forget this tab clears its saved login; aiq ui --revoke signs out every browser. After a daemon restart or a terminal connection failure, run aiq ui again. Remote browsers and SSH forwarding are unsupported."),node("p","","Conversation window, theme, Markdown marks and keyboard choices last for this open page. Tab always belongs to your browser. Copy requires an explicit action. Session control remains in the terminal."));
    return section;
  }
  function render(parsed,focus){
    clearRoute();if(!alive())return;
    currentAddress=parsed;if(parsed.dir)lastDirectory=parsed.dir;
    root.setAttribute("data-nav-page",parsed.path);machine.hidden=parsed.path==="/quotas";
    showAskOrigin(selected(parsed));
    if(parsed.path==="/asks"||parsed.path==="/sessions")sharedScope={all:parsed.all,dir:parsed.dir};
    const shownScope=parsed.path==="/long"?{all:!parsed.dir,dir:parsed.dir}:sharedScope;
    scopeSummary.textContent=shownScope.all?(parsed.path==="/long"?"All workspaces":"All projects"):shownScope.dir.split("/").filter(Boolean).at(-1)||"Choose project";scopeSummary.title=shownScope.dir;scopeMenu.open=false;
    const restored=parsed.path==="/asks"?askViews.get(askKey(parsed)):null;
    const op={retired:false,controller:null,lease:null,scopeTouched:false,asksKey:parsed.path==="/asks"?askKey(parsed):null,filter:restored?.filter??search.value,restoreAllowed:Boolean(restored),reopenContext};current=op;reopenContext=null;op.lease=broker.route();
    if(restored)search.value=restored.filter;
    if(!active(op))return;
    dirInput.value=parsed.dir||lastDirectory;allInput.checked=parsed.path==="/asks"&&parsed.all;dirInput.disabled=false;
    allLabel.hidden=true;workersLabel.hidden=Boolean(selected(parsed))||["/asks","/long","/help","/quotas"].includes(parsed.path);marksLabel.hidden=false;
    windowLabel.hidden=!selected(parsed)&&!(parsed.path==="/long"&&parsed.dir);windowSelect.value=String(parsed.anchor?50:lastPrompts);
    recent.replaceChildren();recent.disabled=true;recentLabel.hidden=parsed.path==="/help"||parsed.path==="/long";catalogStatus.textContent="";directoryTree.replaceChildren();directoryTree.hidden=true;allSessions.hidden=["/long","/help","/quotas"].includes(parsed.path);
    scopeMenu.hidden=["/asks","/long","/help","/quotas"].includes(parsed.path);searchLabel.hidden=parsed.path==="/help"||parsed.path==="/quotas";search.placeholder=parsed.path==="/asks"?"Filter loaded asks":"Filter loaded sessions";
    readingOptions.hidden=windowLabel.hidden;back.hidden=!selected(parsed)&&parsed.path!=="/long";
    title.textContent=parsed.path==="/long"?"Workspaces":parsed.path==="/asks"?"Asks":parsed.path==="/quotas"?"Quotas":parsed.path==="/help"?"Help":selected(parsed)?"Conversation":"Sessions";
    title.hidden=Boolean(selected(parsed))||parsed.path==="/quotas";
    controls.hidden=workersLabel.hidden&&liveLabel.hidden&&windowLabel.hidden&&back.hidden;
    liveLabel.hidden=parsed.path!=="/sessions"||Boolean(selected(parsed));
    if(parsed.path==="/sessions"&&scopeReason)title.textContent+=" · "+scopeReason;
    document.title=title.textContent+" — aiq";
    for(const[path,a]of links){a.href=tabAddress(path);if(path==="/sessions"&&parsed.path.startsWith("/sessions")||path===parsed.path)a.setAttribute("aria-current","page");else a.removeAttribute("aria-current");}
    if(parsed.path==="/help"){content.append(help());if(focus)title.focus({preventScroll:true});return;}
    if(parsed.path.startsWith("/sessions")){recent.replaceChildren();recent.disabled=true;catalogStatus.textContent="Loading recent directories…";void loadDirectories(op,parsed);}
    const announceCurrent=text=>{if(active(op)){announce(text);if(text==="Asks updated"&&restored&&op.restoreAllowed)window.requestAnimationFrame(()=>{if(!active(op)||!op.restoreAllowed||document.activeElement!==title)return;op.restoreAllowed=false;if(op.controller.focusSelected())window.scrollTo({top:restored.scroll,behavior:"instant"});});}};
    if(parsed.path==="/quotas"){
      op.controller=createQuotaView(document,{window,limits,readQuotas:(q,s)=>op.lease.read("quotas",q,s),copyText:t=>copy(t,op),announce:announceCurrent});
    }else if(parsed.path==="/long"){
      op.controller=createLongView(document,{window,dir:parsed.dir,idPrefix:"nav-long-"+(++generation),limits,
        detailQuery:{last:lastPrompts,markup:marks.checked,anchor:0},shortcutsEnabled:()=>shortcuts,
        readLong:(q,s)=>op.lease.read("long",q,s),readAsks:(q,s)=>op.lease.read("asks",q,s),readConversation:(q,s)=>op.lease.read("conversation",q,s),readState:(q,s)=>op.lease.read("state",q,s),
        copyText:t=>copy(t,op),announce:announceCurrent,onWorkspace:dir=>{if(active(op))navigate(address("/long",dir));},
        onConversation:(value,dir)=>{if(active(op))navigate(address("/sessions/"+value.provider+"/"+value.id,dir));}});
    }else if(parsed.path==="/asks"){
      if(!parsed.all&&!parsed.dir){content.append(node("p","nav-empty","Choose a directory, or explicitly select All Asks."));if(focus)dirInput.focus();return;}
      op.controller=createAsksController(document,{window,idPrefix:"nav-app-asks-"+(++generation),limits,
        inbox:true,viewState:restored?.value,
        readAsks:(q,s)=>op.lease.read("asks",q,s),copyText:t=>copy(t,op),announce:announceCurrent,
        onConversation:v=>{if(active(op))navigate(address("/sessions/"+v.provider+"/"+v.id,parsed.dir,parsed.all));},
        onSelection:v=>askSelected(op,v),onAction:typeof actions?.ask==="function"?v=>performAskAction(op,v):undefined,
        onPlacement:v=>{void place(op,v);}});
    }else{
      op.controller=createSessionController(document,{window,idPrefix:"nav-app-session-"+(++generation),limits,
        readingControls:true,prose:true,readCommands:true,resumeCommands:true,autoFollow:true,shortcutsEnabled:()=>shortcuts,
        detailQuery:{last:parsed.anchor?50:lastPrompts,markup:marks.checked,anchor:parsed.anchor},
        readSessions:(q,s)=>op.lease.read("sessions",q,s),readConversation:(q,s)=>op.lease.read("conversation",q,s),readState:(q,s)=>op.lease.read("state",q,s),
        onNewest:v=>{if(active(op))navigate(address("/sessions/"+v.provider+"/"+v.id,parsed.dir,parsed.all),{replace:true});},
        copyText:t=>copy(t,op),announce:announceCurrent,onSelection:v=>selectionChanged(op,v)});
    }
    if(!active(op)){op.controller.teardown();return;}
    if(parsed.path==="/asks"){
      op.asksLayout=node("div","nav-asks-layout");op.contextRoot=node("aside","nav-ask-context");op.contextRoot.setAttribute("aria-label","Original conversation context");op.contextRoot.hidden=true;op.asksLayout.append(op.controller.fragment,op.contextRoot);content.append(op.asksLayout);
    }else content.append(op.controller.fragment);
    if(!active(op)||!op.controller.mount()){try{op.controller.teardown();}catch{}return;}
    if(parsed.path==="/quotas"){normalizeStatuses();const heading=content.querySelector(".nav-quotas-heading");if(focus&&active(op)&&heading){heading.tabIndex=-1;heading.focus({preventScroll:true});}return;}
    if(parsed.path==="/long"){normalizeStatuses();if(focus&&active(op))title.focus({preventScroll:true});return;}
    if(parsed.path==="/asks")op.controller.loadScope({mode:parsed.all?"all":"directory",dir:parsed.dir});
    else{
      if(parsed.all)op.controller.loadSessions({dir:"",canonicalDir:"",workers:workers.checked,all:true});
      else if(parsed.dir)op.controller.loadSessions({dir:parsed.dir,canonicalDir:parsed.dir,workers:workers.checked});
      const pick=selected(parsed);if(pick)op.controller.selectConversation(pick);
      if(!pick&&!parsed.dir&&!parsed.all)announce("Choose an absolute directory to load sessions.");
    }
    normalizeStatuses();if(focus&&active(op))title.focus({preventScroll:true});
  }
  function tabAddress(path){return path==="/help"||path==="/quotas"?address(path):path==="/long"?address(path):address(path,sharedScope.dir,sharedScope.all);}
  function goBack(){try{if(window.history.state?.aiqNavigator===1&&window.history.length>1){window.history.back();return;}}catch{}const all=Boolean(currentAddress?.all&&currentAddress.path.startsWith("/sessions"));navigate(address("/sessions",all?"":currentAddress?.dir||lastDirectory,all),{replace:true});}
  function keys(e){
    if(e.defaultPrevented||e.altKey||e.ctrlKey||e.metaKey||["INPUT","TEXTAREA","SELECT"].includes(e.target.tagName)||e.target.isContentEditable)return;
    if(e.key==="Escape"){e.preventDefault();if(helpDialog.open)helpDialog.close();else if(search.value){search.value="";filter();announce("Search cleared.");}else goBack();return;}
    if(shortcuts&&e.key==="m"){e.preventDefault();marks.checked=!marks.checked;toggleMarks();return;}
    if(!e.target.closest(".nav-controller,.nav-asks,.nav-feed,.nav-long"))return;
    if(shortcuts&&e.key==="/"){e.preventDefault();search.focus();return;}
    if(shortcuts&&e.key==="?"){e.preventDefault();helpDialog.showModal();return;}
    if(navigateNavigatorRows(e,document.activeElement,shortcuts))roving();
  }
  function mount(){
    if(disposed)return false;if(mounted)return alive();if(!root.isConnected)return false;
    try{if(!isCurrent())return false;}catch{return false;}
    mounted=true;owners.set(root,owner);root.replaceChildren(shell);if(!alive()){dispose();return false;}root.hidden=false;
    listen(toolbar,"submit",e=>{e.preventDefault();try{const dir=directory(dirInput.value);search.value="";navigate(address(currentAddress.path==="/long"?"/long":currentAddress.path==="/asks"?"/asks":"/sessions",dir));}catch{announce("Enter a complete absolute directory without dot segments.");}});
    listen(dirInput,"input",()=>{if(current)current.scopeTouched=true;});
    listen(recent,"change",()=>{if(!recent.value)return;try{const dir=directory(recent.value);search.value="";navigate(address(currentAddress.path==="/asks"?"/asks":"/sessions",dir));}catch{announce("Choose an available directory.");}});
    for(const[path,a]of links)listen(a,"click",e=>{if(e.button!==0||e.ctrlKey||e.metaKey||e.shiftKey||e.altKey)return;e.preventDefault();search.value="";navigate(tabAddress(path));});
    listen(brand,"click",e=>{if(e.button!==0||e.ctrlKey||e.metaKey||e.shiftKey||e.altKey)return;e.preventDefault();navigate("/asks?all=true");});
    listen(forget,"click",()=>{if(hasAuth)options.auth.forget();});
    listen(backToAsks,"click",()=>{if(askOrigin)navigate(askOrigin.next);});
    listen(header,"keydown",e=>{if(e.key!=="Escape")return;for(const menu of [scopeMenu,tabMenu])if(menu.open&&menu.contains(e.target)){e.preventDefault();menu.open=false;menu.children[0].focus();}});
    listen(document,"click",e=>{for(const menu of [scopeMenu,tabMenu])if(menu.open&&!menu.contains(e.target))menu.open=false;});
    listen(document,"pointerdown",()=>{if(current)current.restoreAllowed=false;});listen(window,"wheel",()=>{if(current)current.restoreAllowed=false;});
    listen(allInput,"change",()=>{search.value="";navigate(address("/asks",allInput.checked?"":lastDirectory,allInput.checked));});
    listen(allSessions,"click",()=>{if(current)current.scopeTouched=true;search.value="";navigate(address(currentAddress.path==="/asks"?"/asks":"/sessions","",true));});
    listen(search,"input",filter);listen(liveOnly,"change",filter);listen(workers,"change",()=>render(currentAddress,false));listen(marks,"change",toggleMarks);
    listen(clearFilter,"click",()=>{search.value="";filter();search.focus();});
    listen(windowSelect,"change",()=>{const value=windowSelect.value;if(!["50","100","500","all"].includes(value)){announce("Choose a conversation window.");return;}lastPrompts=value==="all"?"all":Number(value);navigate(address(currentAddress.path,currentAddress.dir,currentAddress.all),{replace:true,focus:false});});
    listen(search,"keydown",e=>{if(e.key==="Escape"){e.preventDefault();search.value="";filter();announce("Search cleared.");}});
    listen(theme,"change",()=>{if(["system","light","dark"].includes(theme.value))document.documentElement.dataset.navTheme=theme.value;});
    listen(back,"click",goBack);listen(helpButton,"click",()=>helpDialog.showModal());listen(closeHelp,"click",()=>helpDialog.close());listen(shortcutInput,"change",()=>{shortcuts=shortcutInput.checked;});
    listen(document,"keydown",keys);removes.push(bindNavigatorRoving(content,alive));
    listen(window,"popstate",()=>{search.value="";navigate(window.location.pathname+window.location.search,{pop:true});});
    const location=window.location.pathname+window.location.search;
    navigate(window.location.pathname==="/"?initial||"/asks?all=true":location,{replace:true});
    return alive();
  }
  function dispose(){
    if(disposed)return;disposed=true;
    const old=current;if(old)old.retired=true;current=null;++placementEpoch;
    shell.remove();if(owners.get(root)===owner){root.hidden=true;if(owners.get(root)===owner){document.title="aiq navigator";if(owners.get(root)===owner)owners.delete(root);}}
    for(const remove of removes){try{remove();}catch{}}removes.length=0;
    closeContext(old);
    actionNotice.replaceChildren();actionNotice.hidden=true;
    try{old?.controller?.teardown();}catch{}
    try{old?.lease?.dispose();}catch{}
    broker.close();askViews.clear();askOrigin=null;content.replaceChildren();placement.replaceChildren();copyFallback.replaceChildren();
  }
  return Object.freeze({mount,dispose});
}
