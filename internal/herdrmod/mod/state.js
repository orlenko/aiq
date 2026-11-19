export const machineKey = machine => machine ?? "";
export const sessionKey = row => [row.host,row.provider,row.conversation_id||"workspace:"+row.workspace].join("\u001f");
export const rowKey = row => row.host+":"+row.id;
export function mergeSessionRows(before,after) {
  const key=r=>[r.kind,r.provider,r.id||r.lease_id||r.cwd].join("\u001f"),rows=new Map(before.map(r=>[key(r),r]));
  for(const row of after){const old=rows.get(key(row));if(!old||row.updated_ms>=old.updated_ms)rows.set(key(row),row);}
  return [...rows.values()];
}
export function localTime(ms) { if(!ms) return "Unknown"; const d=new Date(ms),pad=n=>String(n).padStart(2,"0"); return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`; }
const minute = 60;
const hour = 60 * minute;
const day = 24 * hour;

export function quotaCountdown(window, now) {
  const reset = window.resets_at_ms;
  if (!Number.isSafeInteger(reset) || reset <= 0) return null;

  const remaining = Math.max(0, (reset - now) / 1000);
  const seconds = Math.max(1, Math.floor(remaining));
  const pad = value => String(value).padStart(2, "0");
  let text = "Reset due";
  if (seconds >= day) {
    text = `${Math.floor(seconds / day)}d ${pad(Math.floor(seconds / hour) % 24)}h`;
  } else if (seconds >= hour) {
    text = `${Math.floor(seconds / hour)}h ${pad(Math.floor(seconds / minute) % 60)}m`;
  } else if (remaining > 0) {
    text = `${Math.floor(seconds / minute)}m ${pad(seconds % 60)}s`;
  }

  const reported = window.window_seconds;
  const duration = Number.isSafeInteger(reported) && reported > 0
    ? reported : window.kind === "weekly" ? 7 * day : null;
  return {text, fraction: duration === null ? null : Math.min(1, remaining / duration)};
}

export function quotaResetGrant(account) {
  if (account.provider !== "claude" && account.provider !== "codex") return "";
  const noun = account.provider === "claude" ? "reset grant" : "reset credit";
  const count = account.reset_credits;
  if (!Number.isSafeInteger(count) || count < 0) return "";
  if (count === 0) return `No ${noun}s`;
  const expiry = account.reset_credit_expires_at_ms;
  return `${count} ${noun}${count === 1 ? "" : "s"}${expiry ? " · next expires "+localTime(expiry).slice(5) : ""}`;
}

export function quotaRenewals(account) {
  if (account.enabled === false) return [];
  // Build command hints from the account identifier. Provider error text is
  // only used to recognize the fixed Claude poll failure messages.
  const {id, provider, login} = account;
  const prefix = provider + "/";
  const name = id.startsWith(prefix) ? id.slice(prefix.length) : "";
  let argument = "";
  if (name && !name.startsWith(".") && !name.startsWith("-") && !/[/\u0000-\u001f\u007f-\u009f\s]/.test(name)) {
    argument = /^[A-Za-z0-9_./@+:=,%-]+$/.test(id) ? id : "'" + id.replaceAll("'", "'\\''") + "'";
  }
  const hints = [];
  const add = (kind, text) => hints.push({kind, text, command: argument ? `aiq account ${kind} ${argument}` : ""});
  const missingLogin = login?.has_credential === false && login.cli !== "unreadable";
  const codexAuthRequired = provider === "codex" && account.ineligible === "authentication required";
  if (login?.cli === "out" || login?.cli_mismatch === true || missingLogin || codexAuthRequired) {
    add("login", "CLI login required");
  }
  if (provider === "claude" && /^(no poll grant|grant expired|usage: HTTP 401)\b/.test(account.poll_error ?? "")) {
    add("authorize", "Quota authorization required");
  }
  return hints;
}

export function omissionNotice(reply) {
  const omitted = reply?.omitted;
  if (!omitted) return "";
  const notices = [];
  const count = (key, singular, plural, reason) => {
    const n = omitted[key];
    if (Number.isSafeInteger(n) && n > 0) notices.push(`${n} ${n === 1 ? singular : plural} ${reason}`);
  };
  count("rows", "ask omitted", "asks omitted", "by the list limit");
  count("too_large", "transcript skipped", "transcripts skipped", "by the file size limit");
  count("slow", "transcript skipped", "transcripts skipped", "by the read time limit");
  count("unreadable", "transcript", "transcripts", "could not be read");
  count("records", "transcript record skipped", "transcript records skipped", "by the record size limit");
  if (omitted.metadata_limit === 1) notices.push("Session details omitted by the metadata limit");
  if (omitted.metadata_timeout === 1) notices.push("Session details omitted by the read time limit");
  if (omitted.metadata_incomplete === 1) notices.push("Some session details are unavailable");
  return notices.join(" · ");
}
export function terminalText(reason){return ({no_lease:"No running terminal is linked to this conversation.",ended:"The session terminal has ended.",not_herdr:"This session uses another terminal backend.",ambiguous:"More than one terminal matches; choose it in Herdr.",label_missing:"The terminal link could not be verified. Open the workspace in Herdr.",interactive_unmapped:"This session has no recorded terminal link."})[reason]??"The terminal could not be verified.";}
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
export function errorText(code) { return ({busy:"aiq is busy. Refresh to try again.",unavailable:"Cannot reach aiq on this machine.",not_installed:"aiq is not installed on this machine.",update_required:"Update aiq on this machine to use its native bridge.",not_set_up:"aiq data is not set up on this machine.",source_changed:"The aiq data source changed. Refresh before acting.",conflict:"This ask changed. Review its latest version.",unknown_outcome:"Dismissal could not be confirmed. Refresh before trying again.",too_large:"A size limit prevented aiq from loading this data.",preparing:"Session history is still preparing. Refresh to continue.",not_found:"This item is no longer available.",internal:"aiq returned an unreadable result. Refresh to try again."})[code]??"The request did not complete. Refresh to try again."; }
