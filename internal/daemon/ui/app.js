import {directory,encodeForm,route} from "./auth_codec.js";
import {createReadBroker} from "./read_broker.js";
import {createSessionController} from "./session_controller.js";
import {createAsksController} from "./asks_controller.js";

const owners=new WeakMap();
const copyFlights=new WeakMap(); // One physical clipboard operation per document.
const refused=()=>new TypeError("Navigator view unavailable");
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
  if (!window || document.defaultView!==window || root?.ownerDocument!==document || typeof isCurrent!=="function") throw refused();
  const limits={};for(const key of ["rows","arrayEntries","stringUnits"]){const n=options.limits?.[key];if(!Number.isSafeInteger(n)||n<=0)throw refused();limits[key]=n;}
  const initial=options.initialNext,broker=createReadBroker(readers),owner={},removes=[];
  let mounted=false,disposed=false,current=null,generation=0n,placementEpoch=0n,lastDirectory="",currentAddress=null,shortcuts=true;
  const node=(tag,cls,text)=>{const n=document.createElement(tag);if(cls)n.className=cls;if(text!==undefined)n.textContent=text;return n;};
  const button=(text,cls="")=>{const n=node("button",cls,text);n.type="button";return n;};
  const shell=node("div","nav-shell"),header=node("header","nav-header"),nav=node("nav","nav-tabs");nav.setAttribute("aria-label","Navigator pages");
  const title=node("h2","nav-page-title","Sessions");title.tabIndex=-1;
  const links=new Map();for(const [path,label]of [["/sessions","Sessions"],["/asks","Asks"],["/help","Help"]]){const a=node("a","nav-tab",label);a.href=path;a.rel="noreferrer";links.set(path,a);nav.append(a);}
  const back=button("← Back","nav-back");header.append(back,nav);
  const toolbar=node("form","nav-toolbar"),dirLabel=node("label","nav-directory-label","Directory"),dirInput=node("input","nav-directory"),apply=button("Show directory");
  dirInput.type="text";dirInput.autocomplete="off";dirInput.spellcheck=false;dirInput.placeholder="/absolute/project/path";dirLabel.append(dirInput);apply.type="submit";
  const allLabel=node("label","nav-toggle"),allInput=node("input");allInput.type="checkbox";allLabel.append(allInput,document.createTextNode(" All Asks"));
  toolbar.append(dirLabel,apply,allLabel);
  const controls=node("div","nav-controls"),searchLabel=node("label","nav-search-label","Search loaded results"),search=node("input","nav-search");search.type="search";search.autocomplete="off";searchLabel.append(search);
  const workersLabel=node("label","nav-toggle"),workers=node("input");workers.type="checkbox";workersLabel.append(workers,document.createTextNode(" Workers"));
  const marksLabel=node("label","nav-toggle"),marks=node("input");marks.type="checkbox";marksLabel.append(marks,document.createTextNode(" Markdown marks"));
  const themeLabel=node("label","nav-theme-label","Theme"),theme=node("select","nav-theme");for(const [value,label]of [["system","System"],["light","Light"],["dark","Dark"]]){const o=node("option","",label);o.value=value;theme.append(o);}themeLabel.append(theme);
  controls.append(searchLabel,workersLabel,marksLabel,themeLabel);
  const status=node("p","nav-app-status");status.setAttribute("role","status");status.setAttribute("aria-live","polite");
  const note=node("p","nav-snapshot-note","Snapshot views · refresh to read changes. Live session state is not shown here yet.");
  const placement=node("aside","nav-placement");placement.hidden=true;
  const copyFallback=node("aside","nav-copy-fallback");copyFallback.hidden=true;
  const content=node("div","nav-app-content");
  const helpDialog=node("dialog","nav-key-dialog"),closeHelp=button("Close keyboard help"),keyText=node("p","","Tab stays with your browser. In a list: j/k or arrows move, Home/End move to an edge, Enter opens. / searches loaded results; Esc clears search or goes back. In a focused conversation: n/p move between prompt headings.");
  const shortcutLabel=node("label","nav-toggle"),shortcutInput=node("input");shortcutInput.type="checkbox";shortcutInput.checked=true;shortcutLabel.append(shortcutInput,document.createTextNode(" Enable single-key shortcuts"));helpDialog.append(node("h2","","Keyboard help"),keyText,shortcutLabel,closeHelp);
  const helpButton=button("? Keyboard help","nav-key-help");header.append(helpButton);
  shell.append(header,title,toolbar,controls,note,status,placement,copyFallback,content,helpDialog);
  function alive(){if(!mounted||disposed||owners.get(root)!==owner||!root.isConnected)return false;try{return Boolean(isCurrent())&&!disposed&&owners.get(root)===owner;}catch{return false;}}
  function active(op){return alive()&&current===op&&!op.retired;}
  function listen(target,type,fn){const wrapped=e=>{if(alive())fn(e);};removes.push(()=>target.removeEventListener(type,wrapped));target.addEventListener(type,wrapped);}
  function normalizeStatuses(){for(const n of content.querySelectorAll('[role="status"]')){n.removeAttribute("role");n.setAttribute("aria-live","off");}}
  function announce(text){if(!alive())return;normalizeStatuses();filter();status.textContent=text;}
  function filter(){
    if(!alive())return;const q=search.value.toLocaleLowerCase();
    for(const row of content.querySelectorAll(".nav-controller-row,.nav-asks-item"))row.hidden=q!==""&&!row.textContent.toLocaleLowerCase().includes(q);
    roving();
  }
  function roving(){
    for(const group of content.querySelectorAll(".nav-controller-rows,.nav-asks-region")){
      const rows=[...group.querySelectorAll(".nav-controller-row,.nav-asks-item")].filter(n=>!n.hidden);
      const focused=rows.find(n=>n.contains(document.activeElement)),chosen=focused??rows.find(n=>n.dataset.current==="true")??rows[0];
      for(const row of rows){const yes=row===chosen;row.dataset.current=String(yes);const controls=[...row.querySelectorAll("button,a")];for(const c of controls)c.tabIndex=yes?0:-1;}
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
    const old=current;if(old)old.retired=true;current=null;++placementEpoch;
    content.replaceChildren();placement.replaceChildren();placement.hidden=true;copyFallback.replaceChildren();copyFallback.hidden=true;
    try{old?.controller?.teardown();}catch{/* Captured route cleanup only. */}
    try{old?.lease?.dispose();}catch{/* Continue independent cleanup. */}
  }
  function navHistory(next,replace=false){
    if(!alive())return false;
    try{const marked=!replace||window.history.state?.aiqNavigator===1;window.history[replace?"replaceState":"pushState"]({aiqNavigator:marked?1:0},"",next);return alive();}
    catch{announce("The page address could not be updated.");return false;}
  }
  function navigate(next,{replace=false,pop=false,focus=true}={}){
    let parsed;try{parsed=route(next);}catch{announce("Choose a navigator page and an absolute directory.");return;}
    if(!alive())return;if(!pop&&!navHistory(parsed.next,replace))return;
    render(parsed,focus);
  }
  function selectionChanged(op,value){
    if(!active(op))return;
    const next=address("/sessions/"+value.provider+"/"+value.id,currentAddress.dir);
    const previous=selected(currentAddress);
    if(previous&&previous.provider===value.provider&&previous.id===value.id)return;
    if(currentAddress.anchor&&(!previous||previous.provider!==value.provider||previous.id!==value.id)){navigate(next);return;}
    if(next!==currentAddress.next){if(!navHistory(next))throw refused();if(active(op)){currentAddress=route(next);title.textContent="Conversation";document.title=value.provider+" · "+value.id+" — aiq";}}
  }
  async function place(op,value){
    if(!active(op))return;
    const epoch=++placementEpoch;placement.hidden=false;placement.replaceChildren(node("p","","Locating the ask’s turn…"));
    let located=null,failed=false;
    try{
      const response=await op.lease.read("placement",{askId:value.askId});if(!active(op)||epoch!==placementEpoch)return;
      const d=response.data;
      if(!d||d.provider!==value.provider||d.session_id!==value.id||!["none","time","time+text"].includes(d.confidence)||
        (d.anchor!==null&&(!Number.isSafeInteger(d.anchor)||d.anchor<1||d.anchor>9999999999999))||((d.anchor===null)!==(d.confidence==="none")))throw refused();
      located=d.anchor;
    }catch{failed=true;}
    if(!active(op)||epoch!==placementEpoch)return;
    if(located!==null){navigate(address("/sessions/"+value.provider+"/"+value.id,currentAddress.dir,false,located));return;}
    const open=button("Open the session");placement.replaceChildren(node("p","",failed?"Turn placement unavailable; you can open the session.":"Turn not found; you can open the session."),open);
    open.addEventListener("click",()=>{if(active(op)&&epoch===placementEpoch)navigate(address("/sessions/"+value.provider+"/"+value.id,currentAddress.dir));},{once:true});
  }
  function help(){
    const section=node("section","nav-help");section.append(node("h3","","Read your sessions"),node("p","","Choose a directory to list its sessions, then open a conversation. Search filters only the results already loaded. Refresh reads a new snapshot; it never types into or resumes a session."),node("h3","","Asks"),node("p","","Choose a directory or explicitly select All Asks. Open a question’s conversation to locate its turn. When placement is uncertain, open the session yourself."),node("h3","","Login and privacy"),node("p","","Open this tab with aiq ui. Its login expires after 30 minutes without successful requests or eight hours total. Forget this tab clears its saved login; aiq ui --revoke signs out every browser. After a daemon restart or a terminal connection failure, run aiq ui again. Remote browsers and SSH forwarding are unsupported."),node("p","","Theme, Markdown marks and keyboard choices last for this open page. Tab always belongs to your browser. Copy requires an explicit action. Session control remains in the terminal."));
    return section;
  }
  function render(parsed,focus){
    clearRoute();if(!alive())return;
    currentAddress=parsed;if(parsed.dir)lastDirectory=parsed.dir;
    const op={retired:false,controller:null,lease:null};current=op;op.lease=broker.route();
    if(!active(op))return;
    dirInput.value=parsed.dir||lastDirectory;allInput.checked=parsed.path==="/asks"&&parsed.all;dirInput.disabled=allInput.checked;
    allLabel.hidden=parsed.path!=="/asks";workersLabel.hidden=parsed.path==="/asks"||parsed.path==="/help";marksLabel.hidden=parsed.path==="/asks"||parsed.path==="/help";
    title.textContent=parsed.path==="/asks"?"Asks":parsed.path==="/help"?"Help":selected(parsed)?"Conversation":"Sessions";
    document.title=title.textContent+" — aiq";
    for(const[path,a]of links){a.href=address(path,parsed.dir||lastDirectory);if(path==="/sessions"&&parsed.path.startsWith("/sessions")||path===parsed.path)a.setAttribute("aria-current","page");else a.removeAttribute("aria-current");}
    if(parsed.path==="/help"){content.append(help());if(focus)title.focus({preventScroll:true});return;}
    const announceCurrent=text=>{if(active(op))announce(text);};
    if(parsed.path==="/asks"){
      if(!parsed.all&&!parsed.dir){content.append(node("p","nav-empty","Choose a directory, or explicitly select All Asks."));if(focus)dirInput.focus();return;}
      op.controller=createAsksController(document,{window,idPrefix:"nav-app-asks-"+(++generation),limits,
        readAsks:(q,s)=>op.lease.read("asks",q,s),copyText:t=>copy(t,op),announce:announceCurrent,
        onConversation:v=>{if(active(op))navigate(address("/sessions/"+v.provider+"/"+v.id,parsed.dir));},
        onPlacement:v=>{void place(op,v);}});
    }else{
      op.controller=createSessionController(document,{window,idPrefix:"nav-app-session-"+(++generation),limits,
        readingControls:true,shortcutsEnabled:()=>shortcuts,
        detailQuery:{last:50,markup:marks.checked,anchor:parsed.anchor},
        readSessions:(q,s)=>op.lease.read("sessions",q,s),readConversation:(q,s)=>op.lease.read("conversation",q,s),
        copyText:t=>copy(t,op),announce:announceCurrent,onSelection:v=>selectionChanged(op,v)});
    }
    if(!active(op)){op.controller.teardown();return;}
    content.append(op.controller.fragment);
    if(!active(op)||!op.controller.mount()){try{op.controller.teardown();}catch{}return;}
    if(parsed.path==="/asks")op.controller.loadScope({mode:parsed.all?"all":"directory",dir:parsed.dir});
    else{
      if(parsed.dir)op.controller.loadSessions({dir:parsed.dir,canonicalDir:parsed.dir,workers:workers.checked});
      const pick=selected(parsed);if(pick)op.controller.selectConversation(pick);
      if(!pick&&!parsed.dir)announce("Choose an absolute directory to load sessions.");
    }
    normalizeStatuses();if(focus&&active(op))title.focus({preventScroll:true});
  }
  function goBack(){try{if(window.history.state?.aiqNavigator===1&&window.history.length>1){window.history.back();return;}}catch{}navigate(address("/sessions",currentAddress?.dir||lastDirectory),{replace:true});}
  function keys(e){
    if(e.defaultPrevented||e.altKey||e.ctrlKey||e.metaKey||["INPUT","TEXTAREA","SELECT"].includes(e.target.tagName)||e.target.isContentEditable)return;
    if(e.key==="Escape"){e.preventDefault();if(helpDialog.open)helpDialog.close();else if(search.value){search.value="";filter();announce("Search cleared.");}else goBack();return;}
    if(!shortcuts||!e.target.closest(".nav-controller,.nav-asks,.nav-feed"))return;
    if(e.key==="/"){e.preventDefault();search.focus();return;}
    if(e.key==="?"){e.preventDefault();helpDialog.showModal();return;}
    const group=e.target.closest(".nav-controller-rows,.nav-asks-region");if(!group)return;
    const rows=[...group.querySelectorAll(".nav-controller-row,.nav-asks-item")].filter(n=>!n.hidden),index=rows.findIndex(n=>n.contains(document.activeElement));
    let next=index;if(["j","ArrowDown"].includes(e.key))next=Math.min(rows.length-1,index+1);else if(["k","ArrowUp"].includes(e.key))next=Math.max(0,index-1);else if(["g","Home"].includes(e.key))next=0;else if(["G","End"].includes(e.key))next=rows.length-1;else return;
    const target=rows[next]?.querySelector("button,a");if(target){e.preventDefault();target.focus();roving();}
  }
  function mount(){
    if(disposed)return false;if(mounted)return alive();if(!root.isConnected)return false;
    try{if(!isCurrent())return false;}catch{return false;}
    mounted=true;owners.set(root,owner);root.replaceChildren(shell);if(!alive()){dispose();return false;}root.hidden=false;
    listen(toolbar,"submit",e=>{e.preventDefault();try{const dir=directory(dirInput.value);search.value="";navigate(address(currentAddress.path==="/asks"?"/asks":"/sessions",dir));}catch{announce("Enter a complete absolute directory without dot segments.");}});
    for(const[path,a]of links)listen(a,"click",e=>{if(e.button!==0||e.ctrlKey||e.metaKey||e.shiftKey||e.altKey)return;e.preventDefault();search.value="";navigate(address(path,currentAddress?.dir||lastDirectory));});
    listen(allInput,"change",()=>{search.value="";navigate(address("/asks",allInput.checked?"":lastDirectory,allInput.checked));});
    listen(search,"input",filter);listen(workers,"change",()=>render(currentAddress,false));listen(marks,"change",()=>render(currentAddress,false));
    listen(search,"keydown",e=>{if(e.key==="Escape"){e.preventDefault();search.value="";filter();announce("Search cleared.");}});
    listen(theme,"change",()=>{if(["system","light","dark"].includes(theme.value))document.documentElement.dataset.navTheme=theme.value;});
    listen(back,"click",goBack);listen(helpButton,"click",()=>helpDialog.showModal());listen(closeHelp,"click",()=>helpDialog.close());listen(shortcutInput,"change",()=>{shortcuts=shortcutInput.checked;});
    listen(content,"keydown",keys);listen(content,"focusin",roving);
    listen(window,"popstate",()=>{search.value="";navigate(window.location.pathname+window.location.search,{pop:true});});
    const location=window.location.pathname+window.location.search;
    navigate(window.location.pathname==="/"?initial||"/sessions":location,{replace:true});
    return alive();
  }
  function dispose(){
    if(disposed)return;disposed=true;
    const old=current;if(old)old.retired=true;current=null;++placementEpoch;
    shell.remove();if(owners.get(root)===owner){root.hidden=true;if(owners.get(root)===owner){document.title="aiq navigator";if(owners.get(root)===owner)owners.delete(root);}}
    for(const remove of removes){try{remove();}catch{}}removes.length=0;
    try{old?.controller?.teardown();}catch{}
    try{old?.lease?.dispose();}catch{}
    broker.close();content.replaceChildren();placement.replaceChildren();copyFallback.replaceChildren();
  }
  return Object.freeze({mount,dispose});
}
