import test from 'node:test';
import assert from 'node:assert/strict';
import {createConversationFeed} from './feed.js';
import {captureConversation, durationText} from './feed_data.js';
import {fixtureScope, fixtureHead, fixtureLine, fixtureGroup, fixtureMessage, fixtureWindow} from './feed_values.mjs';

// Supplied-width synthetic DOM: ownership evidence, never layout/CSP proof.
class Target {
  listeners = new Map();
  addEventListener(n,f) { if (!this.listeners.has(n)) this.listeners.set(n,new Set()); this.listeners.get(n).add(f); }
  removeEventListener(n,f) { this.listeners.get(n)?.delete(f); }
  emit(n) { for(const f of [...(this.listeners.get(n)||[])]) f({}); }
  count() { return [...this.listeners.values()].reduce((n,s)=>n+s.size,0); }
}
class Element extends Target {
  constructor(document, tag) {
    super();this.ownerDocument=document;this.tagName=tag;this.children=[];this.parentNode=null;this.attrs=new Map();
    this.className='';this.value='';this.clientWidth=100;this.scrollWidth=100;
    this.classList={add:name=>{this.className=[...new Set([...this.className.split(' ').filter(Boolean),name])].join(' ');}};
  }
  append(...nodes) { for(const n of nodes) {
    if(n.tagName==='#fragment') {this.append(...n.children);n.children=[];continue;}
    n.remove();n.parentNode=this;this.children.push(n);
  } }
  remove() { if(this.parentNode) this.parentNode.children=this.parentNode.children.filter(n=>n!==this);this.parentNode=null; }
  set textContent(v) {assert.equal(typeof v,'string');this.value=v;this.children=[];}
  get textContent() {return this.value+this.children.map(n=>n.textContent).join('');}
  setAttribute(n,v) {this.attrs.set(n,v);}
  removeAttribute(n) {this.attrs.delete(n);}
  getAttribute(n) {return this.attrs.get(n)??null;}
  contains(n) {return this===n||this.children.some(c=>c.contains(n));}
  get isConnected() {return this===this.ownerDocument.body||Boolean(this.parentNode?.isConnected);}
  getClientRects() {return this.isConnected?[{}]:[];}
}
function fixture() {
  const window=new Target(),frames=new Map(),observers=[];let count=0,serial=0,resolveFonts;
  const document={defaultView:window, activeElement:null, fonts:new Target(),
    createElement(tag){count++;return new Element(document,tag);},createDocumentFragment(){count++;return new Element(document,'#fragment');}};
  document.body=new Element(document,'body');document.fonts.ready=new Promise(r=>resolveFonts=r);
  window.requestAnimationFrame=f=>{const n=++serial;frames.set(n,f);return n;};window.cancelAnimationFrame=n=>frames.delete(n);
  window.ResizeObserver=class {constructor(f){this.callback=f;this.disconnected=false;observers.push(this);}observe(){}disconnect(){this.disconnected=true;}};
  return {window,document,frames,observers,resolveFonts,get count(){return count;},flush(){for(const f of frames.values())f();frames.clear();},
    feed(data=fixtureWindow(),more={}) {return createConversationFeed(document,data,{window,idPrefix:'fixture',expectedScope:{...fixtureScope},copyText:()=>Promise.resolve(),announce:()=>{},...more});}};
}
const all=n=>[n,...n.children.flatMap(all)];
const has=(n,c)=>n.className.split(' ').includes(c);
const buttons=n=>all(n).filter(n=>n.tagName==='button');
const tick=async()=>{await Promise.resolve();await Promise.resolve();};
function mounted(f,data,options) {const feed=f.feed(data,options);f.document.body.append(feed.fragment);assert.equal(feed.mount(),true);return feed;}

test('wire snapshot rejects mismatched scope, aliases and malformed final group before allocation',()=>{
  for(const mutate of [d=>d.session.id='00000002-0000-4000-8000-000000000002',d=>d.reset=false,d=>d.earlier_hidden=1,d=>d.messages.push(d.messages[0]),
    d=>d.messages[0].groups[0].head.clock_mode='HasTime',d=>d.messages[0].groups.push({...fixtureGroup(1),lines:[{kind:'text'}]})]) {
    const f=fixture(),d=fixtureWindow();mutate(d);assert.throws(()=>f.feed(d),{message:'could not show conversation'});assert.equal(f.count,0);
  }
  const f=fixture();mounted(f,fixtureWindow([])).teardown();assert.equal(f.document.body.children.length,0);
});

test('own data snapshot rejects getters without invocation and ignores unknown traps',()=>{
  let calls=0;const d=fixtureWindow();Object.defineProperty(d.messages[0],'raw',{get(){calls++;return 'changed';}});
  const f=fixture();assert.throws(()=>f.feed(d),{message:'could not show conversation'});assert.equal(calls,0);assert.equal(f.count,0);
  const good=fixtureWindow();Object.defineProperty(good,'unused',{get(){throw new Error('unused');}});
  good.messages[Symbol.iterator]=()=>{throw new Error('input iterator');};
  good.toJSON=()=>{throw new Error('toJSON');};captureConversation(good,fixtureScope);
  const bad=fixtureWindow();bad.messages[0].raw={toString(){calls++;throw new Error('coercion');}};
  assert.throws(()=>captureConversation(bad,fixtureScope),{message:'could not show conversation'});assert.equal(calls,0);
});

