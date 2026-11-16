import test, {afterEach} from 'node:test';
import assert from 'node:assert/strict';
import {createAsksView,navigatorAskCommand,navigatorRemedyCommands} from './asks_view.js';
import {snapshot,scope,limits,firstCopy,latestCopy,sourceID} from './asks_values.mjs';

// Synthetic event/resource ownership only; supplied geometry is NOT browser evidence.
class Target {
  listeners=new Map();
  addEventListener(name,fn){if(!this.listeners.has(name))this.listeners.set(name,new Set());this.listeners.get(name).add(fn);}
  removeEventListener(name,fn){this.listeners.get(name)?.delete(fn);}
  emit(name){for(const fn of [...(this.listeners.get(name)||[])])fn();}
  count(){return [...this.listeners.values()].reduce((n,s)=>n+s.size,0);}
}
class Element extends Target {
  constructor(document,tag){super();Object.assign(this,{ownerDocument:document,tagName:tag,children:[],parentNode:null,attrs:new Map(),className:'',value:'',hidden:false,clientWidth:100,scrollWidth:300});
    this.classList={add:name=>{this.className=[...new Set([...this.className.split(' ').filter(Boolean),name])].join(' ');}};}
  append(...nodes){for(const n of nodes){if(n.tagName==='#fragment'){this.append(...n.children);n.children=[];continue;}n.remove();n.parentNode=this;this.children.push(n);}}
  replaceChildren(...nodes){for(const n of [...this.children])n.remove();this.value='';this.append(...nodes);}
  remove(){if(this.parentNode)this.parentNode.children=this.parentNode.children.filter(n=>n!==this);this.parentNode=null;}
  set textContent(value){assert.equal(typeof value,'string');this.replaceChildren();this.value=value;}
  get textContent(){return this.value+this.children.map(n=>n.textContent).join('');}
  setAttribute(name,value){this.attrs.set(name,String(value));}
  getAttribute(name){return this.attrs.get(name)??null;}
  removeAttribute(name){this.attrs.delete(name);}
  contains(n){return this===n||this.children.some(c=>c.contains(n));}
  get isConnected(){if(this.className==='nav-md'&&this.ownerDocument.refuseLines)return false;return this===this.ownerDocument.body||Boolean(this.parentNode?.isConnected);}
  getClientRects(){for(let n=this;n;n=n.parentNode)if(n.hidden)return [];return this.isConnected?[{}]:[];}
  click(){this.emit('click');}
  focus(){this.ownerDocument.activeElement=this;this.ownerDocument.focusCalls++;}
}
const walk=n=>[n,...n.children.flatMap(walk)];
const has=(n,c)=>n.className.split(' ').includes(c);
const find=(f,c)=>walk(f.document.body).filter(n=>has(n,c));
const visible=n=>{for(;n;n=n.parentNode)if(n.hidden)return false;return true;};
const click=(f,text,index=0)=>walk(f.document.body).filter(n=>n.tagName==='button'&&n.textContent===text)[index].click();
const tick=async()=>{for(let i=0;i<12;i++)await Promise.resolve();};
const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return{promise,resolve,reject};};
const fixtures=new Set();
afterEach(()=>{for(const f of [...fixtures])f.clean();});
function fixture(extra={},data=snapshot()) {
  const window=new Target(),nodes=[],frames=new Map(),observers=[];let serial=0,hook=()=>{},scheduleHook=()=>{};
  const document={defaultView:window,activeElement:null,focusCalls:0,fonts:new Target(),createElement(tag){const n=new Element(document,tag);nodes.push(n);return n;},createDocumentFragment(){return new Element(document,'#fragment');}};
  document.body=new Element(document,'body');let fontsReady;document.fonts.ready=new Promise(r=>fontsReady=r);
  window.visualViewport=new Target();window.requestAnimationFrame=fn=>{scheduleHook();const id=++serial;frames.set(id,fn);return id;};window.cancelAnimationFrame=id=>frames.delete(id);
  window.AbortController=AbortController;
  window.ResizeObserver=class {constructor(fn){this.callback=fn;this.disconnected=false;observers.push(this);hook('constructor',this);}observe(n){hook('observe',this,n);}disconnect(){this.disconnected=true;}};
  const copies=[],announcements=[],navigations=[];
  const options={window,idPrefix:'asksFixture',expectedScope:scope(),limits:limits(),copyText:text=>{copies.push(text);},announce:text=>announcements.push(text),onConversation:value=>navigations.push(value),...extra};
  const view=createAsksView(document,data,options);
  const f={view,options,window,document,nodes,frames,observers,copies,announcements,navigations,
    hook:fn=>hook=fn,schedule:fn=>scheduleHook=fn,connect(){document.body.append(view.fragment);},
    mount(){this.connect();assert.equal(view.mount(),true);},
    flush(){const pending=[...frames.values()];frames.clear();for(const fn of pending)fn();},
    finishFonts(){fontsReady();},clean(){view.teardown();view.teardown();fixtures.delete(f);
      assert.equal(window.count()+window.visualViewport.count()+document.fonts.count()+frames.size+nodes.reduce((n,x)=>n+x.count(),0),0);
      assert.ok(observers.every(o=>o.disconnected));}};
  fixtures.add(f);return f;
}

