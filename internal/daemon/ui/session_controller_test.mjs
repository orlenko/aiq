import test, {afterEach} from 'node:test';
import assert from 'node:assert/strict';
import {createSessionController,navigatorReadCommand} from './session_controller.js';
import {ids,scope,limits,query,row,sessions,detail} from './controller_values.mjs';

// Supplied geometry/explicit event dispatch: synthetic ownership evidence only.
class Target {
  listeners=new Map();
  addEventListener(type,fn){if(!this.listeners.has(type))this.listeners.set(type,new Set());this.listeners.get(type).add(fn);}
  removeEventListener(type,fn){this.listeners.get(type)?.delete(fn);}
  emit(type,event={}){for(const fn of [...(this.listeners.get(type)||[])])fn({type,target:this,...event});}
  count(){return [...this.listeners.values()].reduce((sum,set)=>sum+set.size,0);}
}
class Element extends Target {
  constructor(document,tag){super();Object.assign(this,{ownerDocument:document,tagName:tag,children:[],parentNode:null,attrs:new Map(),className:'',value:'',clientWidth:100,scrollWidth:200});
    this.classList={add:(...names)=>{this.className=[...new Set([...this.className.split(' ').filter(Boolean),...names])].join(' ');}};}
  append(...nodes){for(const n of nodes){if(n.tagName==='#fragment'){this.append(...n.children);n.children=[];continue;}n.remove();n.parentNode=this;this.children.push(n);}}
  appendChild(n){this.append(n);return n;}
  replaceChildren(...nodes){for(const n of [...this.children])n.remove();this.value='';this.append(...nodes);}
  remove(){if(this.parentNode)this.parentNode.children=this.parentNode.children.filter(n=>n!==this);this.parentNode=null;}
  set textContent(v){assert.equal(typeof v,'string');this.replaceChildren();this.value=v;}
  get textContent(){return this.value+this.children.map(n=>n.textContent).join('');}
  setAttribute(n,v){this.attrs.set(n,String(v));}
  getAttribute(n){return this.attrs.get(n)??null;}
  removeAttribute(n){this.attrs.delete(n);}
  contains(n){return this===n||this.children.some(c=>c.contains(n));}
  get isConnected(){return this===this.ownerDocument.body||Boolean(this.parentNode?.isConnected);}
  getClientRects(){return this.isConnected?[{}]:[];}
  focus(){const doc=this.ownerDocument,old=doc.activeElement;if(old===this)return;doc.activeElement=this;old?.emit('focusout',{relatedTarget:this});this.emit('focusin',{relatedTarget:old});doc.focusCalls.push(this);}
  click(){this.emit('click');}
}
const walk=n=>[n,...n.children.flatMap(walk)];
const has=(n,name)=>n.className.split(' ').includes(name);
const find=(f,predicate)=>walk(f.document.body).filter(predicate);
const byText=(f,text)=>find(f,n=>n.tagName==='button'&&n.textContent===text)[0];
const tick=async()=>{for(let i=0;i<10;i++)await Promise.resolve();};
const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return {promise,resolve:data=>resolve({data}),reject};};
const fixtures=new Set();
afterEach(()=>{for(const f of fixtures)f.clean();});
function fixture(extra={}) {
  const window=new Target(),nodes=[],frames=new Map(),observers=[],aborts=[];let serial=0,observerHook=()=>{};
  const document={defaultView:window,activeElement:null,focusCalls:[],fonts:new Target(),createElement(tag){const n=new Element(document,tag);nodes.push(n);return n;},createDocumentFragment(){return new Element(document,'#fragment');}};
  document.body=new Element(document,'body');let fontsReady;document.fonts.ready=new Promise(r=>fontsReady=r);
  window.visualViewport=new Target();window.requestAnimationFrame=fn=>{const id=++serial;frames.set(id,fn);return id;};window.cancelAnimationFrame=id=>frames.delete(id);
  window.AbortController=class extends AbortController {constructor(){super();aborts.push(this);}};
  window.ResizeObserver=class {constructor(fn){this.callback=fn;this.disconnected=false;observers.push(this);observerHook('constructor',this);}observe(node){this.node=node;observerHook('observe',this,node);}disconnect(){this.disconnected=true;}};
  const listCalls=[],detailCalls=[],announcements=[],copies=[];
  const options={window,idPrefix:'controllerFixture',limits:limits(),detailQuery:{last:50,markup:false,anchor:0},
    readSessions:(q,signal)=>{const d=deferred();listCalls.push({q,signal,...d});return d.promise;},
    readConversation:(q,signal)=>{const d=deferred();detailCalls.push({q,signal,...d});return d.promise;},
    copyText:raw=>{copies.push(raw);return Promise.resolve();},announce:text=>announcements.push(text),...extra};
  const controller=createSessionController(document,options);
  const result={window,document,nodes,frames,observers,aborts,listCalls,detailCalls,announcements,copies,controller,options,
    hook:fn=>observerHook=fn,mount(){document.body.append(controller.fragment);assert.equal(controller.mount(),true);},
    finish(){fontsReady();for(const fn of [...frames.values()])fn();frames.clear();},
    clean(){controller.teardown();controller.teardown();fixtures.delete(result);assert.equal(window.count()+window.visualViewport.count()+document.fonts.count()+frames.size+nodes.reduce((n,node)=>n+node.count(),0),0);assert.ok(observers.every(o=>o.disconnected));}};
  fixtures.add(result);return result;
}
function rows(f){return find(f,n=>n.tagName==='button'&&!['Refresh sessions','Refresh conversation','Copy message'].includes(n.textContent));}
function painted(f){return find(f,n=>has(n,'nav-md-line')).map(n=>n.textContent);}

