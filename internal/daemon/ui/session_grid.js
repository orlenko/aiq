import {directory,encodeForm} from "./auth_codec.js";

const columns=[
  ["workspace","Workspace / directory"],["session","Session"],["provider","Provider"],
  ["asks","Pending asks"],["updated","Last activity"],["status","Status"]
];
const choices={
  provider:[["","All providers"],["claude","Claude"],["codex","Codex"],["agy","Antigravity"]],
  asks:[["","Any count"],["pending","Has pending asks"],["clear","No pending asks"],["unknown","Count unknown"]],
  updated:[["","Any time"],["day","Past 24 hours"],["week","Past 7 days"],["unknown","Time unknown"]],
  status:[["","Any status"],["live","Verified live"],["open","Registered open"],["recent","Recent write"],["ended","Verified ended"],["unknown","Unknown"],["workspace","No listed conversation"]]
};
const liveLabels={live:"Live",open:"Registered open",recent:"Recent write",ended:"Ended",unknown:"Unknown"};
const own=(value,key)=>Object.getOwnPropertyDescriptor(value??{},key)?.value;
const emptyFilters=()=>Object.fromEntries(columns.map(([key])=>[key,""]));
function restore(value){
  const state={sort:"updated",descending:true,filters:emptyFilters()};
  const key=own(value,"sort");
  if(columns.some(([k])=>k===key)&&typeof own(value,"descending")==="boolean"){state.sort=key;state.descending=own(value,"descending");}
  for(const [key]of columns){const text=own(own(value,"filters"),key);if(typeof text!=="string"||text.length>512)continue;if(!choices[key]||choices[key].some(([v])=>v===text))state.filters[key]=text;}
  return state;
}
export function sessionGridTime(value){
  const ms=Date.parse(value);return Number.isFinite(ms)&&value!=="0001-01-01T00:00:00Z"?ms:null;
}
export function sessionGridLocalTime(value){
  const ms=sessionGridTime(value);return ms===null?"Unknown":new Date(ms).toLocaleString(undefined,{year:"numeric",month:"short",day:"numeric",hour:"numeric",minute:"2-digit"});
}