test('inbox selects the latest question and only its current panel can copy',async()=>{
 const f=fixture({inbox:true});f.mount();
 const panels=find(f,'nav-asks-selected'),picks=find(f,'nav-asks-pick');
 assert.equal(panels.filter(visible).length,1);assert(find(f,'nav-asks-question').filter(visible)[0].textContent.includes('latest quote'));
 const hiddenCopy=walk(panels.find(n=>!visible(n))).find(n=>n.tagName==='button'&&n.textContent==='Copy quote');hiddenCopy.click();await tick();assert.equal(f.copies.length,0);
 const activeCopy=()=>walk(panels.find(visible)).find(n=>n.tagName==='button'&&n.textContent==='Copy quote');activeCopy().click();await tick();assert.deepEqual(f.copies,[latestCopy]);
 picks[1].click();assert.equal(panels.filter(visible).length,1);assert.equal(picks[1].getAttribute('aria-current'),'true');assert.equal(picks[0].getAttribute('aria-current'),null);
 activeCopy().click();await tick();assert.deepEqual(f.copies,[latestCopy,latestCopy]);assert.equal(walk(panels.find(visible)).find(n=>n.tagName==='button'&&n.textContent==='Conversation unavailable').disabled,true);
 f.clean();
});
test('inbox preserves wording/copy agreement and clears a selection when its group closes',async()=>{
 const f=fixture({inbox:true});f.mount();
 let panel=find(f,'nav-asks-selected').find(visible);
 walk(panel).find(n=>n.tagName==='button'&&n.textContent==='Show original').click();
 assert(find(f,'nav-asks-question').find(visible).textContent.includes('original quote'));
 walk(panel).find(n=>n.tagName==='button'&&n.textContent==='Copy quote').click();await tick();assert.deepEqual(f.copies,[firstCopy]);
 click(f,'Needs your answer (1)');panel=find(f,'nav-asks-selected').find(visible);assert(panel);assert.equal(find(f,'nav-asks-pick').filter(n=>n.getAttribute('aria-current')==='true').length,1);
 assert(find(f,'nav-asks-question').find(visible).textContent.includes('latest quote'));f.clean();
});
test('inbox restores only selection and expanded groups from its bounded presentation state',()=>{
 const f=fixture({inbox:true,viewState:{selectedId:'2',expanded:[false,true,false,false,false]}});f.mount();
 assert.equal(find(f,'nav-asks-context').find(visible).textContent,'Ask 2 · project · agy');
 const state=f.view.viewState();assert.deepEqual(state,{selectedId:'2',sessionKey:JSON.stringify(['/invented/project','agy',sourceID]),history:false,expanded:[false,true,false,false,false]});assert(Object.isFrozen(state)&&Object.isFrozen(state.expanded));
 assert.equal(f.view.focusSelected(),true);assert.equal(f.document.activeElement.getAttribute('aria-current'),'true');f.clean();assert.equal(f.view.focusSelected(),false);
});
test('initially collapsed inbox groups start with latest wording and retain an explicit original choice',async()=>{
 for(const restored of [undefined,{selectedId:'3',expanded:[false,false,false,false,false]}]){
  const f=fixture({inbox:true,viewState:restored});f.mount();click(f,'Closed and dismissed (1)');find(f,'nav-asks-pick')[5].click();
  let panel=find(f,'nav-asks-selected').find(visible);assert(find(f,'nav-asks-question').find(visible).textContent.includes('latest quote'));
  walk(panel).find(n=>n.tagName==='button'&&n.textContent==='Copy quote').click();await tick();assert.equal(f.copies.at(-1),latestCopy);
  walk(panel).find(n=>n.tagName==='button'&&n.textContent==='Show original').click();click(f,'Closed and dismissed (1)');click(f,'Closed and dismissed (1)');find(f,'nav-asks-pick')[5].click();
  panel=find(f,'nav-asks-selected').find(visible);assert(find(f,'nav-asks-question').find(visible).textContent.includes('original quote'));walk(panel).find(n=>n.tagName==='button'&&n.textContent==='Copy quote').click();await tick();assert.equal(f.copies.at(-1),firstCopy);f.clean();
 }
});
test('detached construction, retry, five native disclosures, exact owned text and mode',async()=>{
  const data=snapshot(),f=fixture({},data);assert.equal(f.view.mount(),false);
  f.view.refresh();assert.equal(f.copies.length+f.navigations.length+f.observers.length,0);
  data.sections[0].items[0].first.copy='changed';f.options.copyText=()=>assert.fail('captured hook changed');
  f.mount();assert.equal(f.view.mount(),true);assert.equal(f.document.focusCalls,0);
  assert.deepEqual(find(f,'nav-asks-group-heading').map(n=>n.textContent),['Asks (2)','Warnings (1)','Other open (1)','Probably done (1)','Closed and dismissed (1)']);
  assert.deepEqual(find(f,'nav-asks-region').map(n=>n.hidden),[false,false,true,true,true]);
  assert.equal(find(f,'nav-asks-item').length,6);assert.equal(find(f,'nav-asks-item-heading')[0].textContent,'Ask 9007199254740993');
  assert.equal(find(f,'nav-asks-source')[1].textContent,'unsupported: unavailable');
  assert.equal(find(f,'nav-asks-button').filter(n=>n.textContent==='Conversation unavailable')[0].disabled,true);
  const line=find(f,'nav-md-line').find(n=>n.textContent.includes('<img'));assert.equal(line.children[0].tagName,'span');
  assert.equal(find(f,'nav-md-rule')[0].getAttribute('aria-hidden'),'true');
  assert.ok(find(f,'nav-md-sr').some(n=>n.textContent==='table, 2 rows'));
  assert.ok(find(f,'nav-asks-report')[0].textContent.includes('Last stored watcher report'));
  assert.ok(find(f,'nav-asks-report')[0].textContent.includes('1970-01-01T00:00:00.000Z'));
  assert.ok(find(f,'nav-asks-report')[0].textContent.includes('milliseconds since epoch: 9007199254740991'));
  click(f,'Copy quote');await tick();assert.deepEqual(f.copies,[firstCopy]);
});

