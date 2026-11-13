import test from "node:test";
import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {createTabSession} from "./auth_session.js";
import {route} from "./auth_codec.js";
const build="invented+outcomes",base="http://127.0.0.1:54321",bearer="A".repeat(43),key="aiq.navigator.v1:"+build;
function deferred(){let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return{promise,resolve,reject};}
function setup(extra={}) {
 const values=new Map([[key,bearer]]),events=[];
 const storage={getItem:k=>values.get(k)??null,setItem:(k,v)=>values.set(k,v),removeItem:k=>{events.push("storage-clear");values.delete(k);}};
 const s=createTabSession({base,build,fragment:"",isCurrent:()=>true,getStorage:()=>storage,
  limits:{sessions:1024,conversation:1024,asks:1024},fetcher:async()=>response("{}"),onState:v=>events.push(v.kind),...extra});
 return {s,values,events};
}
const response=(body,status=200,headers={})=>new Response(body,{status,headers:{"Content-Type":"application/json","X-Aiq-Build":build,...headers}});
const query={mode:"all"};
for(const mode of ["caller-abort","expired"]) test("observed 401 clears before "+mode+" suppression",async()=>{
  const waiting=deferred(),parent=new AbortController();let now=0;
  const x=setup({fetcher:()=>waiting.promise,now:()=>now});await x.s.ready;
  const request=x.s.asks(query,parent.signal);const rejected=assert.rejects(request,/Navigator request unavailable/);
  if(mode==="caller-abort")parent.abort();else now=10000;
  waiting.resolve(response("invented forbidden body",401,{"X-Aiq-Build":"wrong"}));await rejected;
  assert.equal(x.s.snapshot().kind,"locked",mode);assert.equal(x.values.has(key),false,mode);x.s.dispose();
});
test("old auth epoch 401 cannot erase successor tab state",async()=>{
 const waiting=deferred();const x=setup({fetcher:()=>waiting.promise});await x.s.ready;
 const request=assert.rejects(x.s.asks(query));x.s.forget();x.values.set(key,"invented successor storage");
 waiting.resolve(response("{}",401));await request;assert.equal(x.values.get(key),"invented successor storage");x.s.dispose();
});
test("request failures allow manual retry without exposing raw causes",async()=>{
 for(const status of [400,404,409,413,500,503]){
  let failed=true,calls=0;const kinds=[];
  const x=setup({fetcher:async()=>{calls++;return failed?response("invented raw private error",status):response("{}");},onReadFailure:k=>kinds.push(k)});await x.s.ready;
  await assert.rejects(x.s.asks(query),e=>e.message==="Navigator request unavailable");
  assert.equal(x.s.snapshot().kind,"restoring");assert.deepEqual(kinds,["request"]);
  failed=false;assert.deepEqual(await x.s.asks(query),{data:{}});assert.equal(x.s.snapshot().kind,"unlocked");assert.equal(calls,2);x.s.dispose();
 }
});
test("network, build, encoding and JSON failures permanently retire data reads",async()=>{
 for(const mode of ["network","build","encoding","json"]){
  let calls=0;const x=setup({fetcher:async()=>{calls++;if(mode==="network")throw Error("invented raw secret");return mode==="build"?response("{}",200,{"X-Aiq-Build":"wrong"}):mode==="encoding"?response("{}",200,{"Content-Encoding":"gzip"}):response("{");}});await x.s.ready;
  assert.equal(x.s.attachHost(()=>x.events.push("detached")),true);
  await assert.rejects(x.s.asks(query),e=>e.message==="Navigator request unavailable");
  assert.equal(x.s.snapshot().kind,"terminal");assert.equal(x.events.filter(v=>v==="detached").length,1);
  assert.equal(x.values.get(key),bearer);await assert.rejects(x.s.asks(query));assert.equal(calls,1);
  x.s.forget();assert.equal(x.values.size,0);x.s.dispose();
 }
});
test("sensitive host detaches before abort callouts and state publication",async()=>{
 const waiting=deferred();let x;
 x=setup({fetcher:(url,options)=>{options.signal.addEventListener("abort",()=>x.events.push("abort"));return waiting.promise;}});await x.s.ready;
 x.s.attachHost(()=>{x.events.push("detached");x.s.forget();});const request=assert.rejects(x.s.asks(query));
 x.s.forget();assert(x.events.indexOf("detached")<x.events.indexOf("abort"));assert(x.events.indexOf("storage-clear")<x.events.lastIndexOf("locked"));
 waiting.resolve(response("{}"));await request;assert.equal(x.events.filter(v=>v==="detached").length,1);x.s.dispose();
});
test("cleanup failure and dispose reentry do not skip independent abort",async()=>{
 const waiting=deferred();let x,aborts=0;
 x=setup({fetcher:(url,options)=>{options.signal.addEventListener("abort",()=>aborts++);return waiting.promise;}});await x.s.ready;
 x.s.attachHost(()=>{x.s.dispose();throw Error("invented cleanup");});const request=assert.rejects(x.s.asks(query));x.s.forget();
 waiting.resolve(response("{}"));await request;assert.equal(aborts,1);assert.equal(x.s.snapshot().kind,"disposed");assert.equal(x.values.size,0);
});
test("caller-only cancellation keeps current auth owner usable",async()=>{
 const waiting=deferred(),controller=new AbortController();let calls=0;const kinds=[];
 const x=setup({fetcher:()=>++calls===1?waiting.promise:Promise.resolve(response("{}")),onReadFailure:k=>kinds.push(k)});await x.s.ready;
 const request=assert.rejects(x.s.asks(query,controller.signal));controller.abort();waiting.reject(Error("invented abort"));await request;
 assert.equal(x.s.snapshot().kind,"restoring");assert.deepEqual(kinds,["cancelled"]);assert.deepEqual(await x.s.asks(query),{data:{}});x.s.dispose();
});
test("shared app route literals preserve exact prompt anchors",()=>{
 const rows=JSON.parse(readFileSync(new URL("./app_route_vectors.json",import.meta.url),"utf8"));assert.equal(rows.length,12);
 for(const row of rows){let ok=true;try{route(row.route)}catch{ok=false}assert.equal(ok,row.ok,row.route);}
 assert.equal(route(rows[2].route).anchor,100123);
});
test("placement is a separately configured fixed-path capability",async()=>{
 let calls=0,url="";const x=setup({limits:{sessions:1024,conversation:1024,asks:1024,placement:128},fetcher:async u=>{calls++;url=u;return response('{"anchor":100123}');}});await x.s.ready;
 assert.deepEqual(await x.s.placement({askId:"9223372036854775807"}),{data:{anchor:100123}});assert.equal(url,base+"/api/nav/asks/9223372036854775807/turn");
 for(const id of ["01","0","9223372036854775808","1/turn?x=y",1])await assert.rejects(x.s.placement({askId:id}));
 assert.equal(calls,1);x.s.dispose();
 const y=setup({fetcher:async()=>{throw Error("must not read");}});await y.s.ready;await assert.rejects(y.s.placement({askId:"1"}));assert.equal(y.s.snapshot().kind,"restoring");y.s.dispose();
});
