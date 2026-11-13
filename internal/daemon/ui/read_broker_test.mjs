import test from "node:test";
import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
const source=await readFile(new URL("./read_broker.js",import.meta.url),"utf8");
const {createReadBroker}=await import("data:text/javascript;base64,"+Buffer.from(source).toString("base64"));
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const list={dir:"/invented",workers:false},detail={provider:"claude",id:"a0000000-0000-4000-8000-000000000001",last:50,markup:false,anchor:0};
const caught=p=>p.then(value=>({value}),()=>({failed:true}));
function fixture() {
 const calls=[];let active=0,max=0;
 const read=(query,signal)=>new Promise((resolve,reject)=>{
  active++;max=Math.max(max,active);
  calls.push({query,signal,resolve:value=>{active--;resolve(value);},reject:()=>{active--;reject(Error("invented"));}});
 });
 return {calls,broker:createReadBroker({sessions:read,conversation:read,asks:read,placement:read}),max:()=>max};
}
test("list and detail remain current and complete fairly",async()=>{
 const f=fixture(),r=f.broker.route();
 const a=r.read("sessions",list),b=r.read("conversation",detail);await tick();
 assert.equal(f.calls.length,1);assert.equal(f.calls[0].signal.aborted,false);
 f.calls[0].resolve("list");assert.equal(await a,"list");await tick();
 assert.equal(f.calls.length,2);f.calls[1].resolve("detail");assert.equal(await b,"detail");assert.equal(f.max(),1);f.broker.close();
});
test("retired physical cleanup holds new route list and detail",async()=>{
 const f=fixture(),a=f.broker.route();const old=caught(a.read("sessions",list));await tick();
 const b=f.broker.route(),nextList=b.read("sessions",list),nextDetail=b.read("conversation",detail);await tick();
 assert.equal(f.calls[0].signal.aborted,true);assert.equal(f.calls.length,1);
 let settled=false;old.then(()=>{settled=true;});await tick();assert.equal(settled,false);
 f.calls[0].resolve("obsolete");assert.deepEqual(await old,{failed:true});await tick();
 f.calls[1].resolve("list");assert.equal(await nextList,"list");await tick();
 f.calls[2].resolve("detail");assert.equal(await nextDetail,"detail");assert.equal(f.max(),1);f.broker.close();
});
test("same-channel replacement keeps latest, preserves other channel",async()=>{
 const f=fixture(),r=f.broker.route();const first=caught(r.read("sessions",list));await tick();
 const previous=caught(r.read("sessions",{...list,dir:"/old"}));
 const other=r.read("conversation",detail),latest=r.read("sessions",{...list,dir:"/latest"});
 assert.deepEqual(await previous,{failed:true});assert.equal(f.calls[0].signal.aborted,true);
 f.calls[0].reject();assert.deepEqual(await first,{failed:true});await tick();
 assert.equal(f.calls[1].query.provider,"claude");f.calls[1].resolve("conversation");await other;await tick();
 assert.equal(f.calls[2].query.dir,"/latest");f.calls[2].resolve("latest");await latest;assert.equal(f.max(),1);f.broker.close();
});
test("caller abort waits physical settlement and advances queued work",async()=>{
 const f=fixture(),r=f.broker.route(),controller=new AbortController();
 const a=caught(r.read("sessions",list,controller.signal)),b=r.read("conversation",detail);await tick();controller.abort();
 let settled=false;a.then(()=>{settled=true;});await tick();assert.equal(settled,false);assert.equal(f.calls.length,1);
 f.calls[0].resolve("retired");assert.deepEqual(await a,{failed:true});await tick();f.calls[1].resolve("current");assert.equal(await b,"current");f.broker.close();
});
test("close rejects bounded queue, cannot release held physical slot early",async()=>{
 const f=fixture(),r=f.broker.route();const a=caught(r.read("sessions",list));await tick();
 const b=caught(r.read("conversation",detail)),c=caught(r.read("asks",{mode:"all"})),d=caught(r.read("placement",{askId:"1"}));
 f.broker.close();assert.deepEqual(await b,{failed:true});assert.deepEqual(await c,{failed:true});assert.deepEqual(await d,{failed:true});
 assert.equal(f.calls[0].signal.aborted,true);assert.equal(f.calls.length,1);f.calls[0].resolve("closed");assert.deepEqual(await a,{failed:true});
 await assert.rejects(f.broker.route().read("sessions",list));
});
test("abort reentry cannot retire replacement route",async()=>{
 const f=fixture(),a=f.broker.route();const old=caught(a.read("sessions",list));await tick();
 let current,latest;f.calls[0].signal.addEventListener("abort",()=>{current=f.broker.route();latest=current.read("asks",{mode:"all"});});
 const stale=f.broker.route();await assert.rejects(stale.read("sessions",list));
 f.calls[0].reject();await old;await tick();assert.equal(f.calls.length,2);f.calls[1].resolve("replacement");assert.equal(await latest,"replacement");stale.dispose();assert.equal(f.calls[1].signal.aborted,false);f.broker.close();
});
test("synchronous throws and hostile then access release once",async()=>{
 for(const method of [()=>{throw Error("invented");},()=>Object.defineProperty({},"then",{get(){throw Error("invented");}})]) {
  let calls=0;const b=createReadBroker({sessions:method,asks:()=>{calls++;return {data:"ok"};}}),r=b.route();
  const a=caught(r.read("sessions",list)),next=r.read("asks",{mode:"all"});assert.deepEqual(await a,{failed:true});assert.deepEqual(await next,{data:"ok"});assert.equal(calls,1);b.close();
 }
});
test("queued query is owned and unsupported capability never reads",async()=>{
 const f=fixture(),r=f.broker.route();const query={...list};const a=r.read("sessions",query);query.dir="/mutated";await tick();assert.equal(f.calls[0].query.dir,"/invented");
 await assert.rejects(r.read("arbitrary",{}));await assert.rejects(r.read("conversation",Object.defineProperty({},"provider",{get(){throw Error("do not read");}})));
 f.calls[0].resolve("ok");await a;f.broker.close();
 const b=createReadBroker({sessions:()=>"ok"});await assert.rejects(b.route().read("placement",{askId:"1"}));b.close();
});
