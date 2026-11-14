import test from "node:test";
import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import vm from "node:vm";
import {rememberBody,signReply,until} from "./auth_test_proof.mjs";
import {canonical32,credential,encodeForm,fragment,route} from "./auth_codec.js";
import {createTabSession} from "./auth_session.js";

const build="invented+0123456789ab",base="http://127.0.0.1:54321",code="A".repeat(43),bearer="AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",key="aiq.navigator.v1:"+build;
const encoder=new TextEncoder();
function storage(seed) {
  const values=new Map(seed?[[key,seed]]:[]),calls=[];
  return {values,calls,getItem(k){calls.push("get");return values.get(k)??null;},setItem(k,v){calls.push("set");values.set(k,v);},removeItem(k){calls.push("remove");values.delete(k);}};
}
function reply(body,status=200,headers={},proofBody=body) {
  return rememberBody(new Response(body,{status,headers:{"Content-Type":"application/json","X-Aiq-Build":build,...headers}}),proofBody);
}
function setup(extra={}) {
  const saved=extra.saved??storage(),requests=[],states=[];
  let reads=0;const supplied=extra.fetcher;
  const session=createTabSession({base,build,fragment:"code="+code,isCurrent:()=>true,getStorage:()=>saved,
    limits:{sessions:128,conversation:128,asks:128},onState:s=>states.push(s),
    fetcher:async(url,options)=>{requests.push({url,options});return reply('{"credential":"'+bearer+'"}');},...extra,fetcher:async(url,options)=>{if(options.method==="GET")reads++;const r=await (supplied?supplied(url,options):(requests.push({url,options}),reply('{"credential":"'+bearer+'"}')));return signReply(r,options,bearer,build);}});
  return {session,saved,requests,states,readStarted:()=>until(()=>reads>0)};
}
function deferred(){let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return{promise,resolve,reject};}