// A semantic HTML table, with one owner composing header and global filters.
// Input rows have already passed captureSessions. No transcript reads happen here.
export function createSessionGrid(document,{data,state,onSelect,onWorkspace,renderDetails,announce,isCurrent}){
  const node=(tag,cls,text)=>{const el=document.createElement(tag);el.className=cls;if(text!==undefined)el.textContent=text;return el;};
  const root=node("div","nav-session-grid"),table=node("table","nav-session-table"),caption=node("caption","nav-grid-caption","Sessions and recorded workspaces. Session ask counts belong to that conversation; workspace-only counts cover asks without a listed conversation. Sort and filter the loaded snapshot.");
  const head=node("thead",""),sortRow=node("tr","nav-grid-headers"),filterRow=node("tr","nav-grid-filters"),body=node("tbody","nav-controller-rows");
  const toolbar=node("div","nav-grid-toolbar"),count=node("p","nav-grid-count"),clear=node("button","nav-grid-clear","Clear column filters");clear.type="button";
  const empty=node("p","nav-grid-empty","No loaded rows match these filters.");empty.hidden=true;
  toolbar.append(count,clear);head.append(sortRow,filterRow);table.append(caption,head,body);root.append(toolbar,table,empty);
  const current=restore(state),headers=new Map(),filters=new Map(),rows=[],removes=[];
  let disposed=false,quick="",liveOnly=false;
  const active=()=>!disposed&&isCurrent();
  function listen(el,type,fn){el.addEventListener(type,fn);removes.push(()=>el.removeEventListener(type,fn));}
  for(const [key,label]of columns){
    const th=node("th","nav-grid-col-"+key),button=node("button","nav-grid-sort",label),mark=node("span","nav-grid-sort-mark");
    th.setAttribute("scope","col");button.type="button";mark.setAttribute("aria-hidden","true");button.append(mark);th.append(button);sortRow.append(th);headers.set(key,{th,button,mark});
    listen(button,"click",()=>{if(!active())return;current.descending=current.sort===key?!current.descending:["asks","updated"].includes(key);current.sort=key;apply();announce(`Sorted by ${label.toLocaleLowerCase()}, ${current.descending?"descending":"ascending"}.`);});
    const cell=node("td","nav-grid-col-"+key),input=node(choices[key]?"select":"input","nav-grid-filter");
    if(choices[key])for(const [value,text]of choices[key]){const option=node("option","",text);option.value=value;input.append(option);}
    else{input.type="search";input.placeholder=key==="workspace"?"Filter path":"Filter session";input.autocomplete="off";input.maxLength=512;}
    input.setAttribute("aria-label",`Filter ${label.toLocaleLowerCase()}`);input.value=current.filters[key];filters.set(key,input);cell.append(input);filterRow.append(cell);
    listen(input,choices[key]?"change":"input",()=>{if(!active())return;current.filters[key]=input.value.slice(0,512);apply();});
  }
  sortRow.append(node("th","nav-grid-col-actions","Actions"));filterRow.append(node("td","nav-grid-col-actions"));
  for(const value of data.sessions){
    const tr=node("tr","nav-controller-row"),item={root:tr,button:null,value};
    const kind=value.kind??"session",workspace=value.workspace||value.activity?.workspace||value.cwd;
    const name=value.label||value.title||(kind==="workspace"?"No listed conversation":"Untitled conversation"),live=value.activity?.live??"unknown";
    const row={...item,kind,workspace,name,live,updated:sessionGridTime(value.updated),asks:value.pending_asks_known?value.pending_asks:null};
    row.search=[workspace,name,value.title,value.id,value.provider,value.model,value.branch,liveLabels[live]].join(" ").toLocaleLowerCase();
    row.identity=[kind,value.provider,value.id,workspace].join("\0");tr.setAttribute("data-nav-live",live);
    const workspaceCell=node("td","nav-grid-col-workspace"),workspaceName=workspace?workspace.split("/").filter(Boolean).at(-1)||"/":"Unknown directory";
    let navigable=Boolean(workspace);if(navigable)try{directory(workspace);}catch{navigable=false;}
    if(navigable){
      const known=value.workspace_known||Boolean(value.activity?.workspace)||kind==="workspace";
      const link=node("a","nav-grid-workspace",workspaceName);link.href=(known?"/long?dir=":"/sessions?dir=")+encodeForm(workspace);link.rel="noreferrer";
      link.title=known?`Workspace history: ${workspace}`:`Conversations in directory: ${workspace}`;
      workspaceCell.append(link,node("span","nav-grid-path",workspace));
      if(onWorkspace)listen(link,"click",event=>{if(event.button>0||event.metaKey||event.ctrlKey||event.shiftKey||event.altKey||!active())return;event.preventDefault();onWorkspace(workspace,known);});
    }else workspaceCell.append(node("span","nav-grid-unknown",workspaceName));
    const sessionCell=node("td","nav-grid-col-session");
    if(value.selectable){
      row.button=node("button","nav-controller-select",name);row.button.type="button";row.button.title=`${value.provider} · ${value.id}${value.title?" · "+value.title:""}`;sessionCell.append(row.button);
      listen(row.button,"click",()=>{if(active())onSelect(value,row);});
    }else sessionCell.append(node("span","nav-controller-unavailable",name));
    sessionCell.append(node("span","nav-grid-session-id",kind==="workspace"?"Recorded workspace":`${value.id.slice(0,8)}${value.worker?" · worker":""}`));
    const provider=node("td","nav-grid-col-provider",value.provider||"Unknown"),asks=node("td","nav-grid-col-asks",row.asks===null?"—":String(row.asks));
    asks.title=row.asks===null?"Pending ask count unavailable":kind==="workspace"?"Open asks recorded for this workspace without a listed conversation":"Open asks attached to this conversation";
    const updated=node("td","nav-grid-col-updated"),time=node("time","",sessionGridLocalTime(value.updated));if(row.updated!==null)time.setAttribute("datetime",new Date(row.updated).toISOString());updated.append(time);
    const status=node("td","nav-grid-col-status",kind==="workspace"?"No listed conversation":liveLabels[live]??liveLabels.unknown);
    status.title=live==="live"?"Managed session verified running":live==="open"?"Registered process exists; this session is not verified live":live==="recent"?"Recent transcript write; process not verified":live==="ended"?"Managed session verified ended":"Live state unknown";
    const actions=node("td","nav-grid-col-actions");renderDetails(actions,value);tr.append(workspaceCell,sessionCell,provider,asks,updated,status,actions);rows.push(row);body.append(tr);
  }
  function field(row,key){return key==="workspace"?row.workspace||null:key==="session"?row.name:key==="provider"?row.value.provider||null:key==="asks"?row.asks:key==="updated"?row.updated:row.kind==="workspace"?"No listed conversation":liveLabels[row.live]??"Unknown";}
  function matches(row){
    if(quick&&!row.search.includes(quick)||liveOnly&&!["live","open"].includes(row.live))return false;
    const f=current.filters;
    if(f.workspace&&!row.workspace.toLocaleLowerCase().includes(f.workspace.toLocaleLowerCase())||f.session&&![row.name,row.value.title,row.value.id].join(" ").toLocaleLowerCase().includes(f.session.toLocaleLowerCase()))return false;
    if(f.provider&&row.value.provider!==f.provider||f.status&&(f.status==="workspace"?row.kind!=="workspace":row.kind==="workspace"||row.live!==f.status))return false;
    if(f.asks&&(f.asks==="unknown"?row.asks!==null:row.asks===null||f.asks==="pending"&&row.asks===0||f.asks==="clear"&&row.asks!==0))return false;
    if(f.updated&&(f.updated==="unknown"?row.updated!==null:row.updated===null||row.updated<Date.now()-(f.updated==="day"?86400000:604800000)))return false;
    return true;
  }
  function apply(){
    const focus=document.activeElement,inside=rows.some(row=>row.root.contains(focus));
    const sorted=[...rows].sort((a,b)=>{
      const x=field(a,current.sort),y=field(b,current.sort);
      if(x===null||y===null)return x===y?a.identity.localeCompare(b.identity):x===null?1:-1;
      const compare=typeof x==="number"?x-y:x.localeCompare(y,undefined,{numeric:true,sensitivity:"base"});
      return (current.descending?-compare:compare)||a.identity.localeCompare(b.identity);
    });
    let visible=0;for(const row of sorted){row.root.hidden=!matches(row);if(!row.root.hidden)visible++;body.append(row.root);}
    if(inside&&focus?.isConnected&&!focus.parentNode?.hidden&&document.activeElement!==focus)focus.focus({preventScroll:true});
    for(const [key,{th,button,mark}]of headers){const selected=current.sort===key;th.setAttribute("aria-sort",selected?current.descending?"descending":"ascending":"none");mark.textContent=selected?current.descending?" ↓":" ↑":" ↕";button.title=`Sort by ${columns.find(([k])=>k===key)[1].toLocaleLowerCase()}`;}
    count.textContent=`${visible} of ${rows.length} loaded rows${data.partial?" · partial snapshot":""}`;
    clear.hidden=!Object.values(current.filters).some(Boolean);empty.hidden=visible!==0||rows.length===0;
    return visible;
  }
  listen(clear,"click",()=>{if(!active())return;current.filters=emptyFilters();for(const input of filters.values())input.value="";apply();filters.get("workspace").focus({preventScroll:true});});
  apply();
  return {root,toolbar,rows,setFilter(text,live=false){if(disposed||typeof text!=="string"||typeof live!=="boolean")return 0;quick=text.toLocaleLowerCase();liveOnly=live;return apply();},
    viewState(){return {sort:current.sort,descending:current.descending,filters:{...current.filters}};},
    teardown(){if(disposed)return;disposed=true;for(const remove of removes)remove();removes.length=0;}};
}
