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
  emit(n,event={}) { for(const f of [...(this.listeners.get(n)||[])]) f(event); }
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
  insertBefore(n,before){n.remove();const at=before===null?this.children.length:this.children.indexOf(before);assert(at>=0);n.parentNode=this;this.children.splice(at,0,n);return n;}
  remove() { if(this.parentNode) this.parentNode.children=this.parentNode.children.filter(n=>n!==this);this.parentNode=null; }
  set textContent(v) {assert.equal(typeof v,'string');this.value=v;this.children=[];}
  get textContent() {return this.value+this.children.map(n=>n.textContent).join('');}
  setAttribute(n,v) {this.attrs.set(n,v);}
  removeAttribute(n) {this.attrs.delete(n);}
  getAttribute(n) {return this.attrs.get(n)??null;}
  contains(n) {return this===n||this.children.some(c=>c.contains(n));}
  focus() {this.ownerDocument.activeElement=this;for(let n=this;n;n=n.parentNode)n.emit('focusin',{target:this});this.onFocus?.();}
  scrollIntoView() {this.scrolled=(this.scrolled??0)+1;this.onScroll?.();}
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

test('incremental snapshots retain unchanged message DOM and replace changed copy facts',async()=>{
  const f=fixture(),one=fixtureMessage(1,[fixtureGroup(0,fixtureHead('prompt'))]);
  const feed=mounted(f,fixtureWindow([one]),{readingControls:true});
  const first=all(f.document.body).find(n=>n.tagName==='article');
  const two=fixtureMessage(2,[fixtureGroup(1,fixtureHead('reply'),[fixtureLine('new answer')])]);
  assert.equal(feed.update(fixtureWindow([one,two])),true);
  assert.equal(all(f.document.body).find(n=>n.tagName==='article'),first);
  const second=all(f.document.body).filter(n=>n.tagName==='article')[1];
  const changed=structuredClone(two);changed.raw='changed raw';changed.groups[0].lines=[fixtureLine('changed answer')];
  assert.equal(feed.update(fixtureWindow([one,changed])),true);
  assert.equal(all(f.document.body).find(n=>n.tagName==='article'),first);
  assert(!second.isConnected);assert.equal(second.count()+all(second).slice(1).reduce((sum,n)=>sum+n.count(),0),0);
  feed.teardown();assert.equal(f.document.body.children.length,0);assert(f.observers.every(o=>o.disconnected));
});
test('focused or copying feeds retain the snapshot and invalid updates leave it usable',async()=>{
  const f=fixture(),feed=mounted(f,fixtureWindow(),{readingControls:true});
  const first=all(f.document.body).find(n=>n.tagName==='article'),copy=buttons(first).find(n=>has(n,'nav-feed-copy'));
  copy.focus();assert.equal(feed.update(fixtureWindow([])),false);assert(first.isConnected);
  f.document.activeElement=null;
  const bad=fixtureWindow();bad.session.id='00000002-0000-4000-8000-000000000002';assert.throws(()=>feed.update(bad));assert(first.isConnected);
  assert.equal(feed.update(fixtureWindow([])),true);assert(!first.isConnected);
  feed.teardown();assert(f.observers.every(o=>o.disconnected));
});

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

function readingWindow() {
  const prompt=(n,ordinal,who)=>({...fixtureMessage(n,[fixtureGroup(ordinal,{...fixtureHead('prompt'),who})]),turn:n});
  const answer={...fixtureMessage(2,[fixtureGroup(1,{...fixtureHead('final'),duration_known:true,duration_ms:125000,tools:3})]),turn:1,kind:'answer',sub:'final',source:'unknown',source_known:false};
  return fixtureWindow([prompt(1,0,'Prompt A'),answer,prompt(3,2,'Prompt B'),prompt(4,3,'Prompt C')]);
}
const byClass=(f,name)=>all(f.document.body).filter(n=>has(n,name));
function reading(f,more={}) {return mounted(f,readingWindow(),{readingControls:true,...more});}
function key(root,target,key,more={}) {const e={target,key,defaultPrevented:false,preventDefault(){this.defaultPrevented=true;},...more};root.emit('keydown',e);return e;}

test('reading controls are opt-in, validate before allocation, and keep owned ordered prompt facts',()=>{
  for(const value of [null,0,'true',undefined]){const f=fixture();assert.throws(()=>f.feed(readingWindow(),{readingControls:value}));assert.equal(f.count,0);}
  let calls=0;const f=fixture(),options={};Object.defineProperty(options,'readingControls',{get(){calls++;return true;}});
  const makeOptions={window:f.window,idPrefix:'fixture',expectedScope:fixtureScope,copyText:()=>{},announce:()=>{}};
  Object.defineProperty(makeOptions,'readingControls',Object.getOwnPropertyDescriptor(options,'readingControls'));
  assert.throws(()=>createConversationFeed(f.document,readingWindow(),makeOptions));assert.equal(calls,0);assert.equal(f.count,0);
  const data=readingWindow(),g=fixture(),feed=mounted(g,data,{readingControls:true});data.messages[0].groups[0].head.who='mutated';
  assert.deepEqual(byClass(g,'nav-feed-outline-prompt').map(n=>n.textContent),['Prompt A','Prompt B','Prompt C']);
  assert.equal(byClass(g,'nav-feed-outline-facts')[0].textContent,'2m · 3 tools');
  assert.equal(g.document.activeElement,null);feed.refresh();assert.equal(g.document.activeElement,null);feed.teardown();
});