// No direct DTO is returned through a promise: even deliberately hostile DTOs
// are nested in the trusted inert envelope.
test('detached construction, retry, literal duplicate/unavailable rows and owned inputs',async()=>{
  const f=fixture();f.controller.loadSessions(query());f.controller.selectConversation(scope('A'));f.controller.refresh('all');
  assert.equal(f.listCalls.length+f.detailCalls.length,0);assert.equal(f.controller.mount(),false);f.mount();
  assert.deepEqual(find(f,n=>n.tagName==='h2').map(n=>n.textContent),['Sessions','Conversation']);
  assert.ok(byText(f,'Refresh sessions'));assert.ok(byText(f,'Refresh conversation'));
  f.options.limits.rows=0;f.options.detailQuery.last=1;f.options.readSessions=()=>assert.fail('caller mutated captured reader');
  const q={dir:'/fixture/A/',canonicalDir:'/fixture/A',workers:true};f.controller.loadSessions(q);q.dir='MUTATED';q.workers=false;
  assert.deepEqual(f.listCalls[0].q,{dir:'/fixture/A/',workers:true});
  const d=sessions('/fixture/A',[row(),row('A',{label:'duplicate A'}),row('B',{id:'unsupported-short',label:'unavailable B'})]);
  f.listCalls[0].resolve(d);await tick();d.sessions[0].label='MUTATED';
  const text=f.document.body.textContent;for(const literal of ['row A','duplicate A','unavailable B','3','7','2'])assert.ok(text.includes(literal));
  assert.ok(!text.includes('MUTATED'));assert.equal(rows(f).length,2,'duplicate selectable rows keep ordinal controls');
  assert.ok(text.toLowerCase().includes('unavailable'));f.clean();assert.equal(f.controller.mount(),false);
});

