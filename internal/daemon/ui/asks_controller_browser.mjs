import {createAsksController} from './asks_controller.js';
import {snapshot,scope,limits} from './asks_values.mjs';

// Invented controlled reads and an in-memory clipboard. Never connects a backend.
const calls=[],copies=[],announcements=[],navigations=[],publications=[];
function publishProbe() {} // Native debugger may pause this fixture-only callout.
const host=createAsksController(document,{window,idPrefix:'loadingFixture',limits:limits(),
  readAsks:(request,signal)=>new Promise((resolve,reject)=>calls.push({request,signal,resolve,reject})),
  copyText:text=>{copies.push(text);},onConversation:value=>navigations.push(value),
  announce:message=>{
    announcements.push(message);
    if(message!=='Asks updated')return;
    const candidate=document.querySelector('.nav-asks-candidate');
    publications.push({hidden:candidate.hidden,rects:candidate.getClientRects().length,
      rowsVisible:[...candidate.querySelectorAll('.nav-asks-item-heading')].some(n=>n.getClientRects().length>0)});
    const buttons=candidate.querySelectorAll('.nav-asks-item .nav-asks-button');buttons[1]?.click();buttons[2]?.click();
    publishProbe();
  }});
document.getElementById('fixture').append(host.fragment);
if(!host.mount())throw Error('invented loading fixture mount failed');
function resolve(index,known=false,totals=[3,7]) {
  const call=calls[index],data=snapshot();data.scope={...call.request};
  if(!known)data.report={known:false};
  if(totals){data.totals={utc_day:'1970-01-12',skipped:totals[0],calls_today:totals[1]};
    if(known)Object.assign(data.report,{skipped:totals[0],calls_today:totals[1]});}
  call.resolve({data});
}
window.asksLoadingFixture={host,calls,copies,announcements,navigations,publications,publishProbe,resolve,scope};
