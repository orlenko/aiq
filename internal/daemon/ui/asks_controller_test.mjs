import test,{afterEach} from 'node:test';
import assert from 'node:assert/strict';
import {createAsksController} from './asks_controller.js';
import {snapshot,scope,limits,firstCopy,sourceID} from './asks_values.mjs';

// Synthetic lifecycle and callout controls; these rectangles are NOT browser proof.
class Target {
  listeners=new Map();
  addEventListener(k,f){if(!this.listeners.has(k))this.listeners.set(k,new Set());this.listeners.get(k).add(f);}
  removeEventListener(k,f){this.listeners.get(k)?.delete(f);}
  emit(k){for(const f of [...(this.listeners.get(k)||[])])f();}
  count(){return [...this.listeners.values()].reduce((n,s)=>n+s.size,0);}
}
class Element extends Target {
  constructor(document,tag){super();Object.assign(this,{ownerDocument:document,tagName:tag,children:[],parentNode:null,attrs:new Map(),className:'',value:'',hidden:false,clientWidth:100,scrollWidth:300});
    this.classList={add:c=>{this.className+=' '+c;}};}
  append(...nodes){for(const n of nodes){if(n.tagName==='#fragment'){this.append(...n.children);continue;}n.remove();n.parentNode=this;this.children.push(n);}}
  replaceChildren(...nodes){for(const n of [...this.children])n.remove();this.value='';this.append(...nodes);}
  remove(){if(this.parentNode)this.parentNode.children=this.parentNode.children.filter(n=>n!==this);this.parentNode=null;}
  set textContent(t){assert.equal(typeof t,'string');this.replaceChildren();this.value=t;this.ownerDocument.domHook(this,t);}
  get textContent(){return this.value+this.children.map(n=>n.textContent).join('');}
  setAttribute(k,v){this.attrs.set(k,String(v));}
  getAttribute(k){return this.attrs.get(k)??null;}
  removeAttribute(k){this.attrs.delete(k);}
  contains(n){return this===n||this.children.some(c=>c.contains(n));}
  get isConnected(){if(this.className==='nav-md'&&this.ownerDocument.refuseLines)return false;return this===this.ownerDocument.body||Boolean(this.parentNode?.isConnected);}
  getClientRects(){return this.isConnected&&visible(this)?[{}]:[];}
  click(){this.emit('click');}
  focus(){this.ownerDocument.activeElement=this;this.ownerDocument.focusCalls++;this.ownerDocument.focusHook(this);}
}
const walk=n=>[n,...n.children.flatMap(walk)];
const visible=n=>{for(;n;n=n.parentNode)if(n.hidden)return false;return true;};
const tick=async()=>{for(let i=0;i<20;i++)await Promise.resolve();};
const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return{promise,resolve,reject};};
const all=()=>({mode:'all',dir:''});
const directory=dir=>({mode:'directory',dir});
const dto=s=>Object.assign(snapshot(),{scope:{...s}});
const fixtures=new Set();
afterEach(()=>{for(const f of [...fixtures])f.clean();});
function fixture(extra={}) {
  const window=new Target(),nodes=[],frames=new Map(),observers=[],calls=[],copies=[],announcements=[],navigations=[];
  let hook=()=>{},schedule=()=>{},serial=0;
  const document={defaultView:window,activeElement:null,focusCalls:0,domHook:()=>{},focusHook:()=>{},fonts:new Target(),
    createElement(tag){const n=new Element(document,tag);nodes.push(n);return n;},createDocumentFragment(){return new Element(document,'#fragment');}};
  document.body=new Element(document,'body');document.activeElement=document.body;
  let fontsReady;document.fonts.ready=new Promise(r=>fontsReady=r);
  window.AbortController=AbortController;window.visualViewport=new Target();
  window.requestAnimationFrame=fn=>{schedule();const id=++serial;frames.set(id,fn);return id;};window.cancelAnimationFrame=id=>frames.delete(id);
  window.ResizeObserver=class {constructor(fn){this.callback=fn;this.disconnected=false;this.disconnects=0;observers.push(this);hook('constructor',this);}observe(n){hook('observe',this,n);}disconnect(){this.disconnects++;this.disconnected=true;hook('disconnect',this);}};
  const options={window,idPrefix:'asksHost',limits:limits(),readAsks:(s,signal)=>{const d=deferred();calls.push({s,signal,...d});return d.promise;},
    copyText:text=>{copies.push(text);},announce:text=>announcements.push(text),onConversation:value=>navigations.push(value),...extra};
  const view=createAsksController(document,options);
  const f={view,options,document,window,nodes,frames,observers,calls,copies,announcements,navigations,
    find:c=>walk(document.body).filter(n=>n.className.split(' ').includes(c)),
    click:(text,index=0)=>walk(document.body).filter(n=>n.tagName==='button'&&n.textContent===text)[index].click(),
    hook:fn=>hook=fn,schedule:fn=>schedule=fn,
    mount(){document.body.append(view.fragment);assert.equal(view.mount(),true);},
    async load(s=scope()){assert.equal(view.loadScope(s),true);await tick();},
    async resolve(index=0,data=dto(calls[index].s)){calls[index].resolve({data});await tick();},
    flush(){const pending=[...frames.values()];frames.clear();for(const fn of pending)fn();},
    clean(){document.domHook=()=>{};document.focusHook=()=>{};hook=()=>{};schedule=()=>{};view.teardown();view.teardown();fixtures.delete(f);
      assert.equal(window.count()+window.visualViewport.count()+document.fonts.count()+frames.size+nodes.reduce((n,x)=>n+x.count(),0),0);
      assert.ok(observers.every(o=>o.disconnected));fontsReady();}};
  fixtures.add(f);return f;
}
function status(f,text){assert.equal(f.find('nav-asks-load-status')[0].textContent,text);}
function noView(f){assert.equal(f.find('nav-asks').length,0);}

