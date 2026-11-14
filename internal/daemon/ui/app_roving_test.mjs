import test from 'node:test';
import assert from 'node:assert/strict';
import {setNavigatorRoving,bindNavigatorRoving} from './app.js';
function control(extra={}){return{tagName:'BUTTON',tabIndex:7,disabled:false,hidden:false,visible:true,aria:null,href:null,getAttribute(n){return n==='aria-disabled'?this.aria:null;},hasAttribute(n){return n==='href'&&this.href!==null;},getClientRects(){return this.visible?[{}]:[];},...extra};}
function row(controls=[],extra={}){return{hidden:false,dataset:{},querySelectorAll:()=>controls,contains:n=>controls.includes(n),...extra};}
const group=rows=>({querySelectorAll:()=>rows});

test('unavailable first and intermediate rows cannot remove valid tab stops',()=>{
 for(const unavailable of [[],[control({disabled:true})],[control({visible:false})],[control({hidden:true})],[control({tagName:'A'})]]){
  const b=control(),c=control(),rows=[row(unavailable),row([b]),row([]),row([c])],entries=setNavigatorRoving(group(rows),null);
  assert.equal(entries.length,2);assert.equal(entries[0].controls[0],b);assert.equal(entries[1].controls[0],c);assert.equal(b.tabIndex,0);assert.equal(c.tabIndex,-1);for(const x of unavailable)assert.equal(x.tabIndex,-1);
 }
});
test('focused eligible row wins over stale current and only usable controls enter Tab order',()=>{
 const dead=control({disabled:true}),b=control(),c=control(),rows=[row([dead],{dataset:{current:'true'}}),row([b,c])],g=group(rows);
 setNavigatorRoving(g,c);assert.equal(rows[0].dataset.current,'false');assert.equal(rows[1].dataset.current,'true');assert.equal(dead.tabIndex,-1);assert.equal(b.tabIndex,0);assert.equal(c.tabIndex,0);
});
test('hidden rows and closed-disclosure controls are skipped without stealing focus',()=>{
 const a=control(),hidden=control(),b=control(),rows=[row([a]),row([hidden],{hidden:true}),row([b])],g=group(rows);
 let entries=setNavigatorRoving(g,b);assert.deepEqual(entries.map(e=>e.row),[rows[0],rows[2]]);assert.equal(b.tabIndex,0);
 b.visible=false;entries=setNavigatorRoving(g,b);assert.equal(entries.length,1);assert.equal(a.tabIndex,0);assert.equal(b.tabIndex,-1);
 b.visible=true;setNavigatorRoving(g,null);assert.equal(a.tabIndex,0);assert.equal(b.tabIndex,-1);
});
test('no usable controls is a safe empty navigation set; links require href',()=>{
 const a=control({disabled:true}),link=control({tagName:'A',href:'/sessions'}),rows=[row([]),row([a]),row([link])];
 assert.equal(setNavigatorRoving(group(rows.slice(0,2)),null).length,0);assert.equal(a.tabIndex,-1);
 const entries=setNavigatorRoving(group(rows),null);assert.equal(entries.length,1);assert.equal(link.tabIndex,0);
});

test('ARIA busy controls keep their focusable entry point across asynchronous re-enable',()=>{
 const busy=control({aria:'true'}),rows=[row([busy])];
 assert.equal(setNavigatorRoving(group(rows),null).length,1);assert.equal(busy.tabIndex,0);
 busy.aria=null;assert.equal(busy.tabIndex,0);
});

test('disclosure activation restores row Tab order before focus leaves its heading',()=>{
 for(const activation of ['pointer click','native Enter click','native Space click']){
  const action=control({visible:false}),g=group([row([action])]),listeners=new Map(),heading={},content={ownerDocument:{activeElement:heading},querySelectorAll:()=>[g],addEventListener:(n,f)=>listeners.set(n,f),removeEventListener:n=>listeners.delete(n)};
  const clean=bindNavigatorRoving(content,()=>true);listeners.get('focusin')();assert.equal(action.tabIndex,-1,activation);
  action.visible=true;listeners.get('click')();assert.equal(action.tabIndex,0,activation);assert.equal(content.ownerDocument.activeElement,heading);
  action.visible=false;listeners.get('click')();assert.equal(action.tabIndex,-1);action.visible=true;listeners.get('click')();assert.equal(action.tabIndex,0);clean();assert.equal(listeners.size,0);
 }
});

test('retired visibility bindings cannot mutate a successor and remove both listeners',()=>{
 const action=control(),g=group([row([action])]),listeners=new Map();let current=true;
 const content={ownerDocument:{activeElement:null},querySelectorAll:()=>[g],addEventListener:(n,f)=>listeners.set(n,f),removeEventListener:n=>listeners.delete(n)};
 const clean=bindNavigatorRoving(content,()=>current),late=listeners.get('click');current=false;action.tabIndex=5;late();assert.equal(action.tabIndex,5);clean();assert.equal(listeners.size,0);late();assert.equal(action.tabIndex,5);
});