test('list scopes, workers, independent canonical identity and stale success/rejection',async()=>{
  const f=fixture();f.mount();f.controller.loadSessions(query());const a=f.listCalls[0];
  f.controller.loadSessions(query('/fixture/B'));assert.equal(a.signal.aborted,true);
  f.listCalls[1].resolve(sessions('/fixture/B',[row('B')]));await tick();const announced=f.announcements.length;a.resolve(sessions('/fixture/A'));await tick();assert.equal(f.announcements.length,announced);
  assert.ok(f.document.body.textContent.includes('row B'));assert.ok(!f.document.body.textContent.includes('row A'));
  f.controller.selectConversation(scope('B'));f.detailCalls[0].resolve(detail('B'));await tick();assert.deepEqual(painted(f),['paint B group 0']);
  f.controller.loadSessions(query('/fixture/B',true));assert.equal(painted(f).length,0);assert.equal(rows(f).length,0);
  f.listCalls[2].resolve(sessions('/fixture/A'));await tick();assert.equal(rows(f).length,0,'current wrong directory must fail whole');
  f.controller.loadSessions(query('/fixture/B',true));f.listCalls[3].resolve(sessions('/fixture/B',[row('B')]));await tick();
  f.controller.refresh('list');assert.equal(rows(f).length,1);assert.ok(/refresh|loading/i.test(f.document.body.textContent));
  f.listCalls[4].reject(new Error('PRIVATE backend detail'));await tick();assert.equal(rows(f).length,1);assert.ok(/stale|unavailable/i.test(f.document.body.textContent));assert.ok(!f.document.body.textContent.includes('PRIVATE'));
  f.controller.loadSessions(query('/fixture/C'));const stale=f.listCalls[5];f.controller.loadSessions(query('/fixture/A'));
  stale.reject(new Error('ignored aborted rejection'));f.listCalls[6].resolve(sessions());await tick();assert.ok(f.document.body.textContent.includes('row A'));f.clean();
});

test('detail A after B, immutable raw/header and same-ID manual reset versions',async()=>{
  const pendingCopy=deferred(),f=fixture({copyText:raw=>{f.copies.push(raw);return pendingCopy.promise;}});f.mount();
  f.controller.selectConversation(scope('A'));f.controller.selectConversation(scope('B'));assert.equal(f.detailCalls[0].signal.aborted,true);
  assert.deepEqual(f.detailCalls[1].q,{...scope('B'),last:50,markup:false,anchor:0});
  const b=detail('B');f.detailCalls[1].resolve(b);await tick();b.session.title='MUTATED';b.messages[0].raw='MUTATED';
  const announced=f.announcements.length;f.detailCalls[0].resolve(detail('A'));await tick();assert.equal(f.announcements.length,announced);assert.deepEqual(painted(f),['paint B group 0']);assert.ok(f.document.body.textContent.includes('detail title B'));assert.ok(!f.document.body.textContent.includes('row title B'));
  const copy=byText(f,'Copy message');copy.click();assert.deepEqual(f.copies,['raw B\n\nhidden **copy**']);
  f.controller.refresh('detail');assert.equal(painted(f).length,0);const old=f.detailCalls[2];f.controller.refresh('detail');
  old.resolve(detail('B'));pendingCopy.resolve(undefined);await tick();assert.equal(painted(f).length,0);assert.ok(!f.announcements.includes('copied'));
  f.detailCalls[3].resolve(detail('B'));await tick();copy.click();assert.equal(f.copies.length,1,'old copy control was released');assert.equal(f.document.focusCalls.length,0);f.clean();
});

test('capture is all-or-nothing before feed allocation; inert envelope ignores DTO methods',async()=>{
  const f=fixture();f.mount();let calls=0;
  const d=detail();for(const name of ['then','toJSON'])Object.defineProperty(d,name,{get(){calls++;throw new Error('unused');}});
  Object.defineProperty(d.messages,Symbol.iterator,{get(){calls++;throw new Error('iterator');}});
  f.controller.selectConversation(scope('A'));f.detailCalls[0].resolve(d);await tick();assert.equal(calls,0);assert.deepEqual(painted(f),['paint A group 0']);
  for(const mutate of [d=>d.session.id=ids.B,d=>d.reset=false,d=>d.session.title=0,d=>d.messages[0].groups[0].lines[0].spans[0].c=['unknown'],
    d=>Object.defineProperty(d.messages[0],'raw',{get(){calls++;throw new Error('consumed');}})]) {
    f.controller.selectConversation(scope('A'));const before=f.nodes.length,bad=detail();mutate(bad);f.detailCalls.at(-1).resolve(bad);await tick();
    assert.equal(painted(f).length,0);assert.equal(f.nodes.length,before,'no partial feed allocation on invalid capture');assert.ok(/unavailable/i.test(f.document.body.textContent));
  }
  assert.equal(calls,0);f.clean();
});

