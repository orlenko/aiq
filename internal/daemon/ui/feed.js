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
  const readingOption = Object.getOwnPropertyDescriptor(options, "readingControls");
  const readingControls = readingOption ? readingOption.value : false;
  const shortcutOption = Object.getOwnPropertyDescriptor(options, "shortcutsEnabled");
  let shortcutsEnabled = shortcutOption ? shortcutOption.value : () => true;
  if (readingOption && (!Object.hasOwn(readingOption,"value") || typeof readingControls !== "boolean") ||
      shortcutOption && (!Object.hasOwn(shortcutOption,"value") || typeof shortcutsEnabled !== "function")) {
    throw new TypeError("could not show conversation");
  }
  let snapshot = captureConversation(data, options.expectedScope);
  const fragment = document.createDocumentFragment();
  let root = document.createElement("section");
  root.className = "nav-feed"; root.setAttribute("aria-label", "Conversation");
  fragment.append(root);
  const helpers = [], controls = [], removes = [];
  const messages = [], prompts = [];
  let position = -1;
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
  function active() { return mounted && !disposed && root?.isConnected; }
  function focusedPosition(target) {
    for (const p of prompts) if (p.button.contains(target) || p.heading.contains(target)) return p.position;
    return messages.find(m => m.node.contains(target))?.position ?? -1;
  }
  function moveTo(p) {
    if (!active() || !p.heading.isConnected) return;
    position = p.position;
    p.heading.focus({preventScroll:true});
    if (!active()) return;
    p.heading.scrollIntoView({block:"center", behavior:"auto"});
    if (active()) status(`Prompt ${prompts.indexOf(p)+1} of ${prompts.length}.`);
  }
  function step(direction) {
    if (!active()) return;
    const focused = focusedPosition(document.activeElement);
    if (focused >= 0) position = focused;
    const p = position < 0 ? (direction > 0 ? prompts[0] : prompts[prompts.length-1]) :
      (direction > 0 ? prompts.find(p => p.position > position) : prompts.findLast(p => p.position < position));
    if (!active()) return;
    if (p) moveTo(p);
    else status(direction > 0 ? "Last prompt." : "First prompt.");
  }
  function readingKeys(event) {
    if (!active() || event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey ||
        !["n","p"].includes(event.key) || !root.contains(event.target) ||
        ["INPUT","TEXTAREA","SELECT"].includes(event.target?.tagName?.toUpperCase()) || event.target?.isContentEditable) return;
    let enabled = false;
    try { enabled = shortcutsEnabled(); } catch { return; }
    if (!active() || !enabled) return;
    event.preventDefault(); step(event.key === "n" ? 1 : -1);
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
    try { status(ok ? "copied" : "Could not copy message."); }
    finally { if (!disposed && mounted) setBusy(false); }
  }
  function teardown() {
    if (disposed) return;
    disposed = true; mounted = false;
    for (const remove of removes) remove(); removes.length = 0;
    for (const helper of helpers) helper.teardown(); helpers.length = 0;
    for (const control of controls) control.raw = "";
    controls.length = messages.length = prompts.length = 0; position = -1; shortcutsEnabled = null; snapshot = null;
    outline = previous = next = stream = null;
    if (root) root.remove(); root = null;
  }
  let outline = null, previous = null, next = null;
  let stream = readingControls ? node("div", "nav-feed-stream") : root;
  try {
    let order = 0;
    const finals = new Map();
    if (readingControls) {
      for (const m of snapshot.messages) for (const g of m.groups) if (g.visible && g.head.mode === "final") finals.set(m.turn,g.head);
      const navigation = node("nav", "nav-feed-navigation"); navigation.setAttribute("aria-label", "Prompt navigation");
      previous = node("button", "nav-feed-previous", "Previous prompt"); next = node("button", "nav-feed-next", "Next prompt");
      previous.type = next.type = "button"; navigation.append(previous,next);
      outline = node("details", "nav-feed-outline");
      root.append(navigation,outline,stream);
    }
    for (const message of snapshot.messages) {
      const article = node("article", "nav-feed-message");
      article.id = `${idPrefix}-${message.id}`;
      stream.append(article);
      const messagePosition = {node:article,position:++order};
      if (readingControls) messages.push(messagePosition);
      const body = node("div", "nav-feed-body");
      body.id = `${article.id}-body`; article.append(body);
      let visible = false;
      for (const group of message.groups) {
        if (!group.visible) continue;
        visible = true;
        if (group.leading_break) body.append(node("div", "nav-feed-break"));
        const head = heading(group.head); if (head) body.append(head);
        if (readingControls && head && ["prompt","steer"].includes(group.head.mode)) {
          head.id = `${article.id}-prompt-${group.ordinal}`; head.tabIndex = -1;
          const button = node("button", "nav-feed-outline-prompt", head.textContent); button.type = "button";
          button.setAttribute("aria-controls",head.id);
          const item = node("li", "nav-feed-outline-item"); item.append(button);
          const final = finals.get(message.turn);
          if (final) {
            const facts = [final.duration_known ? durationText(final.duration_ms) : "",`${final.tools} tool${final.tools === 1 ? "" : "s"}`].filter(Boolean).join(" · ");
            item.append(node("span", "nav-feed-outline-facts", facts));
          }
          messagePosition.position = ++order;
          prompts.push({heading:head,button,item,position:order});
        }
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
    if (readingControls) {
      const list = node("ol", "nav-feed-outline-list"); for (const p of prompts) list.append(p.item);
      outline.append(node("summary", "", `Prompts (${prompts.length})`),list);
      previous.disabled = next.disabled = prompts.length === 0;
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
      if (readingControls) {
        const listen = (target,type,handler) => { target.addEventListener(type,handler); removes.push(() => target.removeEventListener(type,handler)); };
        listen(previous,"click",() => step(-1)); listen(next,"click",() => step(1));
        listen(root,"keydown",readingKeys);
        listen(root,"focusin",event => { if(active()) { const p=focusedPosition(event.target); if(p>=0)position=p; } });
        for (const p of prompts) listen(p.button,"click",() => moveTo(p));
      }
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
