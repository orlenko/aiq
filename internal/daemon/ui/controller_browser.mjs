import {createSessionController} from './session_controller.js';
import {scope,limits,query,row,sessions,detail} from './controller_values.mjs';

// Public invented component fixture; no fetch, clipboard, storage or globals.
export const observations={requests:[],announcements:[],copied:[]};
const pending=[];
const controllers=[];
function make(host,prefix) {
  const controller=createSessionController(document,{window,idPrefix:prefix,limits:limits(),detailQuery:{last:50,markup:false,anchor:0},
    readSessions:q=>{
      observations.requests.push({type:'list',...q});
      return Promise.resolve({data:sessions('/fixture/A',[row('A'),row('B',{label:'<img src=x onerror=alert(1)> long label '+('wide '.repeat(30))}),row('A',{label:'duplicate A'}),row('B',{id:'unsupported-short',label:'unavailable row'})])});
    },
    readConversation:(q,signal)=>{
      observations.requests.push({type:'detail',...q});
      // Deliberately ignore abort; the controller must reject stale delivery.
      return new Promise((resolve,reject)=>pending.push({q,signal,resolve,reject}));
    },
    copyText:raw=>{observations.copied.push(raw);return Promise.resolve();},
    announce:text=>observations.announcements.push(text),
  });
  host.append(controller.fragment);controller.mount();controllers.push(controller);return controller;
}
export const first=make(document.getElementById('fixture-first'),'firstController');
export function deliver(name) {
  const chosen=pending.findIndex(item=>item.q.id===scope(name).id);
  if(chosen<0)return false;
  const [item]=pending.splice(chosen,1),data=detail(name,2);
  data.messages[0].groups[0].lines[0].spans[0].t='<script>inert fixture</script> '+('long painted text '.repeat(40));
  item.resolve({data});return true;
}
export function disposeAll(){for(const controller of controllers)controller.teardown();}
document.getElementById('fixture-load').addEventListener('click',()=>first.loadSessions(query()));
document.getElementById('fixture-deliver-a').addEventListener('click',()=>deliver('A'));
document.getElementById('fixture-deliver-b').addEventListener('click',()=>deliver('B'));
let second;
document.getElementById('fixture-neighbor').addEventListener('click',()=>{
  if(!second){second=make(document.getElementById('fixture-second'),'secondController');second.loadSessions(query());}
});
document.getElementById('fixture-dispose').addEventListener('click',()=>first.teardown());