test('double settlement, direct result refusal, reentrant reader throw and stale thenable rejection',async()=>{
  let controller,mode='double',thenCalls=0;
  const f=fixture({readConversation:q=>{
    if(mode==='direct')return detail('A');
    if(mode==='reenter'){mode='double';controller.selectConversation(scope('B'));throw new Error('PRIVATE stale throw');}
    return {then(resolve,reject){thenCalls++;resolve({data:detail(q.id===ids.B?'B':'A')});reject(new Error('second'));resolve({data:detail('C')});}};
  }});controller=f.controller;f.mount();controller.selectConversation(scope('A'));await tick();assert.deepEqual(painted(f),['paint A group 0']);assert.equal(thenCalls,1);
  mode='direct';controller.selectConversation(scope('A'));await tick();assert.equal(painted(f).length,0);assert.ok(/unavailable/i.test(f.document.body.textContent));
  mode='reenter';controller.selectConversation(scope('A'));await tick();assert.deepEqual(painted(f),['paint B group 0']);assert.ok(!f.document.body.textContent.includes('PRIVATE'));
  // Reentry changes the next reader before its thenable is assimilated.
  let once=true;
  // A separate controller uses a one-shot stale rejecting thenable.
  f.clean();const g=fixture({readConversation:q=>{if(once){once=false;return {then(resolve,reject){g.controller.selectConversation(scope('B'));reject(new Error('late rejected'));}};}return Promise.resolve({data:detail('B')});}});
  g.mount();g.controller.selectConversation(scope('A'));await tick();assert.deepEqual(painted(g),['paint B group 0']);g.clean();
});

test('abort reentry publishes B before obsolete C and C reader is never invoked',async()=>{
  const f=fixture();f.mount();f.controller.selectConversation(scope('A'));
  f.detailCalls[0].signal.addEventListener('abort',()=>f.controller.selectConversation(scope('B')),{once:true});
  f.controller.selectConversation(scope('C'));assert.deepEqual(f.detailCalls.map(call=>call.q.id),[ids.A,ids.B]);
  assert.equal(f.detailCalls[1].signal.aborted,false);f.detailCalls[1].resolve(detail('B'));f.detailCalls[0].resolve(detail('A'));await tick();assert.deepEqual(painted(f),['paint B group 0']);f.clean();
});

test('manual focus is revoked by leave-return and row replacement; announce reentry cannot focus A',async()=>{
  for(const mode of ['still','leave-return','replace','announce']) {
    let armed=false;const f=fixture({announce:text=>{f.announcements.push(text);if(mode==='announce'&&armed&&painted(f).includes('paint A group 0')){armed=false;f.controller.selectConversation(scope('B'));}}});f.mount();
    const neighbor=f.document.createElement('button');neighbor.textContent='neighbor';f.document.body.append(neighbor);
    f.controller.loadSessions(query());f.listCalls[0].resolve(sessions());await tick();
    const button=rows(f)[0];button.focus();button.click();const before=f.document.focusCalls.length;armed=true;
    if(mode==='leave-return'){neighbor.focus();button.focus();}
    if(mode==='replace'){f.controller.refresh('list');f.listCalls[1].resolve(sessions());await tick();}
    const expectedBefore=f.document.focusCalls.length;f.detailCalls[0].resolve(detail());await tick();
    if(mode==='still'){
      assert.equal(f.document.focusCalls.length,before+1);assert.equal(f.document.activeElement.tagName,'h2');assert.equal(f.document.activeElement.getAttribute('tabindex'),'-1');
    } else assert.equal(f.document.focusCalls.length,expectedBefore,'revoked/stale focus intent cannot be renewed');
    if(mode==='announce'){assert.equal(f.detailCalls.length,2);f.detailCalls[1].resolve(detail('B'));await tick();assert.deepEqual(painted(f),['paint B group 0']);}
    f.clean();assert.equal(neighbor.isConnected,true);
  }
});

