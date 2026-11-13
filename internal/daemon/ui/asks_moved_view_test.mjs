import test,{afterEach} from 'node:test';
import assert from 'node:assert/strict';
import {createAsksView} from './asks_view.js';
import {createAsksController} from './asks_controller.js';
import {scope,limits,firstCopy,latestCopy,sourceID} from './asks_values.mjs';
import {movedSnapshot} from './asks_moved_values.mjs';

// Bounded synthetic focus propagation/resource ownership. Native cases separately
// establish platform event order, layout, AX and stopped-propagation behavior.
class Target {
  listeners=new Map();
  addEventListener(type,fn,capture=false){const cap=typeof capture==='object'?Boolean(capture.capture):Boolean(capture);if(!this.listeners.has(type))this.listeners.set(type,[]);const a=this.listeners.get(type);if(!a.some(x=>x.fn===fn&&x.cap===cap))a.push({fn,cap});}
  removeEventListener(type,fn,capture=false){const cap=typeof capture==='object'?Boolean(capture.capture):Boolean(capture);this.listeners.set(type,(this.listeners.get(type)||[]).filter(x=>x.fn!==fn||x.cap!==cap));}
  emit(type){for(const {fn}of [...(this.listeners.get(type)||[])])fn();}
  count(){return [...this.listeners.values()].reduce((n,a)=>n+a.length,0);}
}
function dispatch(node,type,bubbles=true){
  const ancestors=[];for(let p=node.parentNode;p;p=p.parentNode)ancestors.unshift(p);ancestors.unshift(node.ownerDocument);
  const e={type,target:node,stopped:false,stopPropagation(){this.stopped=true;}};
  const invoke=(n,cap)=>{for(const {fn}of [...(n.listeners.get(type)||[])].filter(x=>x.cap===cap))fn(e);};
  for(const n of ancestors){invoke(n,true);if(e.stopped)return;}
  invoke(node,true);invoke(node,false);if(!bubbles||e.stopped)return;
  for(const n of ancestors.reverse()){invoke(n,false);if(e.stopped)return;}
}
class Element extends Target {
  constructor(document,tag){super();Object.assign(this,{ownerDocument:document,tagName:tag,children:[],parentNode:null,attrs:new Map(),className:'',value:'',hidden:false,clientWidth:100,scrollWidth:300});this.classList={add:c=>{this.className+=' '+c;}};}
  append(...nodes){for(const n of nodes){if(n.tagName==='#fragment'){this.append(...n.children);continue;}n.remove();n.parentNode=this;this.children.push(n);this.ownerDocument.writes++;}}
  replaceChildren(...nodes){for(const n of [...this.children])n.remove();this.value='';this.append(...nodes);this.ownerDocument.writes++;}
  remove(){if(this.parentNode){if(this.contains(this.ownerDocument.activeElement))this.ownerDocument.activeElement=this.ownerDocument.body;this.parentNode.children=this.parentNode.children.filter(n=>n!==this);this.ownerDocument.writes++;}this.parentNode=null;}
  set textContent(t){assert.equal(typeof t,'string');this.replaceChildren();this.value=t;this.ownerDocument.writes++;}
  get textContent(){return this.value+this.children.map(n=>n.textContent).join('');}
  setAttribute(k,v){this.attrs.set(k,String(v));this.ownerDocument.writes++;}getAttribute(k){return this.attrs.get(k)??null;}removeAttribute(k){this.attrs.delete(k);this.ownerDocument.writes++;}
  contains(n){return this===n||this.children.some(c=>c.contains(n));}
  get isConnected(){if(this.className==='nav-md'&&this.ownerDocument.refuseLines)return false;return this===this.ownerDocument.body||Boolean(this.parentNode?.isConnected);}
  getClientRects(){return this.isConnected&&visible(this)?[{}]:[];}
  click(){this.emit('click');}
  blur(){const d=this.ownerDocument;if(d.activeElement!==this)return;d.activeElement=d.body;dispatch(this,'blur',false);dispatch(this,'focusout');}
  focus(){const d=this.ownerDocument;if(d.activeElement===this)return;const old=d.activeElement;d.activeElement=d.body;if(old){dispatch(old,'blur',false);dispatch(old,'focusout');}d.activeElement=this;d.focusLog.push(this);dispatch(this,'focus',false);dispatch(this,'focusin');d.focusHook(this);}
}
const walk=n=>[n,...n.children.flatMap(walk)];
const has=(n,c)=>n.className.split(' ').includes(c);
const visible=n=>{for(;n;n=n.parentNode)if(n.hidden)return false;return true;};
const tick=async()=>{for(let i=0;i<20;i++)await Promise.resolve();};
const fixtures=new Set();afterEach(()=>{for(const f of [...fixtures])f.clean();});
function fixture(data=movedSnapshot(),extra={}) {
  const window=new Target(),nodes=[],frames=new Map(),observers=[],copies=[],announcements=[],navigations=[];let hook=()=>{},serial=0;
  const document=Object.assign(new Target(),{defaultView:window,activeElement:null,writes:0,focusLog:[],focusHook:()=>{},fonts:new Target(),
    createElement(tag){const n=new Element(document,tag);nodes.push(n);return n;},createDocumentFragment(){return new Element(document,'#fragment');}});
  document.body=new Element(document,'body');document.activeElement=document.body;let fontsReady;document.fonts.ready=new Promise(r=>fontsReady=r);
  window.AbortController=AbortController;window.visualViewport=new Target();window.requestAnimationFrame=fn=>{const id=++serial;frames.set(id,fn);return id;};window.cancelAnimationFrame=id=>frames.delete(id);
  window.ResizeObserver=class {constructor(fn){this.callback=fn;this.disconnected=false;this.disconnects=0;observers.push(this);hook('constructor',this);}observe(){hook('observe',this);}disconnect(){this.disconnects++;this.disconnected=true;}};
  const options={window,idPrefix:'movedView',expectedScope:scope(),limits:limits(),copyText:t=>{copies.push(t);},announce:t=>{announcements.push(t);},onConversation:v=>{navigations.push(v);},...extra};
  const view=createAsksView(document,data,options);
  const f={view,options,window,document,nodes,frames,observers,copies,announcements,navigations,
    find:c=>walk(document.body).filter(n=>has(n,c)),hook:fn=>hook=fn,
    click:(text,index=0)=>walk(document.body).filter(n=>n.tagName==='button'&&n.textContent===text)[index].click(),
    control:id=>walk(document.body).find(n=>has(n,'nav-asks-moved-button')&&n.textContent.startsWith('Ask '+id+':')),
    heading:id=>walk(document.body).find(n=>has(n,'nav-asks-item-heading')&&n.textContent==='Ask '+id),
    group:index=>walk(document.body).find(n=>n.id==='movedView-group-'+index),
    mount(){document.body.append(view.fragment);assert.equal(view.mount(),true);},
    flush(){const pending=[...frames.values()];frames.clear();for(const fn of pending)fn();},
    clean(){hook=()=>{};document.focusHook=()=>{};view.teardown();view.teardown();fixtures.delete(f);fontsReady();assert.equal(document.count()+window.count()+window.visualViewport.count()+document.fonts.count()+frames.size+nodes.reduce((n,x)=>n+x.count(),0),0);assert.ok(observers.every(o=>o.disconnected));}};
  fixtures.add(f);return f;
}
const targetFocus=(f,id)=>f.document.focusLog.filter(n=>has(n,'nav-asks-moved-target')&&n.textContent==='Ask '+id).length;
function activate(f,id='5'){const control=f.control(id);control.focus();control.click();return control;}