test('detached mount is readless and retryable; no-scope Refresh usable; mounted idempotent',async()=>{
  const f=fixture();assert.equal(f.view.mount(),false);assert.equal(f.view.loadScope(scope()),false);assert.equal(f.view.refresh(),false);
  f.mount();assert.equal(f.view.mount(),true);f.click('Refresh Asks');await tick();status(f,'Choose a scope for Asks');assert.equal(f.calls.length,0);
  assert.equal(f.find('nav-asks-content')[0].getAttribute('aria-label'),'Asks results');
  assert.equal(f.find('nav-asks-content')[0].contains(f.find('nav-asks-load-status')[0]),false);
  await f.load();status(f,'Loading Asks');assert.equal(f.find('nav-asks-content')[0].getAttribute('aria-busy'),'true');await f.resolve();
  status(f,'Asks updated');assert.equal(f.find('nav-asks-heading').length,1);assert.equal(f.document.focusCalls,0);
  assert.equal(f.find('nav-asks-content')[0].getAttribute('aria-busy'),'false');assert.ok(f.find('nav-asks-candidate').every(n=>!n.hidden));
});

test('physical read remains occupied despite abort; coalesces B to C including same-scope intents',async()=>{
  const f=fixture();f.mount();await f.load();const old=f.calls[0];await f.load(directory('/invented/B'));await f.load(all());
  assert.equal(old.signal.aborted,true);assert.equal(f.calls.length,1);noView(f);
  await f.resolve(0);assert.equal(f.calls.length,2);assert.deepEqual(f.calls[1].s,all());noView(f);await f.resolve(1);
  await f.load(all());assert.equal(f.calls.length,3);noView(f);await f.resolve(2);status(f,'Asks updated');
});

test('stale outer rejection consumed once and latest only drains; invalid latest forbids drain',async()=>{
  const f=fixture();f.mount();await f.load();await f.load(all());f.calls[0].reject(Error('invented'));await tick();
  assert.equal(f.calls.length,2);status(f,'Loading Asks');
  f.view.loadScope({mode:'directory',dir:''});f.calls[1].reject(Error('invented'));await tick();
  assert.equal(f.calls.length,2);status(f,'Asks unavailable');noView(f);f.click('Refresh Asks');await tick();status(f,'Choose a scope for Asks');
});

test('never settling reader retains one bounded flight and latest slot through repeated refresh',async()=>{
  const f=fixture();f.mount();await f.load();for(let i=0;i<100;i++)f.view.loadScope(directory('/invented/'+i));
  f.view.refresh();await tick();assert.equal(f.calls.length,1);assert.equal(f.calls[0].signal.aborted,true);noView(f);
  await f.resolve();assert.equal(f.calls.length,2);assert.equal(f.calls[1].s.dir,'/invented/99');
});