test('details atomically select latest paint/copy, keep focus and preserve visible state reasons',async()=>{
  const f=fixture();f.mount();const toggle=find(f,'nav-asks-button').find(n=>n.textContent==='Show details');toggle.focus();toggle.click();
  assert.equal(f.document.activeElement,toggle);assert.equal(toggle.textContent,'Show original');
  assert.equal(find(f,'nav-asks-wording')[0].textContent,'Latest wording');assert.equal(find(f,'nav-asks-details')[0].hidden,false);
  assert.ok(find(f,'nav-md-line').filter(visible).some(n=>n.textContent==='latest quote'));
  click(f,'Copy quote');await tick();assert.deepEqual(f.copies,[latestCopy]);
  toggle.click();click(f,'Copy quote');await tick();assert.deepEqual(f.copies,[latestCopy,firstCopy]);
  click(f,'Closed and dismissed (1)');const footer=find(f,'nav-asks-state').find(n=>n.textContent.includes('stored decision'));
  assert.ok(visible(footer));assert.equal(find(f,'nav-asks-details').at(-1).hidden,true);
  assert.ok(find(f,'nav-asks-ref').every(n=>n.tagName==='p'));
});

test('normal VOID refresh schedules geometry; hide/reveal coalesces without false failure',()=>{
  const f=fixture();f.mount();const before=f.frames.size;assert.ok(before>0);
  f.view.refresh();assert.equal(f.frames.size,before);assert.equal(find(f,'nav-md-scroll')[0].getAttribute('role'),null);
  click(f,'Asks (2)');assert.equal(find(f,'nav-asks-region')[0].hidden,true);
  click(f,'Asks (2)');assert.equal(find(f,'nav-asks-region')[0].hidden,false);assert.equal(f.frames.size,before);
  f.flush();assert.equal(find(f,'nav-md-scroll')[0].getAttribute('role'),'group');
  assert.equal(find(f,'nav-md-scroll')[0].getAttribute('tabindex'),'0');
  assert.ok(!f.document.body.textContent.includes('Asks unavailable'));
});