test('explicit reading navigation preserves answer position when focus moves onto controls',()=>{
  const f=fixture(),feed=reading(f),heads=byClass(f,'nav-feed-prompt'),copies=byClass(f,'nav-feed-copy'),next=byClass(f,'nav-feed-next')[0],previous=byClass(f,'nav-feed-previous')[0];
  copies[1].focus();next.focus();next.emit('click');assert.equal(f.document.activeElement,heads[1]);
  next.focus();next.emit('click');assert.equal(f.document.activeElement,heads[2]);
  previous.focus();previous.emit('click');assert.equal(f.document.activeElement,heads[1]);
  assert.equal(heads[1].scrolled,2);assert.equal(heads[1].tabIndex,-1);feed.teardown();
});

test('initial reading controls have defined edges and outline targets never mutate history',()=>{
  for(const direction of ['previous','next']){
    const f=fixture(),announcements=[],feed=reading(f,{announce:t=>announcements.push(t)}),heads=byClass(f,'nav-feed-prompt'),control=byClass(f,'nav-feed-'+direction)[0];
    control.focus();control.emit('click');const expected=direction==='next'?heads[0]:heads[2];assert.equal(f.document.activeElement,expected);
    control.focus();const edge=direction==='next'?byClass(f,'nav-feed-previous')[0]:byClass(f,'nav-feed-next')[0];edge.emit('click');assert.equal(f.document.activeElement,control);assert.match(announcements.at(-1),/First|Last/);
    const pick=byClass(f,'nav-feed-outline-prompt')[1];pick.focus();pick.emit('click');assert.equal(f.document.activeElement,heads[1]);assert.equal(pick.getAttribute('aria-controls'),heads[1].id);feed.teardown();
  }
});

test('n and p respect global toggle, modifiers, editable targets and native Tab',()=>{
  const f=fixture();let enabled=true;const feed=reading(f,{shortcutsEnabled:()=>enabled}),root=byClass(f,'nav-feed')[0],copies=byClass(f,'nav-feed-copy'),heads=byClass(f,'nav-feed-prompt');copies[1].focus();
  for(const more of [{ctrlKey:true},{altKey:true},{metaKey:true},{shiftKey:true},{defaultPrevented:true}]){const e=key(root,copies[1],'n',more);assert.equal(e.defaultPrevented,Boolean(more.defaultPrevented));assert.equal(f.document.activeElement,copies[1]);}
  for(const target of [f.document.createElement('input'),f.document.createElement('textarea'),f.document.createElement('select'),Object.assign(f.document.createElement('div'),{isContentEditable:true})]){root.append(target);assert.equal(key(root,target,'n').defaultPrevented,false);}
  assert.equal(key(root,f.document.createElement('button'),'n').defaultPrevented,false);
  assert.equal(key(root,copies[1],'Tab').defaultPrevented,false);
  enabled=false;assert.equal(key(root,copies[1],'n').defaultPrevented,false);assert.equal(f.document.activeElement,copies[1]);
  enabled=true;assert.equal(key(root,copies[1],'n').defaultPrevented,true);assert.equal(f.document.activeElement,heads[1]);
  assert.equal(key(root,heads[1],'p').defaultPrevented,true);assert.equal(f.document.activeElement,heads[0]);feed.teardown();
});

test('retired reading controls and reentrant shortcut or focus callouts cannot move focus again',()=>{
  for(const mode of ['shortcut','focus','scroll']){
    const f=fixture();let feed,called=0;feed=reading(f,{shortcutsEnabled:()=>{if(mode==='shortcut')feed.teardown();return true;},announce:()=>called++});
    const root=byClass(f,'nav-feed')[0],head=byClass(f,'nav-feed-prompt')[0],next=byClass(f,'nav-feed-next')[0];
    if(mode==='focus')head.onFocus=()=>feed.teardown();if(mode==='scroll')head.onScroll=()=>feed.teardown();
    if(mode==='shortcut')key(root,next,'n');else next.emit('click');
    assert.equal(called,0);assert.equal(head.scrolled??0,mode==='scroll'?1:0);assert.equal(f.document.body.children.length,0);
    const before=f.document.activeElement;next.emit('click');key(root,next,'n');assert.equal(f.document.activeElement,before);assert.equal(feed.mount(),false);
  }
});

test('multiple human headings in one message navigate independently and ignore invisible headings',()=>{
  const f=fixture(),data=fixtureWindow([fixtureMessage(1,[fixtureGroup(0,{...fixtureHead('prompt'),who:'One'}),fixtureGroup(1,{...fixtureHead('steer'),who:'Two'}),{...fixtureGroup(2,fixtureHead('prompt')),visible:false}])]),feed=mounted(f,data,{readingControls:true});
  const heads=byClass(f,'nav-feed-prompt'),next=byClass(f,'nav-feed-next')[0];next.emit('click');assert.equal(f.document.activeElement,heads[0]);next.emit('click');assert.equal(f.document.activeElement,heads[1]);assert.equal(byClass(f,'nav-feed-outline-prompt').length,2);feed.teardown();
});

test('reading position and listeners belong to each mounted feed',()=>{
  const f=fixture(),a=reading(f),first=byClass(f,'nav-feed-next')[0],firstHeads=byClass(f,'nav-feed-prompt');
  const b=reading(f),second=byClass(f,'nav-feed-next')[1],allHeads=byClass(f,'nav-feed-prompt');
  first.emit('click');assert.equal(f.document.activeElement,firstHeads[0]);second.emit('click');assert.equal(f.document.activeElement,allHeads[3]);a.teardown();second.emit('click');assert.equal(f.document.activeElement,allHeads[4]);b.teardown();
});