test('private scope/limits/hooks survive caller, reader-request and configuration mutation',async()=>{
  const f=fixture(),s=scope();f.mount();await f.load(s);s.dir='/invented/wrong';f.calls[0].s.dir='/invented/wrong';
  f.options.limits.rows=1;f.options.limits.stringUnits=1;f.options.readAsks=()=>assert.fail('mutated reader');
  await f.resolve(0,dto(scope()));status(f,'Asks updated');f.view.refresh();await tick();assert.deepEqual(f.calls[1].s,scope());
  await f.resolve(1,dto(directory('/invented/wrong')));status(f,'Asks unavailable');noView(f);
  f.view.refresh();await tick();assert.deepEqual(f.calls[2].s,scope());await f.resolve(2,dto(scope()));status(f,'Asks updated');
});

test('scope is exact for trailing slash/all/directory and invalid forms never read',async()=>{
  const f=fixture();f.mount();
  for(const s of [{mode:'all',dir:'x'},{mode:'directory',dir:''},{mode:'else',dir:''},{mode:'all'},{mode:'directory',dir:'x'.repeat(50001)}]){
    assert.equal(f.view.loadScope(s),false);await tick();status(f,'Asks unavailable');
  }
  assert.equal(f.calls.length,0);await f.load(directory('/invented/project/'));await f.resolve(0,dto(scope()));noView(f);
  f.view.refresh();await tick();assert.equal(f.calls[1].s.dir,'/invented/project/');await f.resolve(1);status(f,'Asks updated');
});

test('scope getters are not invoked; descriptor trap reentry reserves new intent before stale failure',async()=>{
  const f=fixture();f.mount();let getters=0;
  const bad=Object.defineProperty({},'mode',{get(){getters++;return 'all';}});assert.equal(f.view.loadScope(bad),false);assert.equal(getters,0);
  let once=false;const reentrant=new Proxy({}, {getOwnPropertyDescriptor(){if(!once){once=true;f.view.loadScope(all());}throw Error('invented');}});
  assert.equal(f.view.loadScope(reentrant),false);await tick();assert.equal(f.calls.length,1);assert.deepEqual(f.calls[0].s,all());await f.resolve();status(f,'Asks updated');
});

test('reader reentry A to B to C then throw frees token only after reader stack unwinds',async()=>{
  let f,inside=false,calls=0;f=fixture({readAsks:s=>{
    assert.equal(inside,false);inside=true;calls++;
    try {if(calls===1){f.view.loadScope(directory('/invented/B'));f.view.loadScope(all());throw Error('invented');}
      return {then:resolve=>resolve({data:dto(s)})};}finally{inside=false;}
  }});f.mount();await f.load();assert.equal(calls,2);status(f,'Asks updated');assert.equal(f.find('nav-asks-scope')[0].textContent,'All directories');
});

test('AbortController construction/signal failure with B/C reentry cannot leave a dead flight',async()=>{
  for(const site of ['constructor','signal']){
    const f=fixture();let count=0;f.window.AbortController=class {
      constructor(){count++;if(count===1&&site==='constructor'){f.view.loadScope(scope());f.view.loadScope(all());throw Error('invented');}}
      get signal(){if(count===1&&site==='signal'){f.view.loadScope(scope());f.view.loadScope(all());throw Error('invented');}return {};}
      abort(){}
    };
    f.mount();await f.load();assert.equal(count,2);assert.equal(f.calls.length,1);assert.deepEqual(f.calls[0].s,all());await f.resolve();status(f,'Asks updated');f.clean();
  }
});

test('outer then acquisition/invocation failure after reentry drains latest without recursive read',async()=>{
  for(const site of ['get','invoke']){
    let f,count=0,inside=false;f=fixture({readAsks:s=>{
      assert.equal(inside,false);count++;if(count>1)return Promise.resolve({data:dto(s)});
      return Object.defineProperty({},'then',{get(){inside=true;try{
        if(site==='get'){f.view.loadScope(scope());f.view.loadScope(all());throw Error('invented');}
        return ()=>{inside=true;try{f.view.loadScope(scope());f.view.loadScope(all());throw Error('invented');}finally{inside=false;}};
      }finally{inside=false;}}});
    }});f.mount();await f.load();assert.equal(count,2);status(f,'Asks updated');f.clean();
  }
});