test("closed canonical fragment and nested Go-compatible encoding",()=>{
  const next="/sessions?dir="+encodeForm("/invented/a + % /é".replace(" /","/"));
  const parsed=fragment("code="+code+"&next="+encodeForm(next));
  assert.equal(parsed.next.next,next);assert.equal(parsed.code,code);
  assert.equal(fragment("").code,null);assert.equal(route("/asks?all=true").all,true);
  for(const raw of ["code="+code+"&code="+code,"%63ode="+code,"code="+code.slice(0,42)+"B","code="+code+"%3D","code="+code+"&next=%FF","code="+code+"&unknown=x",null,"x".repeat(8193)])assert.throws(()=>fragment(raw));
  for(const value of ["http://foreign.invalid/","//foreign","/sessions/../asks","/sessions?dir=%2finvented","/asks?all=true&dir=%2Finvented","/sessions?all=true","/sessions?dir=%2Finvented&dir=%2Fother","/sessions?dir=%2Fa%00b","/sessions?dir=%2Fa%2F..%2Fb"])assert.throws(()=>route(value));
  assert(canonical32(code));assert(!canonical32(code.slice(0,42)+"B"));
});
test("shared literal route vectors match the Go page admission",()=>{
  const rows=JSON.parse(readFileSync(new URL("./auth_route_vectors.json",import.meta.url),"utf8"));assert.equal(rows.length,21);
  for(const row of rows){let ok=true;try{route(row.route)}catch{ok=false}assert.equal(ok,row.ok,row.route);}
});
test("credential raw grammar refuses substitution, duplicates and BOM",()=>{
  assert.equal(credential(encoder.encode('{ "credential" : "'+bearer+'" }\n')),bearer);
  for(const body of ['{"credential":null}','{"credential":"'+code+'","credential":"'+bearer+'"}','{"creden\\u0074ial":"'+bearer+'"}','{"credential":"'+bearer+'","extra":0}','{"credential":"\\ud800"}','{"credential":"'+bearer+'"} {}','\ufeff{"credential":"'+bearer+'"}','\u00a0{"credential":"'+bearer+'"}'])assert.throws(()=>credential(encoder.encode(body)));
  assert.throws(()=>credential(new Uint8Array([255])));
});
test("one exchange and private bounded fixed-route capability",async()=>{
  const x=setup();assert.equal(await x.session.ready,true);assert.equal(x.session.snapshot().kind,"unlocked");
  assert.equal(x.saved.values.get(key),bearer);assert.equal(x.requests.length,1);
  assert.equal(x.requests[0].url,base+"/api/nav/login");assert.deepEqual(JSON.parse(x.requests[0].options.body),{action:"exchange",code});
  assert.equal(x.requests[0].options.credentials,"omit");assert.equal(x.requests[0].options.redirect,"error");assert(!("Authorization"in x.requests[0].options.headers));
  assert(!JSON.stringify(x.session).includes(bearer));assert(!JSON.stringify(x.session.snapshot()).includes(bearer));
  x.session.dispose();
});
test("directory catalog uses the fixed proof-protected path and Sessions byte allowance",async()=>{
  const requests=[];const x=setup({fetcher:async(url,options)=>{requests.push({url,options});return options.method==='POST'?reply('{"credential":"'+bearer+'"}'):reply('{"source":"launch metadata","directories":[]}');}});
  assert.equal(await x.session.ready,true);assert.deepEqual(await x.session.directories({}),{data:{source:'launch metadata',directories:[]}});
  assert.equal(requests[1].url,base+'/api/nav/directories');assert.match(requests[1].options.headers.Authorization,/^AiqTab /);assert(!requests[1].options.headers.Authorization.includes(bearer));x.session.dispose();
});
test("restore is not an authenticated liveness claim; success confirms it",async()=>{
  const saved=storage(bearer),requests=[];
  const x=setup({saved,fragment:"",getStorage:()=>saved,fetcher:async(url,opts)=>{requests.push({url,opts});return reply('{"rows":[]}');}});
  assert.equal(await x.session.ready,true);assert.equal(x.session.snapshot().kind,"restoring");
  assert.deepEqual(await x.session.sessions({dir:"/invented",workers:false}),{data:{rows:[]}});
  assert.equal(x.session.snapshot().kind,"unlocked");assert.match(requests[0].opts.headers.Authorization,/^AiqTab /);assert(!requests[0].opts.headers.Authorization.includes(bearer));
  assert.equal(requests[0].url,base+"/api/nav/sessions?dir=%2Finvented&workers=0");x.session.dispose();
});
test("unconfigured readers send zero data requests",async()=>{
  const x=setup({limits:undefined});await x.session.ready;
  await assert.rejects(x.session.sessions({dir:"/invented",workers:false}),/Navigator request unavailable/);
  assert.equal(x.requests.length,1);x.session.dispose();
});
test("failed fresh link never restores old persistence; unknown clearing is truthful",async()=>{
  const saved=storage(bearer);saved.removeItem=()=>{throw Error("invented remove cause")};saved.setItem=()=>{throw Error("invented write cause")};
  const x=setup({getStorage:()=>saved,fetcher:async()=>reply("",401)});
  assert.equal(await x.session.ready,false);assert.equal(x.session.snapshot().kind,"locked");assert.equal(saved.values.get(key),bearer);
  assert.match(x.session.snapshot().notice,/may return on reload/);
  const later=setup({fragment:"",getStorage:()=>saved});assert.equal(await later.session.ready,true);assert.equal(later.session.snapshot().kind,"restoring");later.session.dispose();x.session.dispose();
});
test("approved blocked-storage fresh success is qualified memory-only",async()=>{
  const x=setup({getStorage:()=>{throw Error("invented denial")}});
  assert.equal(await x.session.ready,true);assert.equal(x.session.snapshot().kind,"unlocked");assert.match(x.session.snapshot().notice,/previously saved tab session/);
  x.session.forget();assert.equal(x.session.snapshot().kind,"locked");assert.match(x.session.snapshot().notice,/could not be cleared/);x.session.dispose();
});
test("verified empty memory fallback and confirmed forget differ from unknown",async()=>{
  const saved=storage();saved.setItem=()=>{throw Error("invented write denial")};
  const x=setup({getStorage:()=>saved});assert.equal(await x.session.ready,true);assert.match(x.session.snapshot().notice,/reload requires aiq ui/);
  x.session.forget();assert.equal(x.session.snapshot().kind,"locked");assert.match(x.session.snapshot().notice,/removed/);
  const y=setup({fragment:"",getStorage:()=>saved});assert.equal(await y.session.ready,false);y.session.dispose();x.session.dispose();
});
test("write-then-throw/readback admits the actual persistence outcome",async()=>{
  const saved=storage();saved.setItem=(k,v)=>{saved.values.set(k,v);throw Error("invented uncertain write")};
  const x=setup({getStorage:()=>saved});assert.equal(await x.session.ready,true);assert.equal(x.session.snapshot().notice,"");assert.equal(saved.values.get(key),bearer);x.session.dispose();
});
test("malformed/oversize fragments perform zero storage or exchange",async()=>{
  for(const value of [null,"code="+code+"&next="+encodeForm("//foreign"),"x".repeat(8193)]) {
    const saved=storage(bearer);let requests=0;
    const x=setup({fragment:value,getStorage:()=>saved,fetcher:async()=>{requests++;return reply("");}});
    assert.equal(await x.session.ready,false);assert.equal(requests,0);assert.equal(saved.calls.length,0);assert.equal(x.session.snapshot().kind,"locked");x.session.dispose();
  }
});
test("deadline rejects held headers without waiting for manual peer cleanup",async()=>{
  let timer,aborted=false;
  const x=setup({setTimer:fn=>{timer=fn;return 7},clearTimer:()=>{},fetcher:(_u,o)=>new Promise((_r,reject)=>o.signal.addEventListener("abort",()=>{aborted=true;reject(Error("invented"))},{once:true}))});
  timer();assert.equal(await x.session.ready,false);assert(aborted);assert.equal(x.session.snapshot().kind,"locked");x.session.dispose();
});
test("elapsed deadline is checked even when timer callback is delayed",async()=>{
  let clock=0;
  const x=setup({now:()=>clock,fetcher:async()=>{clock=10001;return reply('{"credential":"'+bearer+'"}');}});
  assert.equal(await x.session.ready,false);assert.equal(x.saved.values.has(key),false);x.session.dispose();
});
test("active401 locks before held body and clears persisted credential",async()=>{
  const saved=storage(bearer);let canceled=false;
  const x=setup({fragment:"",getStorage:()=>saved,fetcher:async()=>reply(new ReadableStream({pull(){},cancel(){canceled=true}}),401,{"X-Aiq-Build":"wrong-build"})});
  await x.session.ready;await assert.rejects(x.session.asks({mode:"all"}),/Navigator request unavailable/);
  assert.equal(x.session.snapshot().kind,"locked");assert.equal(saved.values.has(key),false);assert(canceled);x.session.dispose();
});
test("stale success and stale401 have zero authority after local forget",async()=>{
  for(const status of [200,401]) {
    const saved=storage(bearer),held=deferred();
    const x=setup({fragment:"",getStorage:()=>saved,fetcher:()=>held.promise});await x.session.ready;
    const request=x.session.asks({mode:"all"});await x.readStarted();x.session.forget();const state=x.session.snapshot();
    held.resolve(reply('{"fixture":true}',status));await assert.rejects(request,/Navigator request unavailable/);
    assert.deepEqual(x.session.snapshot(),state);assert.equal(saved.values.has(key),false);x.session.dispose();
  }
});
test("body exact/one-over and reader cancellation settle before rejection",async()=>{
  for(const size of [128,129]) {
    const saved=storage(bearer);let canceled=false;
    const body='{"x":"'+"a".repeat(size-8)+'"}';assert.equal(encoder.encode(body).length,size);
    const x=setup({fragment:"",getStorage:()=>saved,fetcher:async()=>reply(new ReadableStream({start(c){c.enqueue(encoder.encode(body));if(size===128)c.close()},cancel(){canceled=true}}),200,{},body)});await x.session.ready;
    if(size===128)assert.equal((await x.session.asks({mode:"all"})).data.x.length,120);
    else{await assert.rejects(x.session.asks({mode:"all"}));assert(canceled);}
    x.session.dispose();
  }
});
test("disposal during exchange cannot write a late credential",async()=>{
  const held=deferred(),x=setup({fetcher:()=>held.promise});x.session.dispose();held.resolve(reply('{"credential":"'+bearer+'"}'));
  assert.equal(await x.session.ready,false);assert.equal(x.saved.values.has(key),false);assert.equal(x.session.snapshot().kind,"disposed");
});
test("reentrant storage retirement never advances the old attempt",async()=>{
  const saved=storage();let alive=true,requests=0;
  saved.removeItem=()=>{alive=false;};
  const x=setup({isCurrent:()=>alive,getStorage:()=>saved,fetcher:async()=>{requests++;return reply("")}});
  assert.equal(await x.session.ready,false);assert.equal(requests,0);x.session.dispose();
});
test("classic bootstrap first-clear/history failure and pre-import owner",async()=>{
  assert(vm.SyntheticModule,"run fixtures with --experimental-vm-modules; missing execution is not a pass");
  const source=readFileSync(new URL("./auth_bootstrap.js",import.meta.url),"utf8");
  const events=new Map(),loaded=deferred(),steps=[];let entry=0,hidden=false,fragmentSeen;
  const context={location:{hash:"#code="+code,pathname:"/",search:""},history:{state:null,replaceState(){steps.push("clear");context.location.hash="";}},
    document:{documentElement:{dataset:{aiqBuild:build}},getElementById:()=>({set textContent(v){steps.push(v)}})},AbortController,
    addEventListener:(k,f)=>events.set(k,f),removeEventListener:(k,f)=>{if(events.get(k)===f)events.delete(k)},window:null};context.window=context;
  const realm=vm.createContext(context);
  const script=new vm.Script(source,{importModuleDynamically:async()=>{steps.push("import");await loaded.promise;const module=new vm.SyntheticModule(["start"],function(){this.setExport("start",o=>{entry++;fragmentSeen=o.fragment;return{dispose(){hidden=true}};});},{context:realm});await module.link(()=>{});await module.evaluate();return module;}});
  script.runInContext(realm);assert.equal(steps[0],"clear");events.get("pagehide")();loaded.resolve();
  await new Promise(r=>setTimeout(r,30));assert.equal(entry,0);assert.equal(hidden,false);assert.equal(fragmentSeen,undefined);
  assert(!steps.some(v=>typeof v==="string"&&v.includes(code)));
});
test("classic import positive and history failure are real execution controls",async()=>{
  assert(vm.SyntheticModule);
  const source=readFileSync(new URL("./auth_bootstrap.js",import.meta.url),"utf8");
  for(const failure of [false,true]){
    let imported=0,entry=0,removed=0;const events=new Map();
    const context={location:{hash:"#code="+code,pathname:"/",search:""},history:{state:null,replaceState(){if(failure)throw Error("invented");removed++;context.location.hash=""}},document:{documentElement:{dataset:{aiqBuild:build}},getElementById:()=>({textContent:""})},AbortController,
      addEventListener:(k,f)=>events.set(k,f),removeEventListener:(k,f)=>{if(events.get(k)===f)events.delete(k)},window:null};context.window=context;const realm=vm.createContext(context);
    const script=new vm.Script(source,{importModuleDynamically:async()=>{imported++;const module=new vm.SyntheticModule(["start"],function(){this.setExport("start",o=>{assert.equal(removed,1);assert.equal(context.location.hash,"");assert.equal(o.fragment,"code="+code);entry++;return{dispose(){}};});},{context:realm});await module.link(()=>{});await module.evaluate();return module;}});
    script.runInContext(realm);await new Promise(r=>setTimeout(r,30));
    assert.equal(entry,failure?0:1);assert.equal(imported,failure?0:1);assert.equal(events.size,failure?0:2);
  }
});