test('actual feed observer activation reentry retires A after unwind and leaves B resources intact',async()=>{
  for(const stage of ['constructor','observe'])for(const action of ['B','dispose'])for(const ending of ['true','false','throw']) {
    const f=fixture();f.mount();const neighbor=f.document.createElement('button');f.document.body.append(neighbor);neighbor.focus();
    let reentered=false,firstObserver;
    f.hook((point,observer)=>{
      if(!reentered&&point===stage){reentered=true;firstObserver=observer;if(action==='B')f.controller.selectConversation(scope('B'));else f.controller.teardown();}
      if(ending==='throw'&&reentered&&point==='observe'&&observer===firstObserver)throw new Error('invented activation fault');
    });
    f.controller.selectConversation(scope('A'));f.detailCalls[0].resolve(detail('A',ending==='false'?2:1));await tick();
    assert.equal(reentered,true,`${stage}/${action}/${ending} reached actual helper callout`);
    assert.equal(painted(f).length,0);assert.equal(f.document.activeElement,neighbor);assert.ok(!f.announcements.some(text=>text==='Conversation ready.'));
    if(action==='B'){
      assert.equal(f.detailCalls.length,2);assert.equal(f.detailCalls[1].signal.aborted,false);
      f.detailCalls[1].resolve(detail('B'));await tick();assert.deepEqual(painted(f),['paint B group 0']);
      assert.equal(f.observers.filter(o=>!o.disconnected).length,1,'only B observer survives');
      byText(f,'Copy message').click();await tick();assert.deepEqual(f.copies,['raw B\n\nhidden **copy**']);
    } else assert.equal(f.controller.mount(),false);
    assert.ok(f.observers.filter(o=>o!==f.observers.at(-1)||action==='dispose').every(o=>o.disconnected));
    assert.equal(f.nodes.filter(n=>!n.isConnected).reduce((n,node)=>n+node.count(),0),0,'no retired A listeners');
    f.finish();f.clean();assert.equal(neighbor.isConnected,true);assert.equal(f.document.activeElement,neighbor);
  }
});

test('scope change and terminal teardown invalidate both pending channels and pending copy',async()=>{
  const copy=deferred(),f=fixture({copyText:raw=>{f.copies.push(raw);return copy.promise;}});f.mount();
  f.controller.loadSessions(query());f.listCalls[0].resolve(sessions());await tick();
  f.controller.selectConversation(scope('A'));f.detailCalls[0].resolve(detail());await tick();byText(f,'Copy message').click();
  f.controller.refresh('all');const list=f.listCalls[1],conversation=f.detailCalls[1];
  f.controller.loadSessions(query('/fixture/B'));assert.equal(conversation.signal.aborted,true);assert.equal(list.signal.aborted,true);
  const staleCallbacks=[...f.frames.values()];f.controller.teardown();copy.resolve();
  for(const call of [list,conversation,f.listCalls[2]])call.reject(new Error('PRIVATE stale work'));
  for(const fn of staleCallbacks)fn();for(const observer of f.observers)observer.callback();f.finish();await tick();
  assert.ok(!f.announcements.includes('copied'));assert.equal(find(f,n=>has(n,'nav-feed')).length,0);
  const total=f.listCalls.length+f.detailCalls.length;f.controller.loadSessions(query());f.controller.selectConversation(scope('B'));f.controller.refresh('all');
  assert.equal(f.listCalls.length+f.detailCalls.length,total);f.clean();
});