test('first outer settlement wins over double resolve/reject and resolve then throw',async()=>{
  let calls=0;const f=fixture({readAsks:s=>{calls++;return{then(resolve,reject){resolve({data:dto(s)});resolve({data:{}});reject(Error('late'));throw Error('late');}};}});
  f.mount();await f.load();status(f,'Asks updated');assert.equal(f.find('nav-asks').length,1);assert.equal(calls,1);
});

test('stale reader return thenable is still invoked once after synchronous replacement',async()=>{
  let f,calls=0,subscriptions=0,rejectOld;
  f=fixture({readAsks:s=>{calls++;if(calls===1){f.view.loadScope(all());return{then(_resolve,reject){subscriptions++;rejectOld=reject;}};}return Promise.resolve({data:dto(s)});}});
  f.mount();await f.load();assert.equal(calls,1);assert.equal(subscriptions,1);rejectOld(Error('stale'));await tick();assert.equal(calls,2);status(f,'Asks updated');
});

test('malformed outer result/envelope fixed failure retains scope for Refresh; no DTO assimilation',async()=>{
  let calls=0,thenReads=0,dataReads=0;const data=dto(scope());
  Object.defineProperty(data,'then',{get(){thenReads++;throw Error('DTO then');}});
  const results=[null,{},Promise.resolve({}),Promise.resolve(Object.defineProperty({},'data',{get(){dataReads++;throw Error('getter');}})),Promise.resolve({data})];
  const f=fixture({readAsks:()=>results[calls++]});f.mount();await f.load();
  for(let i=0;i<4;i++){status(f,'Asks unavailable');noView(f);f.view.refresh();await tick();}
  status(f,'Asks updated');assert.equal(thenReads+dataReads,0);assert.equal(calls,5);
});

test('whole candidate stays hidden through later-row mount; actual buttons cannot escape',async()=>{
  for(const outcome of ['commit','throw','replace','dispose']){
    const f=fixture();f.mount();await f.load();let count=0,paused=false;
    f.hook(where=>{if(where==='constructor')count++;if(where!=='observe'||count!==3||paused)return;paused=true;
      assert.equal(f.find('nav-asks-candidate')[0].hidden,true);assert.ok(f.find('nav-asks-item-heading').every(n=>!visible(n)));
      f.click('Copy quote');f.click('Open conversation');
      if(outcome==='throw')throw Error('invented later row failure');
      if(outcome==='replace')f.view.loadScope(all());if(outcome==='dispose')f.view.teardown();
    });
    await f.resolve();assert.ok(paused);assert.equal(f.copies.length+f.navigations.length,0);f.hook(()=>{});
    if(outcome==='commit'){status(f,'Asks updated');f.click('Copy quote');await tick();assert.deepEqual(f.copies,[firstCopy]);}
    else if(outcome==='throw'){status(f,'Asks unavailable');noView(f);}
    else if(outcome==='replace'){noView(f);assert.equal(f.calls.length,2);await f.resolve(1);status(f,'Asks updated');}
    else assert.equal(f.find('nav-asks-controller').length,0);
    assert.ok(f.observers.filter(o=>o.disconnected).every(o=>o.disconnects===1));f.clean();
  }
});

test('text-only activation remains hidden through scheduling and failure/disposal',async()=>{
  for(const outcome of ['commit','throw','dispose']){
    const f=fixture();f.mount();await f.load();const data=dto(scope());
    for(const s of data.sections)for(const row of s.items)row.first={lines:[{kind:'text',table_rows:0,spans:[{c:[],d:false,t:'text-only fixture'}]}],copy:'> text-only fixture\n\n'};
    let count=0;f.schedule(()=>{count++;assert.equal(f.find('nav-asks-candidate')[0].hidden,true);f.click('Copy quote');f.click('Open conversation');
      if(outcome==='throw')throw Error('invented');if(outcome==='dispose')f.view.teardown();});
    await f.resolve(0,data);assert.ok(count>0);assert.equal(f.copies.length+f.navigations.length,0);f.schedule(()=>{});
    if(outcome==='commit')status(f,'Asks updated');else if(outcome==='throw'){status(f,'Asks unavailable');noView(f);}else assert.equal(f.find('nav-asks-controller').length,0);
    assert.deepEqual(f.announcements.filter(x=>x.includes('cop')),[]);f.clean();
  }
});