test('pending physical copy survives toggle-back/hide-reopen while old intent stays silent',async()=>{
  const d=deferred();let calls=0;const f=fixture({copyText:()=>{calls++;return d.promise;}});f.mount();
  click(f,'Copy quote');click(f,'Show details');click(f,'Show original');click(f,'Asks (2)');click(f,'Asks (2)');
  click(f,'Copy quote');click(f,'Copy quote',2);assert.equal(calls,1);
  d.resolve();await tick();assert.deepEqual(f.announcements,[]);
  click(f,'Copy quote');await tick();assert.equal(calls,2);assert.deepEqual(f.announcements,['Quote copied']);
});

test('copy throws/rejects/double-settles once and permit stays held through announce',async()=>{
  let f,calls=0,mode='throw';f=fixture({copyText:()=>{calls++;if(mode==='throw')throw Error('private');if(mode==='reject')return Promise.reject(Error('private'));return{then(resolve,reject){resolve();resolve();reject(Error('late'));}};},
    announce:text=>{f.announcements.push(text);click(f,'Copy quote');}});f.mount();
  click(f,'Copy quote');await tick();assert.equal(calls,1);assert.deepEqual(f.announcements,['Could not copy quote']);
  mode='reject';click(f,'Copy quote');await tick();assert.equal(calls,2);
  mode='double';click(f,'Copy quote');await tick();assert.equal(calls,3);assert.deepEqual(f.announcements,['Could not copy quote','Could not copy quote','Quote copied']);
});

test('navigation passes fresh exact primitives, ignores return thenables and contains stale failure',()=>{
  let f,reads=0;const seen=[];f=fixture({onConversation:value=>{assert.deepEqual(value,{provider:'agy',id:sourceID});seen.push(value);value.id='mutated';return Object.defineProperty({},'then',{get(){reads++;throw Error('not consumed');}});}});f.mount();
  click(f,'Open conversation');click(f,'Open conversation');assert.equal(reads,0);assert.equal(seen.length,2);assert.notEqual(seen[0],seen[1]);
  // Mutating callback payloads cannot alter the next captured source identity.
  let second;const g=fixture({idPrefix:'other',onConversation:value=>{second={...value};g.view.teardown();throw Error('private');}});g.mount();click(g,'Open conversation');
  assert.deepEqual(second,{provider:'agy',id:sourceID});assert.deepEqual(g.announcements,[]);
});

test('constructor and observe reentry cannot copy/navigate/toggle before ready publication',()=>{
  for(const site of ['constructor','observe']) {
    const f=fixture();let attempts=0;f.hook(where=>{if(where!==site)return;attempts++;click(f,'Copy quote');click(f,'Open conversation');click(f,'Show details');click(f,'Asks (2)');});
    f.mount();assert.ok(attempts>0);assert.equal(f.copies.length+f.navigations.length,0);
    assert.equal(find(f,'nav-asks-region')[0].hidden,false);assert.equal(find(f,'nav-asks-wording')[0].textContent,'Original wording');
    f.hook(()=>{});f.clean();
  }
});

test('disposal during actual helper constructor/observe/refresh defers cleanup until unwind',()=>{
  for(const site of ['constructor','observe','refresh']) {
    const f=fixture();let once=false;
    const dispose=()=>{if(!once){once=true;f.view.teardown();}};
    if(site==='refresh')f.schedule(dispose);else f.hook(where=>{if(where===site)dispose();});
    f.connect();assert.equal(f.view.mount(),false);assert.ok(once);assert.equal(find(f,'nav-asks').length,0);f.clean();
  }
});

