import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {webcrypto,createHash,createHmac} from 'node:crypto';
import {prepareTabRead} from './tab_proof.js';
import {createTabSession} from './auth_session.js';
const v=JSON.parse(readFileSync(new URL('../../navauth/testdata/tab_vectors.json',import.meta.url),'utf8'));
const base='http://127.0.0.1:'+v.port,enc=new TextEncoder();
function deterministic(subtle=webcrypto.subtle){return{subtle,getRandomValues:a=>{a.set(Buffer.from(v.nonce,'base64url'));return a;}};}
async function prepared(valid=()=>true,crypto=deterministic()){return prepareTabRead(v.credential,base,v.build,v.target,valid,crypto);}
const frame=(...fields)=>Buffer.concat(fields.flatMap(s=>[Buffer.from(String(Buffer.byteLength(s))+':'),Buffer.from(s)]));
function signFor(header,raw,status=200){const requestMAC=header.split('.').at(-1),conditional=header.startsWith('AiqTab2 '),fields=[conditional?'aiq-nav-tab-response-v2':'aiq-nav-tab-response-v1',requestMAC,String(status),'application/json',v.build];if(conditional)fields.push('');fields.push(createHash('sha256').update(raw).digest('base64url'));return createHmac('sha256',Buffer.from(v.key_hex,'hex')).update(frame(...fields)).digest('base64url');}
function saved(){return{getItem:()=>v.credential,setItem(){},removeItem(){}};}
function session(fetcher,extra={}) {return createTabSession({base,build:v.build,fragment:'',getStorage:saved,isCurrent:()=>true,limits:{sessions:1024,conversation:1024,asks:1024},fetcher,...extra});}