test('refresh all cannot overwrite reader-reentrant selection of the identical provider/id',async()=>{
  let reenter=false;
  const f=fixture({readSessions:(q,signal)=>{
    const d=deferred();f.listCalls.push({q,signal,...d});
    if(reenter){reenter=false;f.controller.selectConversation(scope('A'));}
    return d.promise;
  }});f.mount();f.controller.loadSessions(query());f.listCalls[0].resolve(sessions());await tick();
  f.controller.selectConversation(scope('A'));f.detailCalls[0].resolve(detail());await tick();
  reenter=true;f.controller.refresh('all');
  assert.equal(f.detailCalls.length,2,'new same-ID selection record supersedes snapshotted refresh intent');
  assert.equal(f.detailCalls[1].signal.aborted,false,'old refresh must not abort the reentrant selection');
  f.listCalls[1].resolve(sessions());f.detailCalls[1].resolve(detail());await tick();assert.deepEqual(painted(f),['paint A group 0']);f.clean();
});


test('cross-channel retirement detaches old detail before any abort callout',async()=>{
  const observations=[];
  for(const phase of ['ready','activating'])for(const mode of ['directory','workers','dispose']) {
    let entered=false,oldRoot,oldCopy;
    const trace=[];
    const capture=label=>trace.push([label,oldRoot.isConnected,oldCopy.isConnected,painted(f).length,f.document.body.textContent.includes('detail title A')]);
    const f=fixture({readConversation:(q,signal)=>{
      if(entered)capture('reader');
      const pending=deferred();f.detailCalls.push({q,signal,...pending});return pending.promise;
    }});
    f.mount();f.controller.loadSessions(query());f.listCalls[0].resolve(sessions());await tick();
    f.controller.refresh('list');const list=f.listCalls[1];
    list.signal.addEventListener('abort',()=>{capture('abort');entered=true;f.controller.selectConversation(scope('A'));},{once:true});
    const change=()=>{oldRoot=find(f,n=>has(n,'nav-controller-conversation'))[0];oldCopy=byText(f,'Copy message');
      if(mode==='dispose')f.controller.teardown();else f.controller.loadSessions(mode==='directory'?query('/fixture/B'):query('/fixture/A',true));};
    let once=true;
    if(phase==='activating')f.hook(point=>{if(once&&point==='constructor'){once=false;change();}});
    f.controller.selectConversation(scope('A'));f.detailCalls[0].resolve(detail('A'));await tick();
    if(phase==='ready')change();
    if(mode!=='dispose') {
      assert.equal(f.detailCalls.length,2);assert.equal(f.detailCalls[1].signal.aborted,false);
      assert.equal(f.detailCalls[1].q.id,ids.A,'same UUID is a fresh operation');
      const fresh=detail('A');fresh.session.title='fresh header';fresh.messages[0].raw='fresh raw';fresh.messages[0].groups[0].lines[0].spans[0].t='fresh paint';
      f.detailCalls[1].resolve(fresh);await tick();assert.deepEqual(painted(f),['fresh paint']);assert.ok(f.document.body.textContent.includes('fresh header'));
      byText(f,'Copy message').click();await tick();assert.deepEqual(f.copies,['fresh raw']);
    } else assert.equal(f.detailCalls.length,1,'terminal API admits no reentrant reader');
    observations.push({phase,mode,trace});f.finish();f.clean();
  }
  assert.deepEqual(observations, ['ready','activating'].flatMap(phase=>['directory','workers','dispose'].map(mode=>({phase,mode,
    trace:mode==='dispose'?[['abort',false,false,0,false]]:[['abort',false,false,0,false],['reader',false,false,0,false]]}))));
});