test('Copy during replacement never uses an activating Latest candidate, even on later failure',async()=>{
  for(const site of ['constructor','observe'])for(const result of ['commit','throw','dispose']) {
    const f=fixture();f.mount();f.flush();let attempted=false,throwLater=false;
    const oldCopy=find(f,'nav-asks-button').find(n=>n.textContent==='Copy quote');
    f.hook(where=>{
      if(where===site&&!attempted){attempted=true;oldCopy.click();click(f,'Open conversation');click(f,'Show details');click(f,'Asks (2)');
        if(result==='dispose')f.view.teardown();if(result==='throw')throwLater=true;}
      // Throw from observe after construction has returned an owned observer handle.
      if(where==='observe'&&throwLater)throw Error('invented activation failure');
    });
    click(f,'Show details');assert.ok(attempted);assert.deepEqual(f.copies,[]);assert.deepEqual(f.navigations,[]);
    f.hook(()=>{});
    if(result==='commit'){assert.equal(find(f,'nav-asks-wording')[0].textContent,'Latest wording');click(f,'Copy quote');await tick();assert.deepEqual(f.copies,[latestCopy]);}
    else if(result==='throw')assert.equal(f.document.body.textContent,'AsksAsks unavailable');
    else assert.equal(find(f,'nav-asks').length,0);
    f.clean();
  }
});

test('later initial activation failure and false mount remove ALL actions and helpers',()=>{
  for(const failure of ['observe','false']) {
    const f=fixture();let count=0;
    if(failure==='false')f.document.refuseLines=true;
    else f.hook(where=>{if(where==='constructor')count++;if(where==='observe'&&count===3)throw Error('private raw failure');});
    f.connect();assert.equal(f.view.mount(),false);assert.equal(f.document.body.textContent,'AsksAsks unavailable');
    assert.equal(find(f,'nav-asks-button').length,0);assert.equal(f.view.mount(),false);f.view.refresh();f.clean();
  }
});

test('wording activation/refresh failure is terminal; old detached controls cannot copy',()=>{
  for(const failure of ['observe','refresh']) {
    const f=fixture();f.mount();f.flush();const old=find(f,'nav-asks-button').find(n=>n.textContent==='Copy quote');
    if(failure==='observe')f.hook(where=>{if(where==='observe')throw Error('private');});
    else f.schedule(()=>{throw Error('private');});
    click(f,'Show details');old.click();assert.deepEqual(f.copies,[]);
    assert.equal(f.document.body.textContent,'AsksAsks unavailable');f.clean();
  }
});

test('two instances have disjoint IDs, callbacks and teardown; late frames/copy stay silent',async()=>{
  const pending=deferred(),f=fixture({copyText:()=>pending.promise});f.mount();
  const other=createAsksView(f.document,snapshot(),{...f.options,idPrefix:'neighbor'});f.document.body.append(other.fragment);assert.equal(other.mount(),true);
  const ids=walk(f.document.body).filter(n=>n.id).map(n=>n.id);assert.equal(new Set(ids).size,ids.length);
  const neighbor=find(f,'nav-asks')[1],button=walk(neighbor).find(n=>n.tagName==='button');button.focus();
  click(f,'Copy quote');const callbacks=[...f.frames.entries()];f.view.teardown();pending.resolve();await tick();
  for(const [id,fn] of callbacks){f.frames.delete(id);fn();}assert.deepEqual(f.announcements,[]);assert.equal(f.document.activeElement,button);
  assert.equal(neighbor.isConnected,true);other.teardown();f.finishFonts();await tick();f.clean();
});

test('unknown report is factual unavailable text and invalid input refuses before any DOM creation',()=>{
  const data=snapshot();delete data.report;const f=fixture({},data);f.mount();assert.equal(find(f,'nav-asks-report')[0].textContent,'Watcher report unavailable');
  let creates=0;const document={defaultView:{},createElement(){creates++;throw Error('must not build');}};
  assert.throws(()=>createAsksView(document,{},{window:document.defaultView,idPrefix:'invalid',expectedScope:scope(),limits:limits(),copyText(){},announce(){},onConversation(){}}),{name:'TypeError',message:'could not capture asks snapshot'});
  assert.equal(creates,0);
});


test('retained disposed handle severs title/status links to metadata DOM',()=>{
  const f=fixture();f.mount();
  const outer=find(f,'nav-asks')[0],title=find(f,'nav-asks-heading')[0],status=find(f,'nav-asks-status')[0];
  assert.ok(outer.textContent.includes('stored <error> text'));
  const neighbor=f.document.createElement('button');f.document.body.append(neighbor);neighbor.focus();
  const retained=f.view;retained.teardown();
  assert.equal(outer.children.length,0);assert.equal(outer.textContent,'');
  assert.equal(title.parentNode,null);assert.equal(status.parentNode,null);
  assert.equal(f.document.activeElement,neighbor);assert.equal(neighbor.isConnected,true);
  retained.teardown();assert.equal(retained.mount(),false);f.clean();
});

