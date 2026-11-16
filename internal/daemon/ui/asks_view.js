import {captureAsks} from "./asks_data.js";
import {renderLines} from "./dom.js";
import {directory,encodeForm} from "./auth_codec.js";

const labels = ["Asks", "Warnings", "Other open", "Probably done", "Closed and dismissed"];
export function navigatorAskCommand(id,state){
  if(typeof id!=="string"||id.length<1||id.length>19||/[^0-9]/.test(id)||id[0]==="0"||id.length===19&&id>"9223372036854775807"||!["open","probably-done","closed","dismissed"].includes(state))return null;
  return `aiq asks ${state==="open"?"dismiss":"restore"} ${id}`;
}
export function navigatorRemedyCommands(values){
  if(!Array.isArray(values))return [];
  return [["service_install","aiq asks service install","Copy background watcher command"],["setup","aiq asks setup","Copy setup command"],["watch","aiq asks watch","Copy watch command (in its own window)"]].filter(([key])=>values.includes(key));
}
function clock(value) {
  if (!value.known) return "unknown";
  const date = new Date(value.ms);
  return Number.isNaN(date.getTime()) ? `milliseconds since epoch: ${value.ms}` : date.toISOString();
}
function localClock(value) {
  if (!value.known) return "time unknown";
  const date = new Date(value.ms);
  if (!Number.isFinite(date.getTime())) return "time unknown";
  return date.toLocaleString(undefined, {year:"numeric", month:"short", day:"numeric",
    hour:"numeric", minute:"2-digit", timeZoneName:"short"});
}
// Display the admitted line text, while clipboard operations retain the exact
// separately supplied quotation. Terminal decoration is not question content.
function questionText(wording){return wording.lines.map(line=>line.spans.filter(span=>!span.d).map(span=>span.t).join("")).join("\n").trim()||"Question text unavailable";}

