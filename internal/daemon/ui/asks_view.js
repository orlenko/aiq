import {captureAsks} from "./asks_data.js";
import {renderLines} from "./dom.js";

const labels = ["Asks", "Warnings", "Other open", "Probably done", "Closed and dismissed"];
function clock(value) {
  if (!value.known) return "unknown";
  const date = new Date(value.ms);
  return Number.isNaN(date.getTime()) ? `milliseconds since epoch: ${value.ms}` : date.toISOString();
}

// A supplied snapshot only. Its parent owns readers, authentication and replacement.
export function createAsksView(document, data, options) {
  const {window, idPrefix} = options || {};
  let {copyText, announce, onConversation} = options || {};
  if (!window || window !== document.defaultView || typeof idPrefix !== "string" ||
      !/^[A-Za-z][A-Za-z0-9_-]*$/.test(idPrefix) ||
      ![copyText, announce, onConversation].every(f => typeof f === "function")) {
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
  let phase = "detached", flight = null;
  function node(tag, className, text) {
    const n = document.createElement(tag); n.className = className;
    if (text !== undefined) n.textContent = text;
    return n;
  }
  function button(text) {
    const n = node("button", "nav-asks-button", text); n.type = "button"; return n;
  }
  function listen(n, callback) {
    n.addEventListener("click", callback); removes.push(() => n.removeEventListener("click", callback));
  }
  function active() { return phase === "ready" || phase === "activating" || phase === "transitioning"; }
  function eligible(row, intent = row.ready) {
    return phase === "ready" && root?.isConnected && row.group?.expanded && intent && row.ready === intent;
  }
  function revoke(row) {
    if (row.ready) row.ready.copy = "";
    row.ready = null;
  }
  function controls() {
    for (const row of rows) {
      row.copy.setAttribute("aria-disabled", !eligible(row) || flight ? "true" : "false");
      row.nav.setAttribute("aria-disabled", !eligible(row) || !row.data.selectable ? "true" : "false");
    }
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
  }
  function copy(row) {
    if (!eligible(row) || flight) return;
    const intent = row.ready, text = intent.copy, token = {row, intent, settled:false};
    flight = token; controls();
    function settle(ok) {
      if (token.settled || flight !== token) return;
      token.settled = true;
      try {
        if (eligible(row, intent)) {
          const message = ok ? "Quote copied" : "Could not copy quote";
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
    try { onConversation({provider:row.data.provider, id:row.data.session_id}); }
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
    for (const row of rows) for (const key of Object.keys(row)) row[key] = null;
    rows.length = 0; groups.length = 0; snapshot = null;
    copyText = null; announce = null; onConversation = null;
    for (const candidate of candidates) cleanup(candidate);
    if (dispose) root = null;
  }

  root.append(node("p", "nav-asks-scope", snapshot.scope.mode === "all" ? "All directories" : snapshot.scope.dir));
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
  snapshot.sections.forEach((section, index) => {
    const group = {expanded:index < 2, rows:[], region:node("div", "nav-asks-region"),
      button:button(`${labels[index]} (${section.items.length})`)};
    group.region.id = `${idPrefix}-group-${index}`; group.region.hidden = !group.expanded;
    group.button.setAttribute("aria-controls", group.region.id);
    group.button.setAttribute("aria-expanded", group.expanded ? "true" : "false");
    const heading = node("h2", "nav-asks-group-heading"); heading.append(group.button);
    group.region.setAttribute("role", "region"); heading.id = `${group.region.id}-heading`;
    group.region.setAttribute("aria-labelledby", heading.id); root.append(heading, group.region);
    groups.push(group); listen(group.button, () => toggleGroup(group));
    section.items.forEach((data, ordinal) => {
      const article = node("article", "nav-asks-item"); article.id = `${group.region.id}-item-${ordinal}`;
      const quote = node("div", "nav-asks-body"), details = node("div", "nav-asks-details");
      quote.id = `${article.id}-quote`; details.id = `${article.id}-details`; details.hidden = true;
      const row = {group, data, article, quote, details, wording:node("p", "nav-asks-wording", "Original wording"),
        expanded:false, generation:0n, ready:null,
        current:null, toggle:button("Show details"), copy:button("Copy quote"), nav:button("Open conversation")};
      rows.push(row); group.rows.push(row); group.region.append(article);
      article.append(node("h3", "nav-asks-item-heading", `Ask ${data.id}`),
        node("p", "nav-asks-source", `${data.provider}: ${data.session_id}`));
      if (data.state !== "open") article.append(node("p", "nav-asks-state", `${data.state}: ${data.state_text} (${data.state_reason})`));
      article.append(row.wording, quote, row.toggle, row.copy, row.nav, details);
      row.toggle.setAttribute("aria-controls", `${quote.id} ${details.id}`); row.toggle.setAttribute("aria-expanded", "false");
      row.copy.setAttribute("aria-describedby", quote.id);
      row.nav.disabled = !data.selectable;
      if (!data.selectable) row.nav.textContent = "Conversation unavailable";
      details.append(node("p", "", data.workspace), node("p", "", `Raised: ${clock(data.raised_at)}; changed: ${clock(data.state_at)}; repeats: ${data.repeats}`));
      for (const ref of data.refs) details.append(node("p", "nav-asks-ref", ref));
      listen(row.toggle, () => { if (eligible(row)) transaction(() => activate(row, !row.expanded)); });
      listen(row.copy, () => copy(row)); listen(row.nav, () => navigate(row));
    });
  });
  snapshot = null; controls();
  function mount() {
    if (phase === "ready") return true;
    if (phase !== "detached" || !root.isConnected || root.ownerDocument !== document) return false;
    phase = "activating"; root.setAttribute("aria-busy", "true");
    try {
      for (const group of groups) if (group.expanded) for (const row of group.rows) {
        if (!activate(row, false)) return false;
      }
      if (!active()) return false;
      finish(); return true;
    } catch { terminate(false); return false; }
  }
  function refresh() {
    transaction(() => {
      for (const row of rows) if (row.group.expanded && row.current && !protectedRefresh(row, row.current)) return;
    });
  }
  return {fragment, mount, refresh, teardown:() => terminate(true)};
}