test('browser request and verification match independently calculated literal Go vectors',async()=>{
 const p=await prepared();assert.equal(p.authorization,v.authorization);assert(!p.authorization.includes(v.credential));assert(!JSON.stringify(p).includes(v.key_hex));
 await p.verify(v.response,200,'application/json',v.build,enc.encode(v.body));
});
test('changed body, status, MIME, build, nonce, request or key is refused as a whole',async()=>{
 const p=await prepared();
 for(const args of [[v.response,200,'application/json',v.build,enc.encode(v.body+' ')],[v.response,201,'application/json',v.build,enc.encode(v.body)],[v.response,200,'text/plain',v.build,enc.encode(v.body)],[v.response,200,'application/json','other',enc.encode(v.body)],['A'.repeat(43),200,'application/json',v.build,enc.encode(v.body)],['',200,'application/json',v.build,enc.encode(v.body)]])await assert.rejects(p.verify(...args));
 const otherNonce={subtle:webcrypto.subtle,getRandomValues:a=>{a.fill(1);return a;}};
 const q=await prepared(()=>true,otherNonce);await assert.rejects(q.verify(v.response,200,'application/json',v.build,enc.encode(v.body)));
 const other=await prepareTabRead(v.credential,base,v.build,v.target+'x',()=>true,deterministic());await assert.rejects(other.verify(v.response,200,'application/json',v.build,enc.encode(v.body)));
});
test('each asynchronous crypto boundary loses authority on retirement',async()=>{
 for(const phase of ['digest','importKey','selector','request','response-digest','verify']){
  let live=true,signs=0,digests=0;const subtle={};
  for(const name of ['digest','importKey','sign','verify'])subtle[name]=async(...args)=>{const value=await webcrypto.subtle[name](...args);let hit=name===phase;if(name==='digest')hit=(++digests===1?phase==='digest':phase==='response-digest');if(name==='sign')hit=(++signs===1?phase==='selector':phase==='request');if(hit)live=false;return value;};
  if(['response-digest','verify'].includes(phase)){const p=await prepared(()=>live,deterministic(subtle));await assert.rejects(p.verify(v.response,200,'application/json',v.build,enc.encode(v.body)));}
  else await assert.rejects(prepared(()=>live,deterministic(subtle)));
 }
});
test('unsigned replacement data cannot publish or trigger additional data reads',async()=>{
 let calls=0,detached=0;const s=session(async(_url,o)=>{calls++;assert.match(o.headers.Authorization,/^AiqTab2 /);assert(!o.headers.Authorization.includes(v.credential));return new Response('{"invented":"replacement"}',{headers:{'Content-Type':'application/json','X-Aiq-Build':v.build}});});await s.ready;s.attachHost(()=>detached++);
 await assert.rejects(s.asks({mode:'all'}));assert.equal(s.snapshot().kind,'terminal');assert.equal(detached,1);await assert.rejects(s.asks({mode:'all'}));assert.equal(calls,1);s.dispose();
});
test('genuine signed request errors permit explicit retry; missing proof does not',async()=>{
 for(const signed of [true,false]){let fail=true,calls=0;const s=session(async(_url,o)=>{calls++;const raw=enc.encode(fail?'invented private error':'{}'),status=fail?503:200,headers={'Content-Type':'application/json','X-Aiq-Build':v.build};if(signed)headers['X-Aiq-Tab-Proof']=signFor(o.headers.Authorization,raw,status);return new Response(raw,{status,headers});});await s.ready;await assert.rejects(s.asks({mode:'all'}));
  assert.equal(s.snapshot().kind,signed?'restoring':'terminal');fail=false;if(signed){assert.deepEqual(await s.asks({mode:'all'}),{data:{}});assert.equal(calls,2);}else{await assert.rejects(s.asks({mode:'all'}));assert.equal(calls,1);}s.dispose();
 }
});
test('crypto cancellation before fetch causes zero network and keeps caller cancellation local',async()=>{
 let finish;const gate=new Promise(r=>finish=r);let entered=false,calls=0;
 const subtle={digest:async(...args)=>{entered=true;await gate;return webcrypto.subtle.digest(...args);},importKey:webcrypto.subtle.importKey.bind(webcrypto.subtle),sign:webcrypto.subtle.sign.bind(webcrypto.subtle),verify:webcrypto.subtle.verify.bind(webcrypto.subtle)};
 const parent=new AbortController(),s=session(async()=>{calls++;throw Error('must not fetch')},{crypto:deterministic(subtle)});await s.ready;
 const pending=assert.rejects(s.asks({mode:'all'},parent.signal));while(!entered)await new Promise(r=>setImmediate(r));parent.abort();finish();await pending;assert.equal(calls,0);assert.equal(s.snapshot().kind,'restoring');s.dispose();
});
test('static executable graph is complete before auth entry and contains no later import',()=>{
 const source=readFileSync(new URL('./auth_session.js',import.meta.url),'utf8');assert.match(source,/import \{createAppHost\} from "\.\/app\.js"/);assert.match(source,/import \{prepareTabRead2\} from "\.\/tab_proof\.js"/);assert(!/\bimport\s*\(/.test(source));
 for(const file of ['app.js','read_broker.js','session_controller.js','controller_data.js','feed.js','feed_data.js','dom.js','asks_controller.js','asks_view.js','asks_data.js','tab_proof.js'])assert(!/\bimport\s*\(/.test(readFileSync(new URL('./'+file,import.meta.url),'utf8')),file);
});

test('retired verification holds the physical reader before the successor can fetch',async()=>{
 const {createReadBroker}=await import('./read_broker.js');let release,entered=false,calls=0,verifies=0;
 const gate=new Promise(r=>release=r),subtle={};for(const name of ['digest','importKey','sign','verify'])subtle[name]=async(...args)=>{if(name==='verify'&&++verifies===1){entered=true;await gate;}return webcrypto.subtle[name](...args);};
 const s=session(async(_url,o)=>{calls++;const raw=enc.encode('{}');return new Response(raw,{headers:{'Content-Type':'application/json','X-Aiq-Build':v.build,'X-Aiq-Tab-Proof':signFor(o.headers.Authorization,raw)}});},{crypto:deterministic(subtle)});await s.ready;
 const broker=createReadBroker({asks:s.asks}),first=broker.route(),old=assert.rejects(first.read('asks',{mode:'all'}));
 while(!entered)await new Promise(r=>setImmediate(r));const successor=broker.route(),next=successor.read('asks',{mode:'all'});
 await new Promise(r=>setImmediate(r));assert.equal(calls,1);release();await old;assert.deepEqual(await next,{data:{}});assert.equal(calls,2);assert.equal(s.snapshot().kind,'unlocked');broker.close();s.dispose();
});