test('completion announcement stays unpublished; sink throw ignored, replacement/disposal cannot reveal',async()=>{
  for(const outcome of ['throw','replace','dispose']){
    let f,once=false;f=fixture({announce:message=>{f.announcements.push(message);if(message!=='Asks updated'||once)return;once=true;
      assert.equal(f.find('nav-asks-candidate')[0].hidden,true);f.click('Copy quote');f.click('Open conversation');
      if(outcome==='throw')throw Error('invented sink');if(outcome==='replace')f.view.loadScope(all());if(outcome==='dispose')f.view.teardown();
    }});f.mount();await f.load();await f.resolve();assert.ok(once);assert.equal(f.copies.length+f.navigations.length,0);
    if(outcome==='throw')assert.equal(f.find('nav-asks-candidate')[0].hidden,false);
    if(outcome==='replace'){noView(f);await f.resolve(1);status(f,'Asks updated');}
    if(outcome==='dispose')assert.equal(f.find('nav-asks-controller').length,0);
    assert.ok(!f.announcements.includes('Quote copied'));f.clean();
  }
});

test('disposal/replacement in actual constructor/observe defers all cleanup until acquired handles return',async()=>{
  for(const site of ['constructor','observe'])for(const outcome of ['replace','dispose']){
    const f=fixture();f.mount();await f.load();let paused=false,inside=false;
    f.hook(where=>{if(where==='disconnect')assert.equal(inside,false);if(where!==site||paused)return;paused=true;inside=true;
      try{if(outcome==='replace')f.view.loadScope(all());else f.view.teardown();assert.ok(f.observers.every(o=>!o.disconnected));}finally{inside=false;}});
    await f.resolve();assert.ok(paused);assert.ok(f.observers.every(o=>o.disconnected&&o.disconnects===1));f.hook(()=>{});
    if(outcome==='replace'){await f.resolve(1);status(f,'Asks updated');}f.clean();
  }
});

test('abort and child-cleanup reentry cannot overwrite newest scope or overlap readers',async()=>{
  const f=fixture();f.mount();await f.load();let aborts=0;f.calls[0].signal.addEventListener('abort',()=>{aborts++;f.view.loadScope(all());});
  assert.equal(f.view.loadScope(directory('/invented/B')),false);await tick();assert.equal(aborts,1);assert.equal(f.calls.length,1);await f.resolve();assert.deepEqual(f.calls[1].s,all());await f.resolve(1);
  let once=false;f.hook(where=>{if(where==='disconnect'&&!once){once=true;f.view.loadScope(directory('/invented/C'));}});
  assert.equal(f.view.loadScope(directory('/invented/B')),false);await tick();assert.deepEqual(f.calls[2].s,directory('/invented/C'));await f.resolve(2);status(f,'Asks updated');
});

test('normal void copy succeeds; synchronous failure fixed; navigation returns never assimilated',async()=>{
  let mode='void',thenReads=0;const f=fixture({copyText:t=>{f.copies.push(t);if(mode==='throw')throw Error('private fixture');},
    onConversation:value=>{f.navigations.push(value);return Object.defineProperty({},'then',{get(){thenReads++;throw Error('not consumed');}});}});
  f.mount();await f.load();await f.resolve();f.click('Copy quote');await tick();assert.deepEqual(f.copies,[firstCopy]);assert.ok(f.announcements.includes('Quote copied'));
  mode='throw';f.click('Copy quote');await tick();assert.equal(f.announcements.at(-1),'Could not copy quote');
  f.click('Open conversation');assert.deepEqual(f.navigations,[{provider:'agy',id:sourceID}]);assert.equal(thenReads,0);
});

test('one physical pending copy across replacement, no overlapping hook, stale settlement silent',async()=>{
  for(const settle of ['resolve','reject']){
    const pending=deferred();let count=0;const f=fixture({copyText:()=>{count++;return count===1?pending.promise:undefined;}});
    f.mount();await f.load();await f.resolve();f.click('Copy quote');await f.load(all());await f.resolve(1);
    f.click('Copy quote');await tick();assert.equal(count,1);assert.equal(f.announcements.at(-1),'Could not copy quote');
    const n=f.announcements.length;pending[settle](Error('invented'));await tick();assert.equal(f.announcements.length,n);
    f.click('Copy quote');await tick();assert.equal(count,2);assert.equal(f.announcements.at(-1),'Quote copied');f.clean();
  }
});

