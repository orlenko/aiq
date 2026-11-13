import {renderLines} from "./dom.js";
import {captureConversation, durationText} from "./feed_data.js";

// A detached reset snapshot. Fetching, retirement and navigation belong to its host.
export function createConversationFeed(document, data, options) {
  if (!options || !options.window || options.window !== document.defaultView ||
      typeof options.idPrefix !== "string" || !/^[A-Za-z][A-Za-z0-9_-]*$/.test(options.idPrefix) ||
      typeof options.copyText !== "function" || typeof options.announce !== "function") {
    throw new TypeError("could not show conversation");
  }
  const {window, idPrefix, copyText, announce} = options;
  let snapshot = captureConversation(data, options.expectedScope);
  const fragment = document.createDocumentFragment();
  let root = document.createElement("section");
  root.className = "nav-feed"; root.setAttribute("aria-label", "Conversation");
  fragment.append(root);
  const helpers = [], controls = [], removes = [];
  let mounted = false, disposed = false, busy = false;
  function node(tag, className, text) {
    const n = document.createElement(tag); n.className = className;
    if (text !== undefined) n.textContent = text;
    return n;
  }
  function heading(head) {
    if (head.mode === "" || head.mode === "notice") return null;
    let text = head.who;
    if (head.mode === "earlier") text += " · earlier";
    if (head.clock_mode !== "") text += " · " + head.clock_text;
    if (head.mode === "final") {
      if (head.duration_known) text += " · " + durationText(head.duration_ms);
      if (head.tools > 0) text += ` · ${head.tools} tool${head.tools === 1 ? "" : "s"}`;
    }
    const human = head.mode === "prompt" || head.mode === "steer";
    return node(human ? "h3" : "div", human ? "nav-feed-prompt" :
      head.mode === "earlier" ? "nav-feed-answer nav-feed-earlier" : "nav-feed-answer", text);
  }
  function setBusy(value) {
    busy = value;
    for (const {button} of controls) {
      if (value) button.setAttribute("aria-disabled", "true");
      else button.removeAttribute("aria-disabled");
    }
  }
  function status(text) {
    if (disposed || !mounted) return;
    try { announce(text); } catch { /* A status sink cannot reclassify a copy. */ }
  }
  function copy(control) {
    if (disposed || !mounted || busy) return;
    setBusy(true);
    let result;
    try { result = copyText(control.raw); }
    catch { settle(false); return; }
    Promise.resolve(result).then(() => settle(true), () => settle(false));
  }
  function settle(ok) {
    if (disposed || !mounted) return;
    setBusy(false);
    status(ok ? "copied" : "Could not copy message.");
  }
  function teardown() {
    if (disposed) return;
    disposed = true; mounted = false;
    for (const remove of removes) remove(); removes.length = 0;
    for (const helper of helpers) helper.teardown(); helpers.length = 0;
    for (const control of controls) control.raw = "";
    controls.length = 0; snapshot = null;
    if (root) root.remove(); root = null;
  }
  try {
    for (const message of snapshot.messages) {
      const article = node("article", "nav-feed-message");
      article.id = `${idPrefix}-${message.id}`;
      root.append(article);
      const body = node("div", "nav-feed-body");
      body.id = `${article.id}-body`; article.append(body);
      let visible = false;
      for (const group of message.groups) {
        if (!group.visible) continue;
        visible = true;
        if (group.leading_break) body.append(node("div", "nav-feed-break"));
        const head = heading(group.head); if (head) body.append(head);
        const rows = group.empty_body_line ? [...group.lines, {kind:"text", spans:[]}] : group.lines;
        const helper = renderLines(document, rows, {window, markup:snapshot.markup, base:group.base});
        helpers.push(helper); body.append(helper.fragment);
      }
      if (visible) {
        const toolbar = node("div", "nav-feed-actions"); article.append(toolbar);
        const button = node("button", "nav-feed-copy", "Copy message");
        button.type = "button"; button.setAttribute("aria-describedby", body.id); toolbar.append(button);
        if (message.account_known && message.account !== "") toolbar.append(node("span", "nav-feed-account", message.account));
        const control = {button, raw:message.raw}; controls.push(control);
      }
    }
  } catch {
    teardown();
    throw new Error("could not show conversation");
  }
  // Nodes have captured their primitives; keep no full snapshot/history graph.
  snapshot = null;
  function mount() {
    if (disposed) return false;
    if (mounted) return true;
    if (!root.isConnected || root.ownerDocument !== document) return false;
    try {
      for (const helper of helpers) if (!helper.mount()) throw new Error("line activation failed");
      mounted = true;
      for (const control of controls) {
        const click = () => copy(control);
        control.button.addEventListener("click", click);
        removes.push(() => control.button.removeEventListener("click", click));
      }
      return true;
    } catch {
      teardown(); throw new Error("could not activate conversation");
    }
  }
  function refresh() {
    if (!disposed && mounted) for (const helper of helpers) helper.refresh();
  }
  return {fragment, mount, refresh, teardown};
}