test('text-only replacement stays hidden through scheduling until wording is committed',async()=>{
  for(const outcome of ['commit','throw','dispose']){
    const data=snapshot();data.sections[0].items[0].latest={
      lines:[{kind:'text',table_rows:0,spans:[{c:[],d:false,t:'replacement text-only'}]}],
      copy:'> replacement text-only\n\n'};
    const f=fixture({},data);f.mount();f.flush();const observations=[];
    f.schedule(()=>{
      const article=find(f,'nav-asks-item')[0];
      const latest=walk(article).find(n=>has(n,'nav-md-line')&&n.textContent==='replacement text-only');
      observations.push({latestVisible:visible(latest),wording:find(f,'nav-asks-wording')[0].textContent,
        detailsHidden:find(f,'nav-asks-details')[0].hidden,toggle:walk(article).find(n=>n.tagName==='button').textContent});
      click(f,'Copy quote');click(f,'Open conversation');
      if(outcome==='dispose')f.view.teardown();
      if(outcome==='throw')throw Error('invented text-only schedule failure');
    });
    click(f,'Show details');
    assert.deepEqual(observations,[{latestVisible:false,wording:'Original wording',detailsHidden:true,toggle:'Show details'}]);
    assert.deepEqual(f.copies,[]);assert.deepEqual(f.navigations,[]);f.schedule(()=>{});
    if(outcome==='commit'){assert.equal(find(f,'nav-asks-wording')[0].textContent,'Latest wording');click(f,'Copy quote');await tick();assert.deepEqual(f.copies,['> replacement text-only\n\n']);}
    else if(outcome==='throw')assert.equal(f.document.body.textContent,'AsksAsks unavailable');
    else assert.equal(find(f,'nav-asks').length,0);
    f.clean();
  }
});


test('independent totals remain visible apart from stored watcher facts',()=>{
  for (const facts of [{known:false,skipped:3,calls:7},{known:true,skipped:3,calls:7},
    {known:true,skipped:0,calls:0},{known:true,missing:true}]) {
    const data=snapshot();
    if (!facts.known) data.report={known:false};
    if (!facts.missing) {
      if (facts.known) Object.assign(data.report,{skipped:facts.skipped,calls_today:facts.calls});
      data.totals={utc_day:'1970-01-12',skipped:facts.skipped,calls_today:facts.calls};
    }
    const f=fixture({},data);
    if (data.totals) Object.assign(data.totals,{utc_day:'changed',skipped:99,calls_today:99});
    f.mount();
    const root=find(f,'nav-asks')[0],paragraphs=find(f,'nav-asks-totals'),report=find(f,'nav-asks-report')[0];
    assert.equal(paragraphs.length,facts.missing?0:1);
    if (!facts.missing) {
      assert.equal(paragraphs[0].textContent,`Skipped: ${facts.skipped}; calls today: ${facts.calls}; UTC day: 1970-01-12`);
      assert.equal(paragraphs[0].parentNode,root);
      assert.equal(report.contains(paragraphs[0]),false);
      assert.equal(visible(paragraphs[0]),true);
    }
    assert.equal(report.textContent.includes('Last stored watcher report'),facts.known);
    assert.equal(report.textContent.includes('Watcher report unavailable'),!facts.known);
    if (facts.known) assert.ok(report.textContent.includes(`skipped: ${facts.missing?3:facts.skipped}; calls today: ${facts.missing?4:facts.calls}`));
    const neighbor=f.document.createElement('button');f.document.body.append(neighbor);neighbor.focus();
    f.view.teardown();assert.equal(root.textContent,'');assert.equal(f.document.activeElement,neighbor);
    f.clean();
  }
});

