import {createAsksView} from './asks_view.js';
import {createAsksController} from './asks_controller.js';
import {scope,limits} from './asks_values.mjs';
import {movedSnapshot} from './asks_moved_values.mjs';

// Invented component-only native event probes; no backend or OS clipboard.
const fixture=document.getElementById('fixture'),copies=[],announcements=[],navigations=[],targets=[],records=[],tracking=new Map();
const add=document.addEventListener.bind(document),remove=document.removeEventListener.bind(document);
document.addEventListener=(type,fn,capture)=>{add(type,fn,capture);if(capture===true&&['focusin','focusout','blur'].includes(type)){if(!tracking.has(type))tracking.set(type,new Set());tracking.get(type).add(fn);}};
document.removeEventListener=(type,fn,capture)=>{remove(type,fn,capture);if(capture===true)tracking.get(type)?.delete(fn);};
const tracked=()=>[...tracking.values()].reduce((n,a)=>n+a.size,0);
const options={window,expectedScope:scope(),limits:limits(),copyText:text=>{copies.push(text);},announce:text=>{announcements.push(text);},onConversation:value=>{navigations.push(value);}};
let first,second,armed=null,retained=null;
const nativeObserver=window.ResizeObserver;
window.ResizeObserver=class extends nativeObserver {
  observe(node,options){
    super.observe(node,options);
    if(!armed||!node.closest('#firstMovedRoot'))return;
    const mode=armed;armed=null;
    const trigger=document.querySelector('#firstMovedRoot .nav-asks-moved-button'),neighbor=document.getElementById('neighbor');
    retained=trigger;const record={mode,tracking:tracked(),before:targets.length};
    const stop=e=>e.stopPropagation();
    if(mode==='stopped-return'){trigger.addEventListener('focusout',stop);neighbor.addEventListener('focusin',stop);}
    if(mode==='blur')trigger.blur();
    if(['neighbor','return','stopped-return'].includes(mode)){neighbor.focus();if(mode!=='neighbor')trigger.focus();}
    if(mode==='remove')trigger.remove();
    if(mode==='compete')document.querySelectorAll('#firstMovedRoot .nav-asks-moved-button')[1].click();
    if(mode==='dispose')first.teardown();
    if(mode==='stopped-return'){trigger.removeEventListener('focusout',stop);neighbor.removeEventListener('focusin',stop);}
    Object.assign(record,{after:targets.length,active:document.activeElement.id||document.activeElement.tagName,triggerConnected:trigger.isConnected});records.push(record);
    if(mode==='throw')throw Error('invented activation failure');
  }
};
function attach(idPrefix,data=movedSnapshot()) {
  const view=createAsksView(document,data,{...options,idPrefix});fixture.append(view.fragment);
  const root=fixture.lastElementChild;root.id=idPrefix+'Root';
  root.addEventListener('focusin',event=>{if(event.target.classList.contains('nav-asks-moved-target'))targets.push(event.target.id);},true);
  if(!view.mount())throw Error('invented moved view mount failed');return view;
}
first=attach('firstMoved');second=attach('secondMoved');
function reset(data=movedSnapshot()){armed=null;first.teardown();first=attach('firstMoved',data);api.first=first;}
function arm(mode){armed=mode;}
function attachController(){
  const controller=createAsksController(document,{...options,idPrefix:'parentMoved',readAsks:()=>Promise.resolve({data:movedSnapshot()}),announce:message=>{
    announcements.push(message);if(message==='Asks updated'){
      const root=document.querySelector('.nav-asks-candidate'),before=document.activeElement;
      root.querySelector('.nav-asks-moved-button').click();records.push({mode:'unpublished',hidden:root.hidden,sameFocus:document.activeElement===before,tracking:tracked()});
    }
  }});fixture.append(controller.fragment);if(!controller.mount())throw Error('invented parent mount failed');controller.loadScope(scope());return controller;
}
const api={first,second,reset,arm,options,copies,announcements,navigations,targets,records,tracked,clickRetained:()=>retained?.click(),attachController,movedSnapshot};
window.asksMovedFixture=api;