test('group head recipe preserves unknown reply, final duration/tools and notice once',()=>{
  const f=fixture();const heads=[fixtureHead('prompt'),{...fixtureHead('reply'),clock_mode:'reply',clock_text:'reply'},
    {...fixtureHead('final'),duration_known:true,duration_ms:0,tools:2},fixtureHead('notice')];
  const groups=heads.map((h,i)=>fixtureGroup(i,h,[fixtureLine(i===3?'· peer agent: notice':'body')]));
  const feed=mounted(f,fixtureWindow([fixtureMessage(1,groups)]));const nodes=all(f.document.body);
  assert.deepEqual(nodes.filter(n=>has(n,'nav-feed-prompt')||has(n,'nav-feed-answer')).map(n=>[n.tagName,n.textContent]),
    [['h3','you'],['div','agy · reply'],['div','agy · 0s · 2 tools']]);
  assert.equal(f.document.body.textContent.split('· peer agent: notice').length,2);feed.teardown();
});

test('known epoch, preamble and independently known metadata remain distinct',()=>{
  const d=fixtureWindow();d.messages[0].at={ms:0,known:true};d.messages[0].account_known=true;
  const out=captureConversation(d,fixtureScope);assert.equal(out.messages[0].at.known,true);assert.equal(out.messages[0].account_known,true);
  const pre=fixtureMessage(1,[fixtureGroup(0)]);Object.assign(pre,{turn:-1,kind:'skipped',sub:'skipped',source_known:false,source:'unknown'});
  captureConversation(fixtureWindow([pre]),fixtureScope);pre.kind='you';assert.throws(()=>captureConversation(fixtureWindow([pre]),fixtureScope));
});

test('one helper keeps runs within cuts and separates adjacent cuts and blank recipes',()=>{
  const f=fixture(),first=fixtureGroup(0,fixtureHead(),[fixtureLine('a','code'),fixtureLine('b','code')]);first.base=['muted'];
  first.leading_break=true;const hidden={...fixtureGroup(1),visible:false,leading_break:true};const last=fixtureGroup(2,fixtureHead(),[]);last.empty_body_line=true;
  const feed=mounted(f,fixtureWindow([fixtureMessage(1,[first,hidden,last,fixtureGroup(3,fixtureHead(),[fixtureLine('c','code')])])]));
  const nodes=all(f.document.body);assert.equal(nodes.filter(n=>has(n,'nav-md-scroll')).length,2);
  assert.deepEqual(nodes.filter(n=>has(n,'nav-md-scroll')).map(n=>n.children.length),[2,1]);
  assert.equal(nodes.filter(n=>has(n,'nav-feed-break')).length,1);assert.equal(nodes.filter(n=>has(n,'nav-md-line')).length,4);
  assert.equal(nodes.filter(n=>has(n,'nav-md')&&has(n,'nav-md-muted')).length,1);feed.teardown();
});

test('captured raw and options survive caller mutation and truthful copy rejection permits retry',async()=>{
  const f=fixture(),d=fixtureWindow(),calls=[],announcements=[];d.messages[0].raw='aside\n\nhidden\ncopy';
  let reject=true;const opts={copyText:raw=>{calls.push(raw);return reject?Promise.reject(new Error('invented')):Promise.resolve();},announce:s=>announcements.push(s)};
  const feed=mounted(f,d,opts);d.messages[0].raw='changed';d.messages[0].groups[0].lines[0].spans[0].t='changed';opts.copyText=()=>assert.fail('mutated option used');
  const button=buttons(f.document.body)[0];button.emit('click');button.emit('click');await tick();
  assert.deepEqual(calls,['aside\n\nhidden\ncopy']);assert.deepEqual(announcements,['Could not copy message.']);
  reject=false;button.emit('click');await tick();assert.deepEqual(calls,['aside\n\nhidden\ncopy','aside\n\nhidden\ncopy']);assert.equal(announcements[1],'copied');feed.teardown();
});

test('copy throw, reentrant thenable and throwing announcer settle once and release controls',async()=>{
  for(const mode of ['throw','thenable','then-getter']) {
    const f=fixture();let calls=0,announced=0;let button;
    const feed=mounted(f,fixtureWindow(),{copyText:()=>{calls++;button.emit('click');if(mode==='throw')throw new Error('invented');if(mode==='then-getter')return Object.defineProperty({},'then',{get(){throw new Error('invented then');}});return {then(resolve,reject){resolve();reject();resolve();}};},announce:()=>{announced++;throw new Error('sink');}});
    button=buttons(f.document.body)[0];button.emit('click');await tick();await tick();assert.equal(calls,1);assert.equal(announced,1);assert.equal(button.getAttribute('aria-disabled'),null);
    button.emit('click');await tick();await tick();assert.equal(calls,2);feed.teardown();
  }
});

