// Consume JSON line facts only. Text never selects tags, attributes or URLs.
const kinds = new Set(["text", "table", "rule", "quote", "code"]);
const classes = new Map([
  "b", "dim", "i", "u", "inverse", "band", "you", "agent", "muted", "accent", "code",
].map(name => [name, `nav-md-${name}`]));

export function renderLines(document, lines, options) {
  if (!options || typeof options.markup !== "boolean" ||
      !options.window || document.defaultView !== options.window) {
    throw new TypeError("line rendering requires a document, its window and a markup mode");
  }
  const markup = options.markup;
  const window = options.window;
  const fragment = document.createDocumentFragment();
  let root = document.createElement("div");
  root.className = "nav-md";
  fragment.append(root);
  const groups = [];
  let runKind = "", run = null;
  for (const value of Array.isArray(lines) ? lines : []) {
    const line = value && typeof value === "object" && !Array.isArray(value) ? value : {};
    const kind = kinds.has(line.kind) ? line.kind : "text";
    const grouped = kind === "quote" || kind === "code" || kind === "table";
    if (!grouped) {
      runKind = "";
      run = null;
    } else if (kind !== runKind) {
      runKind = kind;
      run = document.createElement(kind === "quote" ? "blockquote" : "div");
      run.className = kind === "quote" ? "nav-md-quote" : "nav-md-scroll";
      root.append(run);
      if (kind !== "quote") {
        groups.push({node: run, label: kind === "table" ? "scrolls sideways" : "code, scrolls sideways"});
      }
    }
    const parent = run || root;
    if (kind === "table" && Number.isSafeInteger(line.table_rows) && line.table_rows > 0) {
      const prefix = document.createElement("span");
      prefix.className = "nav-md-sr";
      prefix.textContent = `table, ${line.table_rows} rows`;
      parent.append(prefix);
    }
    const element = document.createElement("div");
    element.className = "nav-md-line";
    parent.append(element);
    if (kind === "rule" && !markup) {
      element.classList.add("nav-md-rule");
      element.setAttribute("aria-hidden", "true");
      continue;
    }
    for (const span of Array.isArray(line.spans) ? line.spans : []) {
      if (!span || typeof span !== "object" || Array.isArray(span) || typeof span.t !== "string") continue;
      const node = document.createElement("span");
      if (Array.isArray(span.c)) {
        for (const name of span.c) {
          if (classes.has(name)) node.classList.add(classes.get(name));
        }
      }
      if (span.d === true) node.setAttribute("aria-hidden", "true");
      node.textContent = span.t;
      element.append(node);
    }
  }

  let mounted = false, disposed = false, frame = null, observer = null;
  const listeners = [];
  function measure() {
    // Read geometry before writing attributes. Hidden/detached is not fitting.
    const states = groups.map(({node}) => !node.isConnected || node.getClientRects().length === 0 ? null :
      node.scrollWidth > node.clientWidth || node.contains(document.activeElement));
    groups.forEach(({node, label}, i) => {
      if (states[i] === null) return;
      if (states[i]) {
        node.setAttribute("tabindex", "0");
        node.setAttribute("role", "group");
        node.setAttribute("aria-label", label);
      } else {
        node.removeAttribute("tabindex");
        node.removeAttribute("role");
        node.removeAttribute("aria-label");
      }
    });
  }
  function refresh() {
    if (!mounted || disposed || frame !== null) return;
    frame = window.requestAnimationFrame(() => {
      frame = null;
      if (!disposed && mounted) measure();
    });
  }
  function listen(target, name, callback) {
    target.addEventListener(name, callback);
    listeners.push(() => target.removeEventListener(name, callback));
  }
  function teardown() {
    if (disposed) return;
    disposed = true;
    mounted = false;
    if (frame !== null) window.cancelAnimationFrame(frame);
    frame = null;
    if (observer) observer.disconnect();
    observer = null;
    for (const remove of listeners) remove();
    listeners.length = 0;
    groups.length = 0;
    root = null;
  }
  function mount() {
    if (disposed) return false;
    if (mounted) return true;
    if (!root.isConnected || root.ownerDocument !== document) return false;
    mounted = true;
    if (groups.length === 0) return true;
    try {
      if (typeof window.ResizeObserver === "function") {
        observer = new window.ResizeObserver(refresh);
        for (const {node} of groups) observer.observe(node);
      }
      listen(window, "resize", refresh);
      if (window.visualViewport) listen(window.visualViewport, "resize", refresh);
      for (const {node} of groups) {
        listen(node, "focusin", refresh);
        listen(node, "focusout", event => {
          if (!event.relatedTarget || !node.contains(event.relatedTarget)) refresh();
        });
      }
      if (document.fonts) {
        listen(document.fonts, "loadingdone", refresh);
        document.fonts.ready.then(refresh, () => {});
      }
      refresh();
      return true;
    } catch {
      teardown();
      throw new Error("could not activate conversation lines");
    }
  }
  return {fragment, mount, refresh, teardown};
}
