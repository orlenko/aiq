import test from 'node:test';
import assert from 'node:assert/strict';
import {renderLines} from './dom.js';

// A deliberately small DOM for schema and resource-ownership assertions.
// Widths are supplied facts here, never evidence of browser layout behavior.
class Target {
  listeners = new Map();
  addEventListener(name, fn) {
    if (!this.listeners.has(name)) this.listeners.set(name, new Set());
    this.listeners.get(name).add(fn);
  }
  removeEventListener(name, fn) { this.listeners.get(name)?.delete(fn); }
  emit(name, event = {}) { for (const fn of [...(this.listeners.get(name) || [])]) fn(event); }
  count() { return [...this.listeners.values()].reduce((n, set) => n + set.size, 0); }
}
class Element extends Target {
  constructor(document, tag) {
    super(); this.ownerDocument = document; this.tagName = tag; this.children = [];
    this.parentNode = null; this.attrs = new Map(); this.className = ''; this.value = '';
    this.clientWidth = 100; this.scrollWidth = 100; this.scrollLeft = 0; this.visible = true;
    this.classList = {add: name => {
      const names = new Set(this.className.split(' ').filter(Boolean)); names.add(name);
      this.className = [...names].join(' ');
    }};
  }
  append(...nodes) {
    for (const node of nodes) {
      if (node.tagName === '#fragment') { this.append(...node.children); node.children = []; continue; }
      if (node.parentNode) node.parentNode.children = node.parentNode.children.filter(n => n !== node);
      node.parentNode = this; this.children.push(node);
    }
  }
  set textContent(value) { assert.equal(typeof value, 'string', 'unexpected DOM coercion'); this.value = value; this.children = []; }
  get textContent() { return this.value + this.children.map(n => n.textContent).join(''); }
  setAttribute(name, value) { this.attrs.set(name, value); }
  removeAttribute(name) { this.attrs.delete(name); }
  getAttribute(name) { return this.attrs.get(name) ?? null; }
  contains(node) { return this === node || this.children.some(n => n.contains(node)); }
  get isConnected() { return this === this.ownerDocument.body || Boolean(this.parentNode?.isConnected); }
  getClientRects() { return this.isConnected && this.visible ? [{}] : []; }
}
function fixture({observer = true} = {}) {
  const window = new Target(), viewport = new Target(), fonts = new Target();
  const frames = new Map(), callbacks = [], observers = [];
  let frameID = 0, resolveFonts, creates = 0;
  fonts.ready = new Promise(resolve => { resolveFonts = resolve; });
  window.visualViewport = viewport;
  window.requestAnimationFrame = fn => { const id = ++frameID; frames.set(id, fn); callbacks.push(fn); return id; };
  window.cancelAnimationFrame = id => frames.delete(id);
  const document = {defaultView: window, activeElement: null, fonts,
    createElement(tag) { creates++; assert.ok(['div','span','blockquote','button'].includes(tag)); return new Element(document, tag); },
    createDocumentFragment() { creates++; return new Element(document, '#fragment'); },
  };
  document.body = new Element(document, 'body');
  if (observer) window.ResizeObserver = class {
    constructor(callback) { this.callback = callback; this.nodes = []; this.disconnected = false; observers.push(this); }
    observe(node) { this.nodes.push(node); }
    disconnect() { this.disconnected = true; this.nodes = []; }
  };
  return {window, document, viewport, fonts, observers, frames, callbacks,
    get creates() { return creates; }, resolveFonts,
    flush() { const queued = [...frames.values()]; frames.clear(); for (const fn of queued) fn(); },
    render(lines, markup = false) { return renderLines(document, lines, {window, markup}); },
  };
}
const text = t => ({kind: 'text', spans: [{t}]});
const scroll = (kind = 'code', t = 'body') => ({kind, spans: [{t}]});
const all = node => [node, ...node.children.flatMap(all)];
const attrs = node => ['tabindex','role','aria-label'].map(name => node.getAttribute(name));

test('closed wire schema inserts hostile strings as text, without object coercion', () => {
  const f = fixture();
  const hostile = '<img src=x onerror=alert(1)><svg><script>fixture</script></svg>';
  const object = {toString() { throw new Error('coerced'); }};
  const result = f.render([
    null, [], 9, {kind:'constructor', spans:[{t:hostile, c:['b','__proto__','constructor',object,'b'], d:1}]},
    {kind:'__proto__', spans:[null, [], {t:object}, {t:42}, {t:'kept', c:'b', d:'true'}]},
    {kind:object, spans:[{t:'plain', c:['toString',false,'code'], d:true}]},
    {kind:'text', spans:'wrong'}, {kind:'text', spans:[{text:'not wire t', classes:['b']}]},
  ]);
  const root = result.fragment.children[0];
  assert.equal(root.className, 'nav-md');
  assert.equal(root.children.length, 8);
  assert.deepEqual(root.children.map(n => n.tagName), Array(8).fill('div'));
  assert.equal(root.children[3].textContent, hostile);
  assert.equal(root.children[3].children[0].className, 'nav-md-b');
  assert.equal(root.children[3].children[0].getAttribute('aria-hidden'), null);
  assert.equal(root.children[4].textContent, 'kept');
  assert.equal(root.children[4].children[0].className, '');
  assert.equal(root.children[5].children[0].className, 'nav-md-code');
  assert.equal(root.children[5].children[0].getAttribute('aria-hidden'), 'true');
  assert.equal(root.children[6].textContent + root.children[7].textContent, '');
  assert.ok(all(root).every(n => [...n.attrs.keys()].every(a => a === 'aria-hidden')));
  assert.equal(f.observers.length + f.window.count() + f.frames.size, 0);
});