test('scope counts, fixed remedies and decimal command identities remain separate from stored wording',async()=>{
 const data=snapshot();data.scope_counts={open:4,elsewhere_open:7};data.status_line={known:true,problem:true,text:'Stored wording: never copy aiq asks reset --all',remedy:['watch','setup','service_install','watch','setup']};
 const f=fixture({},data);f.mount();assert.ok(f.document.body.textContent.includes('4 open here · 7 open elsewhere'));
 assert.equal(find(f,'nav-asks-all')[0].href,'/asks?all=true');
 assert.equal(find(f,'nav-asks-trust')[0].textContent,'! Stored wording: never copy aiq asks reset --all');
 const remedies=find(f,'nav-asks-button').filter(n=>n.textContent.startsWith('Copy ')&&!['Copy quote','Copy dismiss command','Copy restore command'].includes(n.textContent));
 assert.deepEqual(remedies.map(n=>n.textContent),['Copy background watcher command','Copy setup command','Copy watch command (in its own window)']);
 for(const control of remedies){control.click();await tick();}
 click(f,'Copy dismiss command');await tick();assert.deepEqual(f.copies,['aiq asks service install','aiq asks setup','aiq asks watch','aiq asks dismiss 9007199254740993']);
 const restore=find(f,'nav-asks-button').find(n=>n.textContent==='Copy restore command');restore.click();await tick();assert.equal(f.copies.length,4);
 click(f,'Probably done (1)');restore.click();await tick();assert.equal(f.copies.at(-1),'aiq asks restore 4');
 f.clean();remedies[0].click();assert.equal(f.copies.length,5);
});

test('command and quote copies share one pending slot and retire without stale announcements',async()=>{
 const d=deferred(),f=fixture({copyText:text=>{f.copies.push(text);return d.promise;}});f.mount();
 click(f,'Copy dismiss command');click(f,'Copy quote');assert.deepEqual(f.copies,['aiq asks dismiss 9007199254740993']);
 assert.equal(find(f,'nav-asks-button').find(n=>n.textContent==='Copy quote').getAttribute('aria-disabled'),'true');
 f.clean();const before=f.announcements.length;d.resolve();await tick();assert.equal(f.announcements.length,before);
});

test('ask commands preserve maximum int64 decimal text and reject shell or rounded identities',()=>{
 assert.equal(navigatorAskCommand('9223372036854775807','open'),'aiq asks dismiss 9223372036854775807');
 for(const id of ['0','01','1\n','1;echo invented','9223372036854775808',9223372036854775807,null,{}])assert.equal(navigatorAskCommand(id,'open'),null);
 assert.equal(navigatorAskCommand('7','unknown'),null);
 assert.deepEqual(navigatorRemedyCommands(['watch','__proto__','setup','watch']).map(x=>x[0]),['setup','watch']);
});


test('session-first inbox distinguishes providers, session identities and equal directory basenames',()=>{
 const data=snapshot();data.sections[0].items[1].id='11';data.sections[0].items[1].provider='codex';data.sections[0].items[1].session_id=sourceID;
 data.sections[1].items[0].workspace='/elsewhere/project';
 const f=fixture({inbox:true},data);f.mount();
 const sessions=find(f,'nav-asks-session-pick');assert.equal(sessions.length,3);
 assert.deepEqual(find(f,'nav-asks-session-count').map(n=>n.textContent),['2','1','1']);
 assert.equal(find(f,'nav-asks-session-path')[0].textContent,'/invented/project');
 sessions[2].click();assert.equal(find(f,'nav-asks-session-path')[0].textContent,'/elsewhere/project');
 assert.equal(find(f,'nav-asks-pick').filter(visible).length,1);
 assert.equal(find(f,'nav-asks-context').find(visible).textContent,'Ask 2 · project · agy');
 f.clean();
});

test('inbox search selects a matching session and does not merge unidentified sessions',()=>{
 const data=snapshot();data.sections[0].items[1].id='11';data.sections[0].items[1].latest.lines[0].spans[0].t='unique meteor wording';
 data.sections[1].items[0].provider='unsupported';data.sections[1].items[0].session_id='';
 const f=fixture({inbox:true},data);f.mount();assert.equal(find(f,'nav-asks-session-pick').length,3);
 assert.equal(f.view.setFilter('meteor',true),true);assert.equal(find(f,'nav-asks-pick').filter(visible).length,1);
 assert.equal(find(f,'nav-asks-context').find(visible).textContent,'Ask 11 · project · unsupported');
 assert.equal(f.view.setFilter('no invented match'),false);assert.equal(find(f,'nav-asks-selected').filter(visible).length,0);
 assert.equal(f.view.setFilter(''),true);assert.equal(find(f,'nav-asks-selected').filter(visible).length,1);f.clean();
});

test('dismiss uses admitted revision once and retired or hidden panels cannot act',async()=>{
 const data=snapshot();for(const group of data.sections)for(const row of group.items)row.revision='a'.repeat(64);
 const pending=deferred(),calls=[],f=fixture({inbox:true,onAction:q=>{calls.push(q);return pending.promise;}},data);f.mount();
 const actions=find(f,'nav-asks-action');const selected=actions.find(visible),hidden=actions.find(n=>!visible(n));hidden.click();selected.click();selected.click();
 assert.deepEqual(calls,[{id:'9007199254740993',revision:'a'.repeat(64),action:'dismiss'}]);assert(Object.isFrozen(calls[0]));
 f.view.teardown();selected.click();pending.resolve();await tick();assert.equal(calls.length,1);f.clean();
});