// A supplied snapshot only. Its parent owns readers, authentication and replacement.
export function createAsksView(document, data, options) {
  const {window, idPrefix} = options || {};
  const inbox=options?.inbox??false;
  const restored=options?.viewState??null;
  let {copyText, announce, onConversation, onPlacement, onSelection, onAction, onBatchAction} = options || {};
  if (!window || window !== document.defaultView || typeof idPrefix !== "string" ||
      !/^[A-Za-z][A-Za-z0-9_-]*$/.test(idPrefix) ||
      typeof inbox!=="boolean" || ![copyText, announce, onConversation].every(f => typeof f === "function") || onPlacement!==undefined && typeof onPlacement!=="function" || onSelection!==undefined && typeof onSelection!=="function" || onAction!==undefined && typeof onAction!=="function" || onBatchAction!==undefined && typeof onBatchAction!=="function") {
    throw new TypeError("could not configure Asks view");
  }
  let snapshot = captureAsks(data, options.expectedScope, options.limits);
  data = null; options = null;
  const fragment = document.createDocumentFragment();
  let root = node("section", "nav-asks"); fragment.append(root);
  const title = node("h1", "nav-asks-heading", "Asks"); title.id = `${idPrefix}-heading`;
  root.setAttribute("aria-labelledby", title.id); root.append(title);
  const status = node("p", "nav-asks-status", ""); status.setAttribute("role", "status");
  status.setAttribute("aria-live", "polite"); root.append(status);
  const rows = [], groups = [], removes = [];
  let phase = "detached", flight = null, focusIntent = null, overview = null;
  const overviewButtons = [],commandButtons=[];
  let selectedInbox=null,inboxPanel=null,selectedSession=null,sessionHeading=null,sessionPath=null,sessionFacts=null,sessionQuestions=null,filterText="",actionFlight=null;
  const sessions=[],sessionByKey=new Map();
  let bulkButton=null,bulkConfirm=null,bulkIntent=null,bulkFlight=null;
  const bulkRemoves=[];
  function node(tag, className, text) {
    const n = document.createElement(tag); n.className = className;
    if (text !== undefined) n.textContent = text;
    return n;
  }
  function button(text) {
    const n = node("button", "nav-asks-button", text); n.type = "button"; return n;
  }
  function listen(n, callback) {
    const handle = () => { endFocus(focusIntent); callback(); };
    n.addEventListener("click", handle); removes.push(() => n.removeEventListener("click", handle));
  }
  function active() { return phase === "ready" || phase === "activating" || phase === "transitioning"; }
  function eligible(row, intent = row.ready) {
    return phase === "ready" && root?.isConnected && row.group?.expanded && (!inbox||selectedInbox===row) && intent && row.ready === intent;
  }
  function revoke(row) {
    if (row.ready) row.ready.copy = "";
    row.ready = null;
  }
  function controls() {
    for(const control of commandButtons)control.setAttribute("aria-disabled",phase!=="ready"||!root?.isConnected||flight?"true":"false");
    for (const button of overviewButtons) button.setAttribute("aria-disabled", phase !== "ready" || !root?.isConnected ? "true" : "false");
    for (const row of rows) {
      row.copy.setAttribute("aria-disabled", !eligible(row) || flight ? "true" : "false");
      if(row.command)row.command.setAttribute("aria-disabled",!eligible(row)||flight?"true":"false");
      row.nav.setAttribute("aria-disabled", !eligible(row) || !row.data.selectable ? "true" : "false");
      if(row.action)row.action.disabled=!eligible(row)||Boolean(actionFlight||bulkFlight)||!row.data.revision;
    }
    if(bulkButton){const pending=selectedSession?.rows.filter(row=>row.data.state==="open")??[];
      bulkButton.textContent=`Dismiss all ${pending.length} pending asks`;
      bulkButton.disabled=phase!=="ready"||!root?.isConnected||Boolean(actionFlight||bulkFlight)||!pending.length||!selectedSession?.id||pending.some(row=>!row.data.revision)||new Set(pending.map(row=>row.data.id)).size!==pending.length;
    }
  }
  function cleanFocus(intent) {
    if (!intent || !intent.ended || intent.busy || intent.cleaning) return;
    intent.cleaning = true;
    const handles = intent.removes.splice(0);
    for (const remove of handles) { try { remove(); } catch { /* Matching cleanup stays local. */ } }
    intent.cleaning = false;
    if (focusIntent === intent) focusIntent = null;
  }
  function endFocus(intent, revoke = true) {
    if (!intent) return;
    if (revoke) intent.revoked = true;
    intent.ended = true; cleanFocus(intent);
  }
  function focusCurrent(intent, row) {
    return phase === "ready" && root?.isConnected && focusIntent === intent && !intent.revoked &&
      !intent.ended && row.group && row.heading?.isConnected;
  }
  function focusTarget(row, child, ready, heading) {
    return phase === "ready" && root?.isConnected && row.group?.expanded && child &&
      row.current === child && child.mounted && !child.retired && ready && row.ready === ready &&
      row.heading === heading && root.contains(heading) && heading.isConnected && heading.getClientRects().length > 0;
  }
  function reveal(row, trigger) {
    if (phase !== "ready" || !root.isConnected || !trigger.isConnected || !root.contains(trigger) || !trigger.getClientRects().length) return;
    if(inbox){if(!row.group.expanded)toggleGroup(row.group);selectInbox(row);row.more.open=true;}
    const intent = {initial:document.activeElement, revoked:false, ended:false, busy:1, cleaning:false, removes:[]};
    focusIntent = intent;
    let target = null;
    try {
      const leave = () => endFocus(intent);
      try {
        for (const name of ["focusin", "focusout", "blur"]) {
          // Register ownership first: a setup call may fail/reenter after acquisition.
          intent.removes.push(() => document.removeEventListener(name, leave, true));
          document.addEventListener(name, leave, true);
          if (!focusCurrent(intent, row)) return;
        }
      } finally { intent.busy--; cleanFocus(intent); }
      if (!focusCurrent(intent, row)) return;
      if (!row.group.expanded) toggleGroup(row.group); // VOID; inspect actual publication below.
      if (!focusCurrent(intent, row)) return;
      const child = row.current, ready = row.ready, heading = row.heading;
      if (focusTarget(row, child, ready, heading)) target = {child, ready, heading};
    } catch { if (focusCurrent(intent, row)) terminate(false); }
    finally { endFocus(intent, false); }
    let commit = false;
    try {
      commit = target && !intent.revoked && intent.initial?.isConnected && document.activeElement === intent.initial &&
        focusTarget(row, target.child, target.ready, target.heading);
    } catch { /* An unavailable target cannot acquire focus. */ }
    // Tracking is gone before this terminal callout; no later old-operation DOM writes.
    if (commit) { try { target.heading.focus(); } catch { /* Parent retains focus recovery. */ } }
  }
  function cleanup(candidate) {
    if (!candidate || !candidate.retired || candidate.busy || candidate.cleaned) return;
    candidate.cleaned = true;
    try { candidate.child?.teardown(); } catch { /* Retired cleanup stays local. */ }
    candidate.child = null; candidate.root = null;
  }
  function retire(candidate) {
    if (!candidate) return;
    candidate.retired = true; candidate.root?.remove(); cleanup(candidate);
  }
  function current(row, candidate) {
    return active() && !candidate.retired && row.current === candidate;
  }
  function protectedRefresh(row, candidate) {
    if (!current(row, candidate) || !candidate.mounted) return false;
    candidate.busy++;
    try {
      candidate.child.refresh(); // VOID scheduling, not completed geometry.
      return current(row, candidate);
    } finally { candidate.busy--; cleanup(candidate); }
  }
  function publish(row, candidate, expanded) {
    if (!current(row, candidate)) return false;
    row.expanded = expanded;
    row.wording.textContent = expanded ? "Latest wording" : "Original wording";
    row.details.hidden = !expanded;
    row.toggle.textContent = expanded ? "Show original" : "Show details";
    row.toggle.setAttribute("aria-expanded", expanded ? "true" : "false");
    row.ready = {generation:++row.generation, copy:row.data[expanded ? "latest" : "first"].copy};
    if(row.question)row.question.textContent=questionText(row.data[expanded?"latest":"first"]);
    return true;
  }
  function activate(row, expanded) {
    revoke(row);
    const old = row.current;
    const candidate = {root:node("div", "nav-asks-quote"), child:null, busy:0,
      mounted:false, retired:false, cleaned:false};
    row.current = candidate; candidate.root.hidden = true;
    retire(old);
    if (!current(row, candidate)) { retire(candidate); return false; }
    row.quote.append(candidate.root);
    candidate.busy++;
    try {
      candidate.child = renderLines(document, row.data[expanded ? "latest" : "first"].lines,
        {window, markup:false, base:[]});
      if (!current(row, candidate)) return false;
      candidate.root.append(candidate.child.fragment);
      const mounted = candidate.child.mount() === true;
      if (!current(row, candidate)) return false;
      if (!mounted) throw new Error("line activation failed");
      candidate.mounted = true;
      if (!protectedRefresh(row, candidate)) return false;
      if (!publish(row, candidate, expanded)) return false;
      candidate.root.hidden = false;
      return true;
    } finally {
      candidate.busy--;
      if (!current(row, candidate)) candidate.retired = true;
      cleanup(candidate);
    }
  }
  function finish() {
    if (!active()) return;
    phase = "ready"; status.textContent = ""; root.setAttribute("aria-busy", "false"); controls();
  }
  function transaction(operation) {
    if (phase !== "ready" || !root.isConnected) return;
    phase = "transitioning"; status.textContent = "Updating wording";
    root.setAttribute("aria-busy", "true"); controls();
    try { operation(); finish(); } catch { terminate(false); }
  }
  function toggleGroup(group) {
    transaction(() => {
      for (const row of group.rows) revoke(row);
      group.expanded = !group.expanded;
      group.region.hidden = !group.expanded;
      group.button.setAttribute("aria-expanded", group.expanded ? "true" : "false");
      if (group.expanded) for (const row of group.rows) {
        if (!active()) return;
        if (!row.current) { if (!activate(row, row.expanded)) return; }
        else if (protectedRefresh(row, row.current)) publish(row, row.current, row.expanded);
        else return;
      }
    });
    if(inbox&&selectedInbox?.group===group&&!group.expanded)selectInbox(rows.find(row=>row.group.expanded&&row.session===selectedSession)??null);
    if(inbox)syncSessions();
  }
  function selectInbox(row){
    if(!inbox||phase!=="ready"||!root?.isConnected||row&&!row.group.expanded)return;
    if(row&&selectedInbox===row){syncSessions();controls();return;}
    if(row&&selectedSession!==row.session)cancelBulk();
    if(selectedInbox){selectedInbox.panel.hidden=true;selectedInbox.article.append(selectedInbox.panel);selectedInbox.pick.removeAttribute("aria-current");selectedInbox.article.setAttribute("data-selected","false");}
    selectedInbox=row;inboxPanel.replaceChildren();
    if(row){selectedSession=row.session;syncSessions();if(sessionQuestions)selectedSession.item.append(sessionQuestions);row.panel.hidden=false;inboxPanel.append(row.panel);row.pick.setAttribute("aria-current","true");row.article.setAttribute("data-selected","true");}
    else inboxPanel.append(node("p","nav-empty","Choose an Ask to read its question."));
    controls();
    try{onSelection?.(row?Object.freeze({askId:row.data.id,provider:row.data.provider,id:row.data.session_id}):null);}catch{}
    try{announce(row?"Ask selected.":"Choose an Ask.");}catch{/* Selection does not depend on its status sink. */}
  }
  function matches(row){return !filterText||row.search.includes(filterText);}
  function syncSessions(){
    if(!inbox)return;
    for(const session of sessions){
      session.item.hidden=!session.rows.some(matches);
      session.pick.setAttribute("aria-current",session===selectedSession?"true":"false");
    }
    for(const row of rows)row.article.hidden=row.session!==selectedSession||!matches(row);
    for(const group of groups){
      const count=group.rows.filter(row=>row.session===selectedSession&&matches(row)).length;
      group.heading.hidden=count===0;
      group.button.textContent=`${group.label} (${count})`;
      group.region.hidden=!group.expanded||count===0;
    }
    if(sessionHeading){sessionHeading.textContent=selectedSession?`${selectedSession.provider} · ${selectedSession.id?selectedSession.id.slice(0,8):"Unidentified session"}`:"No pending asks";sessionPath.textContent=selectedSession?.workspace||"Choose a session to review its questions.";sessionFacts.textContent=selectedSession?`${selectedSession.pending} pending · this machine`:"This machine";}
  }
  function selectSession(session){
    if(phase!=="ready"||!root?.isConnected)return;
    const row=session.rows.find(row=>row.data.state==="open"&&matches(row))??session.rows.find(matches);
    if(!row)return;
    if(selectedSession!==session)cancelBulk();selectedSession=session;
    if(!row.group.expanded)toggleGroup(row.group);
    selectInbox(row);
  }
  function setFilter(value,explicit=false){
    if(phase!=="ready"||!inbox||typeof value!=="string")return false;
    filterText=value.toLocaleLowerCase();
    if(!selectedInbox||!matches(selectedInbox)){
      const row=rows.find(row=>row.session===selectedSession&&row.data.state==="open"&&matches(row))??
        (explicit||!selectedSession||selectedSession.pending===0?rows.find(row=>row.data.state==="open"&&matches(row))??rows.find(matches):null);
      if(row){selectedSession=row.session;if(!row.group.expanded)toggleGroup(row.group);selectInbox(row);}else if(selectedInbox)selectInbox(null);
    }
    syncSessions();inboxPanel.hidden=!selectedInbox||!matches(selectedInbox);return rows.some(matches);
  }
  async function act(row){
    if(!eligible(row)||actionFlight||bulkFlight||!row.data.revision||!onAction)return;
    const token={};actionFlight=token;controls();
    try{await onAction(Object.freeze({id:row.data.id,revision:row.data.revision,action:row.data.state==="open"?"dismiss":"restore"}));}
    catch{if(eligible(row))status.textContent="Could not update this ask. Refresh Asks before trying again.";}
    finally{if(actionFlight===token)actionFlight=null;if(phase==="ready")controls();}
  }
  function cancelBulk(){
    for(const remove of bulkRemoves.splice(0))remove();
    bulkIntent=null;if(bulkConfirm){bulkConfirm.hidden=true;bulkConfirm.replaceChildren();}
    try{bulkFlight?.abort.abort();}catch{}
  }
  function prepareBulk(){
    if(!inbox||phase!=="ready"||!root?.isConnected||bulkButton.disabled||!onBatchAction)return;
    cancelBulk();
    const bind=(target,type,fn)=>{target.addEventListener(type,fn);bulkRemoves.push(()=>target.removeEventListener(type,fn));};
    const session=selectedSession,items=Object.freeze(session.rows.filter(row=>row.data.state==="open").map(row=>Object.freeze({id:row.data.id,revision:row.data.revision,action:"dismiss"})));
    const intent={session,items};bulkIntent=intent;bulkConfirm.replaceChildren();bulkConfirm.hidden=false;
    const scope=node("p","",`Dismiss all ${items.length} pending asks in ${session.workspace}?`),identity=node("p","",`${session.provider} · ${session.id}`),confirm=button(`Dismiss ${items.length} asks`),cancel=button("Cancel");
    bulkConfirm.append(scope,identity,confirm,cancel);
    bind(cancel,"click",()=>{cancelBulk();bulkButton.focus();});
    bind(confirm,"click",async()=>{
      if(bulkIntent!==intent||phase!=="ready"||selectedSession!==session||!root?.isConnected||bulkFlight)return;
      const token={abort:new window.AbortController()};bulkFlight=token;bulkIntent=null;bulkConfirm.hidden=true;controls();
      const valid=()=>phase==="ready"&&root?.isConnected&&bulkFlight===token&&!token.abort.signal.aborted&&selectedSession===session;
      try{await onBatchAction(items,Object.freeze({sessionKey:session.key,workspace:session.workspace,provider:session.provider,id:session.id}),token.abort.signal,valid);}
      catch{if(valid())status.textContent="Session dismissal stopped. Refresh Asks to check its outcome.";}
      finally{if(bulkFlight===token)bulkFlight=null;if(phase==="ready")controls();}
    });
    const escape=event=>{if(event.key==="Escape"){event.preventDefault();cancelBulk();bulkButton.focus();}};
    bind(bulkConfirm,"keydown",escape);cancel.focus();
  }
  function copy(row,command=null) {
    const ready=()=>row?eligible(row):phase==="ready"&&root?.isConnected;
    if (!ready() || flight) return;
    const intent = row?.ready, text = command??intent.copy, token = {row, intent, settled:false};
    flight = token; controls();
    function settle(ok) {
      if (token.settled || flight !== token) return;
      token.settled = true;
      try {
        if (row?eligible(row,intent):ready()) {
          const message = command===null?(ok?"Quote copied":"Could not copy quote"):(ok?"Terminal command copied":"Could not copy terminal command");
          status.textContent = message;
          try { announce(message); } catch { /* Status cannot reclassify the copy. */ }
        }
      } finally {
        if (flight === token) flight = null;
        if (phase === "ready") controls();
      }
    }
    try { Promise.resolve(copyText(text)).then(() => settle(true), () => settle(false)); }
    catch { settle(false); }
  }
  function navigate(row) {
    if (!eligible(row) || !row.data.selectable) return;
    const intent = row.ready;
    try {
      if (onPlacement) onPlacement(Object.freeze({askId:row.data.id,provider:row.data.provider,id:row.data.session_id,excerpt:questionText(row.data[row.expanded?"latest":"first"])}));
      else onConversation({provider:row.data.provider, id:row.data.session_id});
    }
    catch {
      if (eligible(row, intent)) {
        status.textContent = "Conversation unavailable";
        try { announce("Conversation unavailable"); } catch { /* Fixed, current-only status. */ }
      }
    }
    // Trusted VOID adapter owns any asynchronous navigation rejection.
  }
  function terminate(dispose) {
    if (phase === "disposed" || phase === "failed" && !dispose) return;
    phase = dispose ? "disposed" : "failed"; flight = null;
    cancelBulk();
    endFocus(focusIntent);
    const candidates = [];
    for (const row of rows) {
      revoke(row);
      if (row.current) { row.current.retired = true; candidates.push(row.current); }
    }
    // Silence and detach ALL roots before the first child cleanup callout.
    for (const candidate of candidates) candidate.root?.remove();
    if (dispose) { root?.remove(); root?.replaceChildren(); }
    else {
      root.replaceChildren(title, node("p", "nav-asks-status", "Asks unavailable"));
      root.setAttribute("aria-busy", "false");
    }
    for (const remove of removes) remove(); removes.length = 0;
    overview?.replaceChildren(); overview = null; overviewButtons.length = 0;
    commandButtons.length=0;
    for (const row of rows) for (const key of Object.keys(row)) row[key] = null;
    rows.length = 0; groups.length = 0; snapshot = null;
    selectedInbox=null;inboxPanel=null;selectedSession=null;sessionQuestions=null;sessionByKey.clear();sessions.length=0;
    copyText = null; announce = null; onConversation = null; onPlacement = null;onSelection=null;onAction=null;onBatchAction=null;
    for (const candidate of candidates) cleanup(candidate);
    if (dispose) root = null;
  }

  root.append(node("p", "nav-asks-scope", snapshot.scope.mode === "all" ? "All directories" : snapshot.scope.dir));
  if(snapshot.scope_counts)root.append(node("p","nav-asks-scope-counts",snapshot.scope_counts.open+" open here · "+snapshot.scope_counts.elsewhere_open+" open elsewhere"));
  if(snapshot.scope.mode!=="all"){const all=node("a","nav-asks-all","Show all Asks");all.href="/asks?all=true";root.append(all);}
  if(snapshot.status_line){
    const value=snapshot.status_line,line=node("p","nav-asks-trust",(value.problem?"! ":"")+value.text);
    if(value.problem)line.setAttribute("data-problem","true");root.append(line);
    for(const [,text,label]of navigatorRemedyCommands(value.remedy)){const control=button(label);control.title=text;root.append(control);commandButtons.push(control);listen(control,()=>copy(null,text));}
  }
  if (snapshot.totals) {
    const t = snapshot.totals;
    root.append(node("p", "nav-asks-totals", `Skipped: ${t.skipped}; calls today: ${t.calls_today}; UTC day: ${t.utc_day}`));
  }
  let report = snapshot.report;
  let reportNode = node("aside", "nav-asks-report"); root.append(reportNode);
  if (!report.known) reportNode.append(node("p", "", "Watcher report unavailable"));
  else {
    reportNode.append(node("p", "", "Last stored watcher report"));
    reportNode.append(node("p", "", `Pass: ${clock(report.pass_at)}; OK: ${clock(report.ok_at)}; error: ${clock(report.error_at)}; next try: ${clock(report.next_try_at)}`));
    reportNode.append(node("p", "", `Watched: ${report.watched}; waiting: ${report.waiting}; skipped: ${report.skipped}; calls today: ${report.calls_today}`));
    reportNode.append(node("p", "", report.last_error));
    for (const reason of report.not_watched) reportNode.append(node("p", "", reason));
  }
  report = null; reportNode = null;
  if (snapshot.moved_today) { overview = node("section", "nav-asks-moved"); root.append(overview); }
  snapshot.sections.forEach((section, index) => {
    const group = {expanded:index < 2, label:inbox&&index===0?"Needs your answer":labels[index],rows:[], region:node("div", "nav-asks-region"),
      button:button(`${inbox&&index===0?"Needs your answer":labels[index]} (${section.items.length})`)};
    group.region.id = `${idPrefix}-group-${index}`; group.region.hidden = !group.expanded;
    group.button.setAttribute("aria-controls", group.region.id);
    group.button.setAttribute("aria-expanded", group.expanded ? "true" : "false");
    const heading = node("h2", "nav-asks-group-heading");group.heading=heading; heading.append(group.button);
    heading.setAttribute("data-group",String(index));if(inbox&&index===1&&section.items.length===0)heading.hidden=true;
    group.region.setAttribute("role", "region"); heading.id = `${group.region.id}-heading`;
    group.region.setAttribute("aria-labelledby", heading.id); root.append(heading, group.region);
    groups.push(group); listen(group.button, () => toggleGroup(group));
    section.items.forEach((data, ordinal) => {
      const article = node("article", "nav-asks-item"); article.id = `${group.region.id}-item-${ordinal}`;
      const quote = node("div", "nav-asks-body"), details = node("div", "nav-asks-details");
      quote.id = `${article.id}-quote`; details.id = `${article.id}-details`; details.hidden = true;
      const row = {group, data, article, quote, details, wording:node("p", "nav-asks-wording", "Original wording"),
        expanded:inbox, generation:0n, ready:null, heading:node("h3", "nav-asks-item-heading", `Ask ${data.id}`),
        current:null, toggle:button("Show details"), copy:button("Copy quote"), nav:button("Open conversation"),command:null};
      rows.push(row); group.rows.push(row); group.region.append(article);
      article.append(row.heading,
        node("p", "nav-asks-source", `${data.provider}: ${data.session_id}`));
      article.append(node("p","nav-asks-live","Live state unknown · open the conversation to inspect its activity."));
      if(data.workspace){try{directory(data.workspace);const link=node("a","nav-asks-workspace","Workspace history →");link.href="/long?dir="+encodeForm(data.workspace);link.rel="noreferrer";article.append(link);}catch{}}
      if (data.state !== "open") article.append(node("p", "nav-asks-state", `${data.state}: ${data.state_text} (${data.state_reason})`));
      article.append(row.wording, quote, row.toggle, row.copy, row.nav, details);
      const command=navigatorAskCommand(data.id,data.state);
      if(command){row.command=button(data.state==="open"?"Copy dismiss command":"Copy restore command");row.command.title=command;article.append(row.command);listen(row.command,()=>copy(row,command));}
      row.toggle.setAttribute("aria-controls", `${quote.id} ${details.id}`); row.toggle.setAttribute("aria-expanded", "false");
      row.copy.setAttribute("aria-describedby", quote.id);
      row.nav.disabled = !data.selectable;
      if (!data.selectable) row.nav.textContent = "Conversation unavailable";
      details.append(node("p", "", data.workspace), node("p", "", `Raised: ${localClock(data.raised_at)}; changed: ${localClock(data.state_at)}; repeats: ${data.repeats}`));
      for (const ref of data.refs) details.append(node("p", "nav-asks-ref", ref));
      listen(row.toggle, () => { if (eligible(row)) transaction(() => activate(row, !row.expanded)); });
      listen(row.copy, () => copy(row)); listen(row.nav, () => navigate(row));
    });
  });
  if (overview) {
    const byID = new Map(rows.map(row => [row.data.id, row]));
    const selected = snapshot.moved_today.ids.map(id => byID.get(id)); // Capture guarantees unique complete references.
    const closed = selected.filter(row => row.data.state === "closed").length;
    const heading = node("h2", "nav-asks-moved-heading", "Moved today"); heading.id = `${idPrefix}-moved-heading`;
    overview.setAttribute("aria-labelledby", heading.id);
    overview.append(heading, node("p", "nav-asks-moved-day", `Local day: ${snapshot.moved_today.local_day}`),
      node("p", "nav-asks-moved-counts", `${closed} closed, ${selected.length - closed} probably done`));
    const list = node("ul", "nav-asks-moved-list"); overview.append(list);
    for (const row of selected) {
      row.heading.id = `${row.article.id}-heading`; row.heading.setAttribute("tabindex", "-1");
      row.heading.classList.add("nav-asks-moved-target");
      const control = button(`Ask ${row.data.id}: ${row.data.state}: ${row.data.state_text}`);
      control.classList.add("nav-asks-moved-button"); control.setAttribute("aria-controls", row.heading.id);
      const entry = node("li", "nav-asks-moved-entry"); entry.append(control); list.append(entry);
      overviewButtons.push(control); listen(control, () => reveal(row, control));
    }
  }
  if(inbox){
    root.classList.add("nav-asks-inbox");
    const list=node("div","nav-asks-inbox-list");sessionQuestions=list;inboxPanel=node("aside","nav-asks-inbox-detail");inboxPanel.setAttribute("aria-label","Selected Ask");
    for(const child of [...root.children])list.append(child);
    const rail=node("nav","nav-asks-sessions"),pendingList=node("div","nav-asks-session-list"),history=node("details","nav-asks-session-history"),historyList=node("div","nav-asks-session-list"),work=node("div","nav-asks-work");
    rail.setAttribute("aria-label","Sessions with asks");pendingList.setAttribute("role","list");historyList.setAttribute("role","list");history.append(node("summary","","Done and dismissed"),historyList);rail.append(node("h2","nav-asks-rail-title","Sessions"),pendingList,history,list);
    sessionHeading=node("h2","nav-asks-session-title");sessionPath=node("p","nav-asks-session-path");sessionFacts=node("p","nav-asks-session-facts");
    work.append(sessionPath,sessionHeading,sessionFacts);
    if(onBatchAction){bulkButton=button("Dismiss all pending asks");bulkButton.classList.add("nav-asks-bulk");bulkConfirm=node("section","nav-asks-bulk-confirm");bulkConfirm.hidden=true;bulkConfirm.setAttribute("aria-label","Confirm session dismissal");work.append(bulkButton,bulkConfirm);listen(bulkButton,prepareBulk);}
    work.append(inboxPanel);root.append(rail,work);
    const summary=node("p","nav-asks-summary",`${groups[0].rows.length} need your answer · ${groups[1].rows.length} ${groups[1].rows.length===1?"warning":"warnings"}`),diagnostics=node("details","nav-asks-diagnostics");diagnostics.append(node("summary","","Watcher details and recent changes"));
    for(const child of [...list.children])if(["nav-asks-report","nav-asks-totals","nav-asks-moved","nav-asks-scope","nav-asks-scope-counts"].includes(child.className))diagnostics.append(child);
    list.replaceChildren(summary,...list.children,diagnostics);
    for(const row of rows){
      const project=row.data.workspace.split("/").filter(Boolean).at(-1)||"Unknown project";
      const label=node("p","nav-asks-context",`Ask ${row.data.id} · ${project} · ${row.data.provider}`);
      const key=row.data.selectable?JSON.stringify([row.data.workspace,row.data.provider,row.data.session_id]):"unidentified:"+row.data.id;
      let session=sessionByKey.get(key);
      if(!session){session={key,workspace:row.data.workspace,provider:row.data.provider,id:row.data.selectable?row.data.session_id:"",rows:[],pending:0,item:node("div","nav-asks-session"),pick:button("")};session.pick.classList.add("nav-asks-session-pick");session.item.setAttribute("role","listitem");session.item.append(session.pick);sessions.push(session);sessionByKey.set(key,session);listen(session.pick,()=>selectSession(session));}
      row.session=session;session.rows.push(row);if(row.data.state==="open")session.pending++;
      row.search=[questionText(row.data.latest),row.data.id,row.data.provider,row.data.session_id,row.data.workspace].join(" ").toLocaleLowerCase();
      row.pick=button(questionText(row.data.latest).replace(/\s+/g," ").slice(0,180));row.pick.classList.add("nav-asks-pick");row.pick.setAttribute("aria-controls",`${row.article.id}-panel`);
      row.article.setAttribute("data-filter-text",[questionText(row.data.latest),row.data.id,row.data.provider,row.data.session_id,row.data.workspace].join(" "));
      const age=localClock(row.data.raised_at);
      const meta=node("p","nav-asks-row-meta",`Ask ${row.data.id} · ${age}${row.data.selectable?"":" · conversation unavailable"}`);meta.title=row.data.workspace;
      row.panel=node("div","nav-asks-selected");row.panel.id=`${row.article.id}-panel`;row.panel.hidden=true;
      row.question=node("div","nav-asks-question",questionText(row.data.latest));
      row.nav.textContent=row.data.selectable?"Show conversation beside ask":"Conversation unavailable";
      row.more=node("details","nav-asks-more");row.more.append(node("summary","","Details and original wording"));
      for(const child of [...row.article.children])row.more.append(child);
      const actions=node("div","nav-asks-actions");
      if(onAction){row.command?.remove();row.action=button(row.data.state==="open"?"Dismiss":"Restore");row.action.classList.add("nav-asks-action");actions.append(row.action);listen(row.action,()=>act(row));}
      actions.append(row.nav,row.copy);
      row.panel.append(label,node("p","nav-asks-asked",`Asked ${age}`),row.question,actions,node("p","nav-asks-terminal-note","Answer in the session’s terminal."),row.more);
      row.article.append(row.pick,meta,row.panel);
      listen(row.pick,()=>selectInbox(row));
    }
    for(const session of sessions){
      const name=session.workspace.split("/").filter(Boolean).at(-1)||"Unknown project";
      session.pick.append(node("span","nav-asks-session-name",name),node("span","nav-asks-session-count",String(session.pending)),node("span","nav-asks-session-identity",`${session.provider} · ${session.id?session.id.slice(0,8):"unidentified"}`));session.pick.append(node("span","nav-asks-session-directory",session.workspace));session.pick.title=session.workspace+"\n"+session.provider+"/"+session.id;
      (session.pending?pendingList:historyList).append(session.item);
    }
    const info=node("div","nav-asks-inbox-info");
    for(const child of [...list.children])if(["nav-asks-trust","nav-asks-all"].includes(child.className)||commandButtons.includes(child))info.append(child);
    rail.append(info,pendingList,history);
    if(!sessions.some(session=>session.pending))pendingList.append(node("p","nav-empty","No sessions need your answer."));
    history.hidden=!sessions.some(session=>!session.pending);
    for(const row of rows)row.more.setAttribute("data-ask-details",row.data.id);
    if(restored&&Array.isArray(restored.expanded)&&restored.expanded.length===groups.length&&restored.expanded.every(v=>typeof v==="boolean")){
      groups.forEach((group,index)=>{group.expanded=restored.expanded[index];group.region.hidden=!group.expanded;group.button.setAttribute("aria-expanded",String(group.expanded));});
    }
  }
  snapshot = null; controls();
  function mount() {
    if (phase === "ready") return true;
    if (phase !== "detached" || !root.isConnected || root.ownerDocument !== document) return false;
    phase = "activating"; root.setAttribute("aria-busy", "true");
    try {
      for (const group of groups) if (group.expanded) for (const row of group.rows) {
        if (!activate(row, inbox)) return false;
      }
      if (!active()) return false;
      finish();if(inbox){
        const old=rows.find(row=>row.data.id===restored?.selectedId&&(restored?.history||row.data.state==="open"));
        const next=old??rows.find(row=>row.data.state==="open"&&row.session.key===restored?.sessionKey)??rows.find(row=>row.group.expanded&&row.data.state==="open")??rows.find(row=>row.group.expanded)??null;
        if(next){selectedSession=next.session;if(!next.group.expanded)toggleGroup(next.group);}selectInbox(next);syncSessions();
      }return true;
    } catch { terminate(false); return false; }
  }
  function refresh() {
    transaction(() => {
      for (const row of rows) if (row.group.expanded && row.current && !protectedRefresh(row, row.current)) return;
    });
  }
  function viewState(){return phase==="ready"&&inbox?Object.freeze({selectedId:selectedInbox?.data.id??"",sessionKey:selectedSession?.key??"",history:selectedInbox?.data.state!=="open",expanded:Object.freeze(groups.map(group=>group.expanded))}):null;}
  function focusSelected(){if(phase!=="ready"||!selectedInbox?.pick.isConnected)return false;selectedInbox.pick.focus({preventScroll:true});return true;}
  return {fragment, mount, refresh,viewState,focusSelected,setFilter, teardown:() => terminate(true)};
}