test('owned complete overview has local-day/counts and native targets without another quote renderer',()=>{
  const data=movedSnapshot(),f=fixture(data);data.moved_today.ids.reverse();data.sections[4].items[0].state_text='changed';f.mount();
  assert.equal(f.find('nav-asks-moved-heading')[0].textContent,'Moved today');assert.equal(f.find('nav-asks-moved-day')[0].textContent,'Local day: 1970-01-01');assert.equal(f.find('nav-asks-moved-counts')[0].textContent,'1 closed, 1 probably done');
  assert.deepEqual(f.find('nav-asks-moved-button').map(n=>n.textContent),['Ask 5: closed: stored decision','Ask 4: probably-done: stored explanation']);
  assert.equal(f.control('5').getAttribute('aria-controls'),f.heading('5').id);assert.equal(f.heading('5').getAttribute('tabindex'),'-1');assert.equal(f.find('nav-md').length,3);const legacy=movedSnapshot();delete legacy.moved_today;const baseline=fixture(legacy);baseline.mount();assert.equal(f.observers.length,baseline.observers.length);baseline.clean();
  assert.equal(f.group(3).hidden,true);assert.equal(f.group(4).hidden,true);assert.equal(f.document.focusLog.length,0);
});
test('absent versus supplied empty overview does not manufacture zero',()=>{
  const d=movedSnapshot();delete d.moved_today;const old=fixture(d);old.mount();assert.equal(old.find('nav-asks-moved').length,0);
  const empty=movedSnapshot();for(const s of empty.sections)s.items=[];empty.moved_today.ids=[];const f=fixture(empty);f.mount();assert.equal(f.find('nav-asks-moved-counts')[0].textContent,'0 closed, 0 probably done');assert.equal(f.find('nav-asks-moved-button').length,0);
});
test('detached and disposed controls cannot reveal/focus; normal action opens existing group once',()=>{
  const f=fixture(),control=walk(f.view.fragment).find(n=>has(n,'nav-asks-moved-button'));assert.equal(f.view.mount(),false);control.click();assert.equal(f.document.focusLog.length,0);f.mount();activate(f);
  assert.equal(f.group(4).hidden,false);assert.equal(f.document.activeElement,f.heading('5'));assert.equal(targetFocus(f,'5'),1);assert.equal(f.document.count(),0);assert.equal(f.control('5').getAttribute('aria-disabled'),'false');
  f.view.teardown();const n=f.document.focusLog.length;control.click();assert.equal(f.document.focusLog.length,n);f.clean();
});
test('already expanded target preserves Latest wording, copy and exact source pair',async()=>{
  const f=fixture();f.mount();activate(f);const article=f.heading('5').parentNode;walk(article).find(n=>n.tagName==='button'&&n.textContent==='Show details').click();const helpers=f.observers.length;activate(f);
  assert.equal(f.group(4).hidden,false);assert.equal(f.observers.length,helpers);assert.equal(walk(article).find(n=>has(n,'nav-asks-wording')).textContent,'Latest wording');
  walk(article).find(n=>n.tagName==='button'&&n.textContent==='Copy quote').click();await tick();assert.deepEqual(f.copies,[latestCopy]);walk(article).find(n=>n.tagName==='button'&&n.textContent==='Open conversation').click();assert.deepEqual(f.navigations,[{provider:'agy',id:sourceID}]);
});
test('neighbor, leave-return, blur-body and stopped bubbling revoke monotonically',()=>{
  for(const mode of ['neighbor','return','blur','stopped-return']){
    const f=fixture();f.mount();f.flush();const initial=f.control('5'),neighbor=f.document.createElement('button');f.document.body.append(neighbor);
    const stop=e=>e.stopPropagation();if(mode==='stopped-return'){initial.addEventListener('focusout',stop);neighbor.addEventListener('focusin',stop);}
    let once=false;f.hook(where=>{if(where!=='observe'||once)return;once=true;assert.equal(f.document.count(),3);if(mode==='blur')initial.blur();else{neighbor.focus();if(mode!=='neighbor')initial.focus();}});
    activate(f);assert.ok(once);assert.equal(targetFocus(f,'5'),0);assert.equal(f.document.count(),0);assert.equal(f.document.activeElement,mode==='neighbor'?neighbor:mode==='blur'?f.document.body:initial);assert.equal(f.group(4).hidden,false);
    if(mode==='stopped-return'){initial.removeEventListener('focusout',stop);neighbor.removeEventListener('focusin',stop);}f.hook(()=>{});activate(f);assert.equal(targetFocus(f,'5'),1);f.clean();
  }
});
test('eventless initiating-control removal and retained detached click cannot acquire focus',()=>{
  const f=fixture();f.mount();const initial=f.control('5');let once=false;f.hook(where=>{if(where==='observe'&&!once){once=true;initial.remove();}});initial.focus();initial.click();assert.ok(once);assert.equal(f.document.activeElement,f.document.body);assert.equal(targetFocus(f,'5'),0);assert.equal(f.document.count(),0);initial.click();assert.equal(targetFocus(f,'5'),0);
});
test('competing control activation revokes even when focus never changes',()=>{
  const f=fixture();f.mount();let once=false;f.hook(where=>{if(where==='observe'&&!once){once=true;f.control('4').click();}});activate(f);assert.ok(once);assert.equal(targetFocus(f,'5')+targetFocus(f,'4'),0);assert.equal(f.document.count(),0);f.hook(()=>{});activate(f,'4');assert.equal(targetFocus(f,'4'),1);
});
test('actual helper failure, false mount and disposal remove all tracking with zero stale focus',()=>{
  for(const mode of ['throw','false','dispose']){
    const f=fixture();f.mount();let once=false;const control=f.control('5');if(mode==='false')f.document.refuseLines=true;
    f.hook(where=>{if(where==='observe'&&!once){once=true;if(mode==='throw')throw Error('invented');if(mode==='dispose')f.view.teardown();}});control.focus();control.click();assert.equal(targetFocus(f,'5'),0);assert.equal(f.document.count(),0);
    if(mode!=='dispose')assert.equal(f.document.body.textContent,'AsksAsks unavailable');else assert.equal(f.find('nav-asks').length,0);f.clean();
  }
});
test('heading-focus reentry leaves neighbor untouched and no old-operation DOM writes afterward',()=>{
  const f=fixture();f.mount();const neighbor=f.document.createElement('button');f.document.body.append(neighbor);let after=null;
  f.document.focusHook=n=>{if(has(n,'nav-asks-moved-target')){f.view.teardown();neighbor.focus();after=f.document.writes;}};activate(f);assert.notEqual(after,null);assert.equal(f.document.writes,after);assert.equal(f.document.activeElement,neighbor);assert.equal(f.document.count(),0);
});
test('partial capture-listener setup failure removes attempted and acquired registrations',()=>{
  for(const site of [1,2,3])for(const after of [false,true]){
    const f=fixture();f.mount();const add=f.document.addEventListener.bind(f.document);let count=0;f.document.addEventListener=(...args)=>{count++;if(count===site&&!after)throw Error('invented setup');add(...args);if(count===site&&after)throw Error('invented setup');};activate(f);assert.equal(f.document.count(),0);assert.equal(targetFocus(f,'5'),0);assert.equal(f.document.body.textContent,'AsksAsks unavailable');f.clean();
  }
});
test('setup reentry with a handle acquired after cancellation cleans exactly the old registrations',()=>{
  const f=fixture();f.mount();const add=f.document.addEventListener.bind(f.document);let once=false;f.document.addEventListener=(...args)=>{if(!once){once=true;f.control('4').focus();f.control('4').click();}add(...args);};activate(f);assert.ok(once);assert.equal(targetFocus(f,'5'),0);assert.equal(targetFocus(f,'4'),1);assert.equal(f.document.activeElement,f.heading('4'));assert.equal(f.document.count(),0);
});
test('repeated expanded/collapsed reveals return document resources to baseline',()=>{
  const f=fixture();f.mount();for(let i=0;i<8;i++){activate(f);assert.equal(f.document.count(),0);assert.equal(f.document.activeElement,f.heading('5'));f.click('Closed and dismissed (1)');assert.equal(f.group(4).hidden,true);}assert.equal(targetFocus(f,'5'),8);
});
test('two instances retain unique ordinal targets and neighbor focus on disposal',()=>{
  const f=fixture();f.mount();const other=createAsksView(f.document,movedSnapshot(),{...f.options,idPrefix:'neighborView'});f.document.body.append(other.fragment);assert.equal(other.mount(),true);const ids=walk(f.document.body).filter(n=>n.id).map(n=>n.id);assert.equal(ids.length,new Set(ids).size);
  const neighbor=walk(f.document.body).find(n=>n.id==='neighborView-moved-heading').parentNode;const button=walk(neighbor).find(n=>n.tagName==='button');button.focus();const old=f.control('5');f.view.teardown();old.click();assert.equal(f.document.activeElement,button);assert.equal(neighbor.isConnected,true);assert.equal(f.document.count(),0);other.teardown();f.clean();
});
test('actual loading controller keeps hidden unpublished overview inert before complete publication',async()=>{
  const f=fixture();let attempts=0;const controller=createAsksController(f.document,{...f.options,idPrefix:'parentView',readAsks:()=>Promise.resolve({data:movedSnapshot()}),announce:message=>{if(message==='Asks updated'){attempts++;assert.equal(f.find('nav-asks-candidate')[0].hidden,true);f.control('5').click();assert.equal(targetFocus(f,'5'),0);assert.equal(f.document.count(),0);}}});
  f.document.body.append(controller.fragment);assert.equal(controller.mount(),true);controller.loadScope(scope());await tick();assert.equal(attempts,1);assert.equal(f.find('nav-asks-candidate')[0].hidden,false);activate(f);assert.equal(targetFocus(f,'5'),1);controller.teardown();f.clean();
});