test('all fixed classes, exact decoration boolean and literal lookalikes', () => {
  const f = fixture(), names = ['b','dim','i','u','inverse','band','you','agent','muted','accent','code'];
  const root = f.render([text('│ ─ >'), {kind:'text', spans:names.map((name,i) => ({t:name, c:[name], d:i === 0}))}]).fragment.children[0];
  assert.equal(root.children[0].children[0].getAttribute('aria-hidden'), null);
  assert.deepEqual(root.children[1].children.map(n => n.className), ['nav-md-b','nav-md-dim','nav-md-i','nav-md-u','nav-md-inverse','nav-md-band','nav-md-you','nav-md-agent','nav-md-muted','nav-md-accent','nav-md-code']);
  assert.deepEqual(root.children[1].children.map(n => n.getAttribute('aria-hidden')), ['true',...Array(10).fill(null)]);
});

test('required boolean mode fails before construction; mode is captured by value', () => {
  for (const mode of [undefined,null,0,1,'false',{},[]]) {
    const f = fixture();
    assert.throws(() => renderLines(f.document, [], {window:f.window, markup:mode}), TypeError);
    assert.equal(f.creates, 0);
  }
  const f = fixture();
  for (const options of [undefined, {}, {window:{},markup:false}]) assert.throws(() => renderLines(f.document, [], options), TypeError);
  assert.equal(f.creates, 0);
  const options = {window:f.window,markup:false};
  const result = renderLines(f.document, [{kind:'rule',spans:[{t:'───',d:false}]}], options);
  options.markup = true;
  const rule = result.fragment.children[0].children[0];
  assert.equal(rule.className, 'nav-md-line nav-md-rule');
  assert.equal(rule.textContent, ''); assert.equal(rule.getAttribute('aria-hidden'), 'true');
  const kept = f.render([{kind:'rule',spans:[{t:'---'}]}], true).fragment.children[0].children[0];
  assert.equal(kept.className, 'nav-md-line'); assert.equal(kept.textContent, '---');
  assert.equal(kept.getAttribute('aria-hidden'), null);
});

test('kind grouping preserves blanks and announces every actual table marker', () => {
  const f = fixture();
  const root = f.render([
    {kind:'quote',spans:[{t:'q1'}]}, {kind:'quote',spans:[{t:'q2'}]},
    {...scroll('table','one'),table_rows:2}, scroll('table','tail'),
    {...scroll('table','adjacent'),table_rows:3}, scroll('code','code'), text(''), text('after'),
  ]).fragment.children[0];
  assert.deepEqual(root.children.map(n => [n.tagName,n.className]), [
    ['blockquote','nav-md-quote'], ['div','nav-md-scroll'], ['div','nav-md-scroll'], ['div','nav-md-line'], ['div','nav-md-line'],
  ]);
  assert.deepEqual(root.children[0].children.map(n => n.textContent), ['q1','q2']);
  assert.deepEqual(root.children[1].children.map(n => [n.className,n.textContent]), [
    ['nav-md-sr','table, 2 rows'],['nav-md-line','one'],['nav-md-line','tail'],['nav-md-sr','table, 3 rows'],['nav-md-line','adjacent'],
  ]);
  assert.equal(root.children[3].textContent, '');
  assert.ok(all(root).every(n => !['table','h1','h2','h3'].includes(n.tagName)));
  for (const count of [0,-1,1.5,NaN,Infinity,Number.MAX_SAFE_INTEGER+1,'2',null]) {
    const table = f.render([{...scroll('table'),table_rows:count}]).fragment.children[0].children[0];
    assert.deepEqual(table.children.map(n => n.className), ['nav-md-line']);
  }
  for (const lines of [null,{},'bad']) assert.equal(f.render(lines).fragment.children[0].children.length, 0);
});

test('returned trees own their nodes and capture input values', () => {
  const f = fixture(), input = [text('original')];
  input[0].spans[0].c = ['b'];
  const a = f.render(input), b = f.render(input);
  input[0].spans[0].t = 'changed'; input[0].spans[0].c.push('code'); input.push(text('new'));
  const first = a.fragment.children[0].children[0].children[0];
  first.textContent = 'mutated output'; first.classList.add('changed');
  assert.equal(b.fragment.textContent, 'original');
  assert.equal(b.fragment.children[0].children[0].children[0].className, 'nav-md-b');
  assert.notEqual(a.fragment.children[0], b.fragment.children[0]);
});