test('reselecting the current ask or session preserves its host-owned conversation context',()=>{
 const f=fixture({inbox:true});f.mount();const detail=find(f,'nav-asks-inbox-detail')[0],context=f.document.createElement('aside');context.textContent='Invented conversation context';detail.append(context);
 const selected=find(f,'nav-asks-pick').find(n=>n.getAttribute('aria-current')==='true');selected.click();assert.equal(context.isConnected,true);assert(detail.contains(context));
 find(f,'nav-asks-session-pick').find(n=>n.getAttribute('aria-current')==='true').click();assert.equal(context.isConnected,true);assert(detail.contains(context));f.clean();
});

test('session dismissal snapshots all open groups despite filtering and requires confirmation',async()=>{
 const data=snapshot();data.sections[0].items[1].id='12';for(const group of data.sections)for(const row of group.items)row.revision='a'.repeat(64);
 const calls=[],pending=deferred(),f=fixture({inbox:true,onBatchAction:(...args)=>{calls.push(args);return pending.promise;}},data);f.mount();
 f.view.setFilter('latest',true);const bulk=find(f,'nav-asks-bulk')[0];assert.equal(bulk.textContent,'Dismiss all 3 pending asks');bulk.click();assert.equal(calls.length,0);
 let confirm=find(f,'nav-asks-bulk-confirm')[0];assert(confirm.textContent.includes('/invented/project'));assert(confirm.textContent.includes(sourceID));click(f,'Cancel');assert.equal(calls.length,0);assert.equal(confirm.hidden,true);
 for(let i=0;i<3;i++){bulk.click();click(f,'Cancel');}
 bulk.click();click(f,'Dismiss 3 asks');assert.equal(calls.length,1);assert.deepEqual(calls[0][0].map(row=>row.id),['9007199254740993','2','3']);assert(calls[0][0].every(row=>row.action==='dismiss'&&Object.isFrozen(row)));assert(Object.isFrozen(calls[0][0]));assert.equal(calls[0][1].id,sourceID);assert.equal(calls[0][3](),true);assert.equal(bulk.disabled,true);
 find(f,'nav-asks-session-pick')[1].click();assert.equal(calls[0][2].aborted,true);assert.equal(calls[0][3](),false);pending.resolve();await tick();f.clean();
});

test('passive filter restoration does not replace a session that still has pending asks',()=>{
 const data=snapshot();data.sections[0].items[1].id='12';data.sections[0].items[1].latest.lines[0].spans[0].t='unique other-session meteor';
 const f=fixture({inbox:true},data);f.mount();const key=f.view.viewState().sessionKey;
 assert.equal(f.view.setFilter('meteor'),true);assert.equal(f.view.viewState().sessionKey,key);assert.equal(find(f,'nav-asks-selected').filter(visible).length,0);
 f.view.setFilter('meteor',true);assert.notEqual(f.view.viewState().sessionKey,key);assert.equal(find(f,'nav-asks-selected').filter(visible).length,1);f.clean();
});

test('searching away permanently cancels a batch even when search returns before settlement',async()=>{
 const data=snapshot();data.sections[0].items[1].id='12';for(const group of data.sections)for(const row of group.items)row.revision='a'.repeat(64);
 data.sections[0].items[0].latest.lines[0].spans[0].t='original-session unique decision';data.sections[0].items[1].latest.lines[0].spans[0].t='other-session unique decision';
 const pending=deferred();let flight;const f=fixture({inbox:true,onBatchAction:(items,scope,signal,valid)=>{flight={signal,valid};return pending.promise;}},data);f.mount();const key=f.view.viewState().sessionKey;
 find(f,'nav-asks-bulk')[0].click();click(f,'Dismiss 3 asks');assert.equal(flight.valid(),true);
 f.view.setFilter('other-session',true);assert.notEqual(f.view.viewState().sessionKey,key);assert.equal(flight.signal.aborted,true);
 f.view.setFilter('original-session',true);assert.equal(f.view.viewState().sessionKey,key);assert.equal(flight.valid(),false);
 pending.resolve();await tick();f.clean();
});