test('copy token precedes reentrant hook; trusted copy thenable normalized once and first settlement wins',async()=>{
  let f,count=0,thenReads=0,subscriptions=0;f=fixture({copyText:()=>{count++;f.click('Copy quote',2);
    return Object.defineProperty({},'then',{get(){thenReads++;return(resolve,reject)=>{subscriptions++;resolve({then(){assert.fail('copy result assimilated');}});reject(Error('late'));throw Error('late');};}});
  }});f.mount();await f.load();await f.resolve();f.click('Copy quote');await tick();assert.equal(count,1);assert.equal(thenReads,1);assert.equal(subscriptions,1);
  assert.equal(f.announcements.at(-1),'Quote copied');assert.equal(f.announcements.filter(x=>x==='Quote copied').length,1);
});

test('focus from retired content recovers to Refresh before cleanup, outside focus never stolen',async()=>{
  const f=fixture();f.mount();await f.load();await f.resolve();const button=f.find('nav-asks-button')[0];button.focus();let inspected=false;
  f.hook(where=>{if(where==='disconnect'){inspected=true;assert.equal(f.document.activeElement,f.find('nav-asks-refresh')[0]);assert.equal(button.isConnected,false);}});
  await f.load(all());assert.ok(inspected);const focus=f.document.focusCalls;await f.resolve(1);assert.equal(f.document.focusCalls,focus);f.hook(()=>{});
  const neighbor=f.document.createElement('button');f.document.body.append(neighbor);neighbor.focus();await f.load(scope());await f.resolve(2);
  assert.equal(f.document.activeElement,neighbor);f.view.teardown();assert.equal(f.document.activeElement,neighbor);assert.equal(neighbor.isConnected,true);
});

test('focus recovery reentry cannot resurrect retired scope; load completion never jumps focus',async()=>{
  const f=fixture();f.mount();await f.load();await f.resolve();f.find('nav-asks-button')[0].focus();let once=false;
  f.document.focusHook=()=>{if(!once){once=true;f.view.loadScope(all());}};
  assert.equal(f.view.loadScope(directory('/invented/B')),false);await tick();assert.ok(once);assert.deepEqual(f.calls[1].s,all());const n=f.document.focusCalls;await f.resolve(1);
  assert.equal(f.document.focusCalls,n);status(f,'Asks updated');
});

test('actual child mount false/failure gives fixed unavailable; retry preserves scope',async()=>{
  const f=fixture();f.mount();await f.load();f.document.refuseLines=true;await f.resolve();status(f,'Asks unavailable');noView(f);
  f.document.refuseLines=false;f.click('Refresh Asks');await tick();assert.deepEqual(f.calls[1].s,scope());await f.resolve(1);status(f,'Asks updated');
});

test('unknown/known report, zero/missing and 3/7 totals survive actual child admission and retirement',async()=>{
  const f=fixture();f.mount();
  for(const [known,totals] of [[false,[3,7]],[true,[3,7]],[true,[0,0]],[true,null]]){
    await f.load();const data=dto(scope());if(!known)data.report={known:false};
    if(totals){data.totals={utc_day:'1970-01-12',skipped:totals[0],calls_today:totals[1]};if(known)Object.assign(data.report,{skipped:totals[0],calls_today:totals[1]});}
    await f.resolve(f.calls.length-1,data);status(f,'Asks updated');assert.equal(f.find('nav-asks-report')[0].textContent.includes('Last stored watcher report'),known);
    assert.equal(f.find('nav-asks-totals').length,totals?1:0);if(totals)assert.equal(f.find('nav-asks-totals')[0].textContent,`Skipped: ${totals[0]}; calls today: ${totals[1]}; UTC day: 1970-01-12`);
  }
});

test('disposed pending reads/copies and saved frame callbacks stay silent, neighbor retained, idempotent',async()=>{
  const pending=deferred();const f=fixture({copyText:()=>pending.promise});f.mount();await f.load();await f.resolve();f.click('Copy quote');
  const frames=[...f.frames.values()];await f.load(all());const neighbor=f.document.createElement('button');f.document.body.append(neighbor);neighbor.focus();
  const n=f.announcements.length;f.view.teardown();f.view.teardown();pending.resolve();f.calls[1].resolve({data:dto(all())});await tick();
  for(const fn of frames)fn();assert.equal(f.announcements.length,n);assert.equal(f.document.activeElement,neighbor);assert.equal(f.view.mount(),false);
  assert.equal(f.view.refresh(),false);assert.equal(f.view.loadScope(all()),false);assert.equal(f.calls.length,2);assert.equal(f.find('nav-asks-controller').length,0);
});