test('mount retry, consumed fragment, initial frame and signals coalesce', async () => {
  const f = fixture(), result = f.render([scroll('table'),scroll('code')]);
  const root = result.fragment.children[0];
  assert.equal(result.mount(), false);
  result.refresh(); assert.equal(f.observers.length + f.frames.size + f.window.count(), 0);
  f.document.body.append(result.fragment);
  assert.equal(result.fragment.children.length, 0);
  root.children[0].scrollWidth = 101;
  assert.equal(result.mount(), true); assert.equal(result.mount(), true);
  assert.equal(f.observers.length, 1); assert.equal(f.observers[0].nodes.length, 2);
  assert.equal(f.frames.size, 1); assert.deepEqual(attrs(root.children[0]), [null,null,null]);
  f.window.emit('resize'); f.viewport.emit('resize'); f.fonts.emit('loadingdone'); f.observers[0].callback();
  f.resolveFonts(); await Promise.resolve();
  assert.equal(f.frames.size, 1); f.flush();
  assert.deepEqual(attrs(root.children[0]), ['0','group','scrolls sideways']);
  assert.deepEqual(attrs(root.children[1]), [null,null,null]);
  root.children[1].scrollWidth = 120; result.refresh(); f.flush();
  assert.deepEqual(attrs(root.children[1]), ['0','group','code, scrolls sideways']);
  result.teardown(); result.teardown();
  assert.equal(f.window.count() + f.viewport.count() + f.fonts.count() + f.frames.size, 0);
  assert.ok(root.children.every(n => n.count() === 0));
  assert.equal(f.observers[0].disconnected, true); assert.equal(result.mount(), false);
});

test('observer-free activation, hidden refresh and settled focus retain attributes together', () => {
  const f = fixture({observer:false}), result = f.render([scroll()]);
  const root = result.fragment.children[0], group = root.children[0];
  f.document.body.append(result.fragment); group.scrollWidth = 200;
  assert.equal(result.mount(), true); f.flush();
  assert.deepEqual(attrs(group), ['0','group','code, scrolls sideways']);
  group.scrollLeft = 31; group.scrollWidth = 100; f.document.activeElement = group;
  f.window.emit('resize'); f.flush();
  assert.deepEqual(attrs(group), ['0','group','code, scrolls sideways']); assert.equal(group.scrollLeft, 31);
  const child = f.document.createElement('button'); group.append(child);
  f.document.activeElement = child; group.emit('focusout',{relatedTarget:child});
  assert.equal(f.frames.size, 0); result.refresh(); f.flush();
  assert.deepEqual(attrs(group), ['0','group','code, scrolls sideways']);
  f.document.activeElement = f.document.body; group.visible = false;
  group.emit('focusout',{relatedTarget:f.document.body}); f.flush();
  assert.deepEqual(attrs(group), ['0','group','code, scrolls sideways']);
  group.visible = true; result.refresh(); f.flush();
  assert.deepEqual(attrs(group), [null,null,null]); assert.equal(group.scrollLeft, 31);
  group.scrollWidth = 200; f.viewport.emit('resize'); f.flush();
  assert.deepEqual(attrs(group), ['0','group','code, scrolls sideways']);
  result.teardown();
});

test('terminal disposal guards queued observer, frame and font callbacks', async () => {
  const f = fixture(), result = f.render([scroll()]);
  const root = result.fragment.children[0]; f.document.body.append(result.fragment); result.mount();
  const pendingFrame = f.callbacks[0], pendingObserver = f.observers[0].callback;
  result.teardown(); root.children[0].scrollWidth = 999;
  pendingFrame(); pendingObserver(); f.resolveFonts(); await Promise.resolve();
  f.window.emit('resize'); f.fonts.emit('loadingdone'); result.refresh();
  assert.equal(f.frames.size, 0); assert.deepEqual(attrs(root.children[0]), [null,null,null]);
  assert.equal(result.mount(), false);
  const early = f.render([scroll()]); early.teardown();
  f.document.body.append(early.fragment); assert.equal(early.mount(), false);
});

test('partial observer setup failure releases prior resources and is terminal', () => {
  const f = fixture(); let disconnects = 0;
  f.window.ResizeObserver = class { observe() { throw new Error('fixture failure'); } disconnect() { disconnects++; } };
  const result = f.render([scroll()]); f.document.body.append(result.fragment);
  assert.throws(() => result.mount(), {message:'could not activate conversation lines'});
  assert.equal(disconnects, 1); assert.equal(result.mount(), false);
  assert.equal(f.window.count() + f.frames.size + f.fonts.count(), 0);
});

test('listener setup failure unwinds an already installed observer and window listener', () => {
  const f = fixture();
  f.viewport.addEventListener = () => { throw new Error('fixture listener failure'); };
  const result = f.render([scroll('table'), scroll('code')]);
  f.document.body.append(result.fragment);
  assert.throws(() => result.mount(), {message:'could not activate conversation lines'});
  assert.equal(f.observers.length, 1); assert.equal(f.observers[0].disconnected, true);
  assert.equal(f.window.count() + f.viewport.count() + f.fonts.count() + f.frames.size, 0);
  f.observers[0].callback(); result.refresh();
  assert.equal(f.frames.size, 0); assert.equal(result.mount(), false);
});
