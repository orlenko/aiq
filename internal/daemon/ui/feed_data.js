// Capture consumed JSON facts once. Never render from caller-owned objects.
const kinds = new Set(["you", "steer", "answer", "line", "notice", "skipped"]);
const subs = new Set(["prompt", "skipped_prompt", "steer", "reply", "said", "final", "earlier", "continuation", "skipped", "none"]);
const heads = new Set(["", "prompt", "steer", "notice", "reply", "earlier", "final"]);
const clocks = new Set(["", "relative", "reply"]);
const sources = new Set(["human", "aiq", "peer", "notice", "unknown"]);
const lines = new Set(["text", "table", "rule", "quote", "code"]);
const uuid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
const failure = () => { throw new TypeError("could not show conversation"); };
const require = value => { if (!value) failure(); };
function field(value, key) {
  require(value !== null && typeof value === "object" && !Array.isArray(value));
  const descriptor = Object.getOwnPropertyDescriptor(value, key);
  require(descriptor && Object.hasOwn(descriptor, "value"));
  return descriptor.value;
}
function string(value) { require(typeof value === "string"); return value; }
function boolean(value) { require(typeof value === "boolean"); return value; }
function integer(value, min = 0, max = Number.MAX_SAFE_INTEGER) {
  require(Number.isSafeInteger(value) && value >= min && value <= max); return value;
}
function word(value, catalog) { require(catalog.has(value)); return value; }
function array(value, capture) {
  require(Array.isArray(value));
  const length = Object.getOwnPropertyDescriptor(value, "length").value;
  const out = [];
  for (let i = 0; i < length; i++) {
    const descriptor = Object.getOwnPropertyDescriptor(value, i);
    require(descriptor && Object.hasOwn(descriptor, "value"));
    out.push(capture(descriptor.value));
  }
  return out;
}
function time(value) {
  const ms = integer(field(value, "ms"), -Number.MAX_SAFE_INTEGER);
  const known = boolean(field(value, "known"));
  require(known || ms === 0);
  return {ms, known};
}
function scope(value) {
  const provider = string(field(value, "provider")), id = string(field(value, "id"));
  require(["claude", "codex", "agy"].includes(provider) && uuid.test(id));
  return {provider, id};
}
function head(value) {
  return {mode: word(field(value, "mode"), heads), who: string(field(value, "who")),
    clock_mode: word(field(value, "clock_mode"), clocks), clock_text: string(field(value, "clock_text")),
    at: time(field(value, "at")), duration_known: boolean(field(value, "duration_known")),
    duration_ms: integer(field(value, "duration_ms"), -9223372036854, 9223372036854), tools: integer(field(value, "tools"))};
}
function group(value) {
  return {ordinal: integer(field(value, "ordinal")), visible: boolean(field(value, "visible")),
    leading_break: boolean(field(value, "leading_break")), empty_body_line: boolean(field(value, "empty_body_line")),
    head: head(field(value, "head")), base: array(field(value, "base"), string),
    lines: array(field(value, "lines"), value => ({kind: word(field(value, "kind"), lines),
      table_rows: integer(field(value, "table_rows")), spans: array(field(value, "spans"), value => ({
        c: array(field(value, "c"), string), d: boolean(field(value, "d")), t: string(field(value, "t"))}))}))};
}
function message(value) {
  const out = {id: string(field(value, "id")), key: string(field(value, "key")),
    kind: word(field(value, "kind"), kinds), sub: word(field(value, "sub"), subs), turn: integer(field(value, "turn"), -1),
    at: time(field(value, "at")), source_known: boolean(field(value, "source_known")), source: word(field(value, "source"), sources),
    account_known: boolean(field(value, "account_known")), account: string(field(value, "account")),
    raw: string(field(value, "raw")), groups: array(field(value, "groups"), group)};
  require(/^[0-9a-f]{32}$/.test(out.id) && /^[0-9a-f]{64}$/.test(out.key));
  require(!out.source_known || out.source !== "unknown");
  if (!out.account_known) out.account = "";
  return out;
}

export function captureConversation(data, expectedScope) {
  try {
    const expected = scope(expectedScope), session = scope(field(data, "session"));
    require(session.provider === expected.provider && session.id === expected.id);
    const out = {build: string(field(data, "build")), session,
      earlier_hidden: integer(field(data, "earlier_hidden")), later_hidden: integer(field(data, "later_hidden")),
      window_start: integer(field(data, "window_start")), reset: boolean(field(data, "reset")),
      markup: boolean(field(data, "markup")), messages: array(field(data, "messages"), message)};
    require(out.reset && out.earlier_hidden === out.window_start);
    const ids = new Set(); let ordinal = 0;
    for (const m of out.messages) {
      require(!ids.has(m.id)); ids.add(m.id);
      if (m.turn === -1) {
        require(out.window_start === 0 && m.kind === "skipped" && m.sub === "skipped" &&
          !m.source_known && !m.account_known && !m.at.known);
      } else require(m.turn >= out.window_start);
      for (const g of m.groups) require(g.ordinal === ordinal++);
    }
    return out;
  } catch { failure(); }
}

// A definition on wire milliseconds, not discarded source nanoseconds.
export function durationText(ms) {
  if (ms < 60000) return `${Math.trunc(ms / 1000)}s`;
  if (ms < 3600000) return `${Math.trunc(ms / 60000)}m`;
  return `${Math.trunc(ms / 3600000)}h${String(Math.trunc(ms / 60000) % 60).padStart(2, "0")}m`;
}
