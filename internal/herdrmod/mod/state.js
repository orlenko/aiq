export const machineKey = machine => machine ?? "";
export const sessionKey = row => [row.host,row.provider,row.conversation_id||"workspace:"+row.workspace].join("\u001f");
export const rowKey = row => row.host+":"+row.id;
export function mergeSessionRows(before,after) {
  const key=r=>[r.kind,r.provider,r.id||r.lease_id||r.cwd].join("\u001f"),rows=new Map(before.map(r=>[key(r),r]));
  for(const row of after){const old=rows.get(key(row));if(!old||row.updated_ms>=old.updated_ms)rows.set(key(row),row);}
  return [...rows.values()];
}
export function localTime(ms) { if(!ms) return "Unknown"; const d=new Date(ms),pad=n=>String(n).padStart(2,"0"); return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`; }
export function terminalText(reason){return ({no_lease:"No running terminal is linked to this conversation.",ended:"The session terminal has ended.",not_herdr:"This session uses another terminal backend.",ambiguous:"More than one terminal matches; choose it in Herdr.",label_missing:"This session predates terminal linking; open its terminal in Herdr.",interactive_unmapped:"This session has no recorded terminal link."})[reason]??"The terminal could not be verified.";}
export function groups(rows,query="",section="pending") {
  const result=new Map(), q=query.toLowerCase();
  for(const row of rows) {
    if(section==="pending" && row.state!=="open") continue;
    if(section==="done" && row.state==="open")continue;
    if(q && ![row.workspace,row.session?.name,row.provider,row.machine??"Here",row.latest.preview].join(" ").toLowerCase().includes(q))continue;
    const key=sessionKey(row);
    if(!result.has(key))result.set(key,{key,rows:[],machine:row.machine,workspace:row.workspace,provider:row.provider,name:row.session?.name||"",updated:0});
    const group=result.get(key);group.rows.push(row);group.updated=Math.max(group.updated,row.raised_at_ms);
  }
  return [...result.values()].map(g=>({...g,rows:g.rows.sort((a,b)=>a.raised_at_ms-b.raised_at_ms||Number(a.id)-Number(b.id))})).sort((a,b)=>b.updated-a.updated||a.key.localeCompare(b.key));
}
export class Selection {
  constructor(){this.group=null;this.ask=null;this.peek=null;this.generation=0;}
  choose(row){const key=row?rowKey(row):null;if(key===this.ask)return;this.group=row?sessionKey(row):null;this.ask=key;this.peek=null;this.generation++;}
  reconcile(list){
    const all=list.flatMap(g=>g.rows),same=all.find(r=>rowKey(r)===this.ask);
    if(same)return same;
    const group=list.find(g=>g.key===this.group);
    const next=group?.rows[0]??all[0]??null;this.choose(next);return next;
  }
}
export function errorText(code) { return ({busy:"aiq is busy. Refresh to try again.",unavailable:"Cannot reach aiq on this machine.",not_installed:"aiq is not installed on this machine.",update_required:"Update aiq on this machine to use its native bridge.",not_set_up:"aiq data is not set up on this machine.",source_changed:"The aiq data source changed. Refresh before acting.",conflict:"This ask changed. Review its latest version.",unknown_outcome:"Dismissal could not be confirmed. Refresh before trying again.",too_large:"This result is too large. Open a smaller window.",preparing:"Session history is still preparing. Refresh to continue.",not_found:"This item is no longer available.",internal:"aiq returned an unreadable result. Refresh to try again."})[code]??"The request did not complete. Refresh to try again."; }