test("conversation controller inputs emit shared actual-decoder wire literals",async()=>{
  const rows=JSON.parse(readFileSync(new URL("./auth_reader_vectors.json",import.meta.url),"utf8"));assert.equal(rows.length,10);
  for(const fresh of [false,true])for(const row of rows){
    const urls=[],saved=storage(fresh?undefined:bearer);
    const x=setup({fragment:fresh?"code="+code:"",getStorage:()=>saved,fetcher:async(url,o)=>{
      if(o.method==="POST")return reply('{"credential":"'+bearer+'"}');urls.push(url);return reply('{}');
    }});assert.equal(await x.session.ready,true);
    if(row.path){await x.session.conversation(row.query);assert.deepEqual(urls,[base+row.path]);assert.equal(x.session.snapshot().kind,"unlocked");}
    else{await assert.rejects(x.session.conversation(row.query));assert.deepEqual(urls,[]);}
    x.session.dispose();
  }
});
test("storage acquisition and resolved method cannot repersist after retirement",async()=>{
  for(const phase of ["acquisition","method"])for(const action of ["forget","dispose"]){
    const saved=storage(),held=deferred();let armed=false,x,writes=0;
    const original=saved.setItem;
    Object.defineProperty(saved,"setItem",{get(){if(armed&&phase==="method"){armed=false;x.session[action]();}return(k,v)=>{writes++;original.call(saved,k,v);};}});
    x=setup({fetcher:()=>held.promise,getStorage:()=>{if(armed&&phase==="acquisition"){armed=false;x.session[action]();}return saved;}});
    armed=true;held.resolve(reply('{"credential":"'+bearer+'"}'));
    assert.equal(await x.session.ready,false);assert.equal(writes,0);assert.equal(saved.values.has(key),false);
    assert.equal(x.session.snapshot().kind,action==="forget"?"locked":"disposed");
    const later=setup({fragment:"",getStorage:()=>saved});assert.equal(await later.session.ready,false);later.session.dispose();x.session.dispose();
  }
});
test("liveness callout retirement or failure refuses before a storage effect",async()=>{
  const unavailable=setup({isCurrent:()=>{throw Error("invented initial witness failure")}});
  assert.equal(await unavailable.session.ready,false);assert.equal(unavailable.requests.length,0);assert.equal(unavailable.saved.calls.length,0);unavailable.session.dispose();
  for(const action of ["dispose","throw"]){
    const saved=storage(),held=deferred();let arm=false,inCurrent=false,x;
    x=setup({fetcher:()=>held.promise,isCurrent:()=>{if(inCurrent){inCurrent=false;if(action==="throw")throw Error("invented witness failure");x.session.dispose();}return true;},
      getStorage:()=>{if(arm){arm=false;inCurrent=true;}return saved;}});
    arm=true;held.resolve(reply('{"credential":"'+bearer+'"}'));
    assert.equal(await x.session.ready,false);assert.equal(saved.values.has(key),false);assert(!x.states.some(s=>s.kind==="unlocked"));x.session.dispose();
  }
});
test("Forget and active401 finish clearing before reentrant notification or abort",async()=>{
  for(const denied of [false,true])for(const action of ["dispose","forget","401","abort"]){
    const saved=storage();let armed=false,notified=0,calls=0,x,request;
    x=setup({getStorage:()=>saved,onState:s=>{if(armed&&s.kind==="locked"){notified++;assert(notified<3,"bounded reentrant publication");x.session[action==="forget"?"forget":"dispose"]();}},
      fetcher:async(_url,o)=>{
        if(calls++===0)return reply('{"credential":"'+bearer+'"}');
        if(action==="abort")return new Promise((_resolve,reject)=>o.signal.addEventListener("abort",()=>{x.session.dispose();reject(Error("invented abort"));},{once:true}));
        return reply("",401);
      }});
    assert.equal(await x.session.ready,true);armed=true;
    if(denied){saved.removeItem=()=>{throw Error("invented removal denial")};saved.setItem=()=>{throw Error("invented overwrite denial")};}
    if(action==="401")await assert.rejects(x.session.asks({mode:"all"}));
    else{if(action==="abort"){request=x.session.asks({mode:"all"});await x.readStarted();}x.session.forget();if(request)await assert.rejects(request);}
    assert.equal(saved.values.has(key),denied);assert.equal(notified,1);
    if(denied)assert.match(x.session.snapshot().notice,/may return on reload/);
    else if(action!=="401")assert.match(x.session.snapshot().notice,/removed/);
    x.session.dispose();
  }
});
test("replacement owner keeps its storage and publication during old cleanup",async()=>{
  const saved=storage();let current=true,replace=false,published="",x;
  const replacement="AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI";
  x=setup({isCurrent:()=>current,onState:s=>{published="A:"+s.kind;},getStorage:()=>{
    if(replace){replace=false;current=false;saved.values.set(key,replacement);published="B";}return saved;
  }});await x.session.ready;replace=true;x.session.forget();
  assert.equal(saved.values.get(key),replacement);assert.equal(published,"B");
  const later=setup({fragment:"",getStorage:()=>saved});assert.equal(await later.session.ready,true);assert.equal(later.session.snapshot().kind,"restoring");
  x.session.dispose();assert.equal(published,"B");assert.equal(saved.values.get(key),replacement);later.session.dispose();
});