test('explicit all remains owned and closed without admitting numeric zero or an anchor',()=>{
 const f=fixture({detailQuery:{last:'all',markup:false,anchor:0}});f.mount();f.controller.selectConversation(scope('A'));assert.equal(f.detailCalls[0].q.last,'all');f.clean();
 for(const last of [0,'0','ALL',null])assert.throws(()=>fixture({detailQuery:{last,markup:false,anchor:0}}));
 assert.throws(()=>fixture({detailQuery:{last:'all',markup:false,anchor:1}}));
});

test('read command templates accept only complete IDs and supported follow providers',()=>{
  for(const provider of ['claude','codex','agy']){
    assert.equal(navigatorReadCommand(provider,ids.A),`aiq convo ${ids.A}`);
    assert.equal(navigatorReadCommand(provider,ids.A,true),provider==='agy'?null:`aiq convo ${ids.A} --follow`);
  }
  for(const id of ['','short',ids.A+'; echo invented',ids.A+'\n',ids.A+' ',null,5,{}])assert.equal(navigatorReadCommand('claude',id),null);
  for(const provider of ['copilot','unknown','claude;echo',null])assert.equal(navigatorReadCommand(provider,ids.A),null);
  assert.equal(navigatorReadCommand('claude',ids.A,'--follow'),null);
});

test('read chips copy fixed text only after activation and omit unsupported IDs and agy follow',async()=>{
  const f=fixture({readCommands:true});f.mount();f.controller.loadSessions(query());
  const values=[row('A',{provider:'claude'}),{...row('B'),provider:'agy',cwd:'/invented/$(text)',commands:[{verb:'untrusted command'}]}, {...row('C'),id:'short'}];
  f.listCalls[0].resolve(sessions('/fixture/A',values));await tick();assert.deepEqual(f.copies,[]);
  const buttons=find(f,n=>has(n,'nav-command-copy'));assert.equal(buttons.length,3);
  for(const button of buttons){button.click();await tick();}
  assert.deepEqual(f.copies,[`aiq convo ${ids.A}`,`aiq convo ${ids.A} --follow`,`aiq convo ${ids.B}`]);
  assert.ok(!f.copies.some(s=>s.includes('invented')||s.includes('untrusted')));f.clean();
});

test('read chips suppress duplicate activation and retire pending copy completions',async()=>{
  let settle;const pending=new Promise(r=>settle=r),f=fixture({readCommands:true,copyText:raw=>{f.copies.push(raw);return pending;}});
  f.mount();f.controller.selectConversation({...scope('A'),provider:'claude'});const d=detail();d.session.provider='claude';f.detailCalls[0].resolve(d);await tick();
  const old=byText(f,'Copy read command'),follow=byText(f,'Copy follow command');old.click();old.click();follow.click();
  assert.deepEqual(f.copies,[`aiq convo ${ids.A}`]);assert.equal(old.getAttribute('aria-disabled'),'true');
  f.controller.selectConversation(scope('B'));const before=f.announcements.length;old.click();settle();await tick();
  assert.equal(f.announcements.length,before);assert.equal(f.copies.length,1);
  f.detailCalls[1].resolve(detail('B'));await tick();assert.equal(byText(f,'Copy read command').getAttribute('aria-disabled'),null);f.clean();
});

test('read chip rejection is generic and preserves retry without stale controls',async()=>{
  let fail=true;const f=fixture({readCommands:true,copyText:raw=>{f.copies.push(raw);if(fail)throw new Error('PRIVATE clipboard detail');return Promise.resolve();}});
  f.mount();f.controller.selectConversation(scope('A'));f.detailCalls[0].resolve(detail());await tick();
  const button=byText(f,'Copy read command');button.click();await tick();assert.equal(button.getAttribute('aria-disabled'),null);
  assert.ok(f.announcements.includes('Read command could not be copied.'));assert.ok(!f.document.body.textContent.includes('PRIVATE'));
  fail=false;button.click();await tick();assert.ok(f.announcements.includes('Read command copied. Paste it in a terminal.'));
  f.clean();button.click();assert.equal(f.copies.length,2);
});