test('pending copy and delayed helper callbacks have no effect after teardown',async()=>{
  const f=fixture();let resolve,announced=0;const group=fixtureGroup(0,fixtureHead(),[fixtureLine('code','code')]);
  const feed=mounted(f,fixtureWindow([fixtureMessage(1,[group])]),{copyText:()=>new Promise(r=>resolve=r),announce:()=>announced++});
  const button=buttons(f.document.body)[0];button.emit('click');const callbacks=[...f.frames.values()];feed.teardown();resolve();
  for(const callback of callbacks)callback();for(const o of f.observers)o.callback();f.resolveFonts();await tick();button.emit('click');
  assert.equal(announced,0);assert.equal(f.frames.size,0);assert.equal(f.document.body.children.length,0);assert.equal(feed.mount(),false);
});

async function announcerReentryCase(mode) {
  const f=fixture();let button,copies=0,announced=0,reentry=1;
  const feed=mounted(f,fixtureWindow(),{copyText:()=>{copies++;if(mode==='throw')throw new Error('invented failure');return mode==='reject'?Promise.reject(new Error('invented rejection')):Promise.resolve();},
    announce:()=>{announced++;if(reentry-->0)button.emit('click');}});
  button=buttons(f.document.body)[0];button.emit('click');await tick();await tick();
  assert.equal(copies,1,'announcer reentry must not admit a second copy');assert.equal(announced,1,'one action announces once');
  assert.equal(button.getAttribute('aria-disabled'),null);
  button.emit('click');await tick();await tick();assert.equal(copies,2,'later explicit retry must work');feed.teardown();
}
test('announcer reentry remains blocked through successful settlement',async()=>{await announcerReentryCase('success');});
test('announcer reentry remains blocked through synchronous failure and teardown',async()=>{
  await announcerReentryCase('throw');await announcerReentryCase('reject');
  const f=fixture();let feed,button,announced=0;feed=mounted(f,fixtureWindow(),{announce:()=>{announced++;feed.teardown();}});
  button=buttons(f.document.body)[0];let afterDispose=0;const remove=button.removeAttribute.bind(button);
  button.removeAttribute=name=>{if(!button.isConnected)afterDispose++;remove(name);};
  button.emit('click');await tick();assert.equal(announced,1);assert.equal(afterDispose,0);assert.equal(feed.mount(),false);assert.equal(f.document.body.children.length,0);
});

test('two feeds, disconnected retry, refresh and repeated teardown isolate resources and focus',()=>{
  const f=fixture(),g=fixtureWindow([fixtureMessage(1,[fixtureGroup(0,fixtureHead(),[fixtureLine('code','code')])])]);
  const a=f.feed(g),b=f.feed(g,{idPrefix:'neighbor'});assert.equal(a.mount(),false);f.document.body.append(a.fragment,b.fragment);
  assert.equal(a.mount(),true);assert.equal(b.mount(),true);const neighbor=buttons(f.document.body)[1];f.document.activeElement=neighbor;
  a.refresh();a.teardown();a.teardown();assert.equal(f.document.activeElement,neighbor);assert.equal(neighbor.isConnected,true);assert.equal(b.mount(),true);
  b.teardown();assert.equal(f.window.count()+f.document.fonts.count()+f.frames.size,0);assert.ok(f.observers.every(o=>o.disconnected));
});

test('partial activation failure disposes prepared helpers and removes only its root',()=>{
  for(const mode of ['throw','false']) {
  const f=fixture();let observes=0;f.window.ResizeObserver=class {observe(){observes++;if(mode==='throw'&&observes===2)throw new Error('invented failure');if(mode==='false'&&observes===1)all(f.document.body).filter(n=>has(n,'nav-md'))[1].remove();}disconnect(){}};
  const d=fixtureWindow([fixtureMessage(1,[fixtureGroup(0,fixtureHead(),[fixtureLine('a','code')]),fixtureGroup(1,fixtureHead(),[fixtureLine('b','code')])])]);
  const sentinel=f.document.createElement('div');f.document.body.append(sentinel);const feed=f.feed(d);f.document.body.append(feed.fragment);
  assert.throws(()=>feed.mount(),{message:'could not activate conversation'});assert.deepEqual(f.document.body.children,[sentinel]);assert.equal(feed.mount(),false);
  assert.equal(f.window.count()+f.document.fonts.count()+f.frames.size,0);
  }
});

test('wire duration literals retain signed branches and explicit millisecond precision',()=>{
  // Independently generated with Go integer arithmetic; source ns/terminal
  // labels were recorded separately, including the lossy 10000-hour pair.
  for(const [ms,want] of [[-9223372036854,'-9223372036s'],[-61000,'-61s'],[-999,'0s'],[0,'0s'],[59999,'59s'],[60000,'1m'],[3599999,'59m'],[3600000,'1h00m'],[35999999999,'9999h59m'],[9223372036854,'2562047h47m']]) assert.equal(durationText(ms),want);
});
