import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {webcrypto,createHmac,createHash} from 'node:crypto';
import {prepareTabRead2} from './tab_proof.js';
import {createTabSession} from './auth_session.js';
const v=JSON.parse(readFileSync(new URL('../../navauth/testdata/tab_conditional_vectors.json',import.meta.url),'utf8'));
const base='http://127.0.0.1:'+v.port,enc=new TextEncoder(),query={provider:'claude',id:'00000001-0000-4000-8000-000000000001',last:50,markup:false,anchor:0};
const deterministic=(subtle=webcrypto.subtle)=>({subtle,getRandomValues:a=>{a.set(Buffer.from(v.nonce,'base64url'));return a;}});
const frame=(...fields)=>Buffer.concat(fields.flatMap(s=>[Buffer.from(String(Buffer.byteLength(s))+':'),Buffer.from(s)]));
function proof(header,status,etag,body){return createHmac('sha256',Buffer.from(v.key_hex,'hex')).update(frame('aiq-nav-tab-response-v2',header.split('.').at(-1),String(status),status===304?'':'application/json',v.build,etag,createHash('sha256').update(body).digest('base64url'))).digest('base64url');}
function response(header,status,etag,body,mime=status===304?null:'application/json'){
 const headers={'X-Aiq-Build':v.build,'X-Aiq-Tab-Proof':proof(header,status,etag,body)};if(mime!==null)headers['Content-Type']=mime;if(etag)headers.ETag=etag;
 return new Response(status===304?null:body,{status,headers});
}
function session(fetcher,extra={}){return createTabSession({base,build:v.build,fragment:'',isCurrent:()=>true,getStorage:()=>({getItem:()=>v.credential,setItem(){},removeItem(){}}),limits:{sessions:1024,conversation:1024,asks:1024},fetcher,crypto:deterministic(),...extra});}
const tick=()=>new Promise(r=>setImmediate(r));

test('v2 browser matches independent full, bodyless304 and error literals',async()=>{
 for(const c of v.cases){const p=await prepareTabRead2(v.credential,base,v.build,v.target,c.condition,()=>true,deterministic());assert.equal(p.authorization,c.authorization);await p.verify(c.response,c.status,c.mime,v.build,enc.encode(c.body),c.etag);assert(!p.authorization.includes(v.credential));}
});
test('ETag, condition, MIME and body substitutions cannot satisfy conditional proofs',async()=>{
 const c=v.cases[1],p=await prepareTabRead2(v.credential,base,v.build,v.target,c.condition,()=>true,deterministic());
 for(const args of [[c.response,304,'application/json',v.build,new Uint8Array(),c.etag],[c.response,304,'',v.build,new Uint8Array(),'' ],[c.response,304,'',v.build,new Uint8Array(),'"'+'c'.repeat(64)+'"'],[c.response,304,'',v.build,enc.encode('x'),c.etag],[c.response,304,'','other-build',new Uint8Array(),c.etag],[c.response,200,'application/json',v.build,new Uint8Array(),c.etag]])await assert.rejects(p.verify(...args));
 const other=await prepareTabRead2(v.credential,base,v.build,v.target,'"'+'d'.repeat(64)+'"',()=>true,deterministic());await assert.rejects(other.verify(c.response,304,'',v.build,new Uint8Array(),c.etag));
});
test('every v2 cryptographic await loses authority when its owner retires',async()=>{
 for(const phase of ['digest','importKey','selector','request','response-digest','verify']){
  let live=true,signs=0,digests=0;const subtle={};
  for(const name of ['digest','importKey','sign','verify'])subtle[name]=async(...args)=>{const result=await webcrypto.subtle[name](...args);let hit=name===phase;if(name==='digest')hit=(++digests===1?phase==='digest':phase==='response-digest');if(name==='sign')hit=(++signs===1?phase==='selector':phase==='request');if(hit)live=false;return result;};
  const c=v.cases[1],prepare=()=>prepareTabRead2(v.credential,base,v.build,v.target,c.condition,()=>live,deterministic(subtle));
  if(['response-digest','verify'].includes(phase)){const p=await prepare();await assert.rejects(p.verify(c.response,304,'',v.build,new Uint8Array(),c.etag));}else await assert.rejects(prepare());
 }
});
test('a verified full conversation ETag permits a verified null-body304 envelope',async()=>{
 const requests=[];const s=session(async(url,o)=>{requests.push({url,o});const c=v.cases[requests.length-1];return response(o.headers.Authorization,c.status,c.etag,c.body);});await s.ready;
 const first=await s.conversation(query);assert.deepEqual(first,{data:JSON.parse(v.cases[0].body),etag:v.cases[0].etag});
 const next=await s.conversation({...query,etag:first.etag});assert.deepEqual(next,{notModified:true,etag:first.etag});
 assert.equal(requests[0].o.headers['If-None-Match'],undefined);assert.equal(requests[1].o.headers['If-None-Match'],first.etag);assert.equal(s.snapshot().kind,'unlocked');s.dispose();
});
test('native-style empty304 streams reach EOF before verification and nonempty streams refuse',async()=>{
 for(const nonempty of [false,true]){
  const tag=v.cases[0].etag;let reads=0;
  const s=session(async(_url,o)=>{
   const r=response(o.headers.Authorization,304,tag,'');
   const stream=new ReadableStream({pull(controller){reads++;if(nonempty)controller.enqueue(enc.encode('x'));controller.close();}});
   Object.defineProperty(r,'body',{value:stream});return r;
  });await s.ready;
  if(nonempty){await assert.rejects(s.conversation({...query,etag:tag}));assert.equal(s.snapshot().kind,'terminal');}
  else {assert.deepEqual(await s.conversation({...query,etag:tag}),{notModified:true,etag:tag});assert.equal(s.snapshot().kind,'unlocked');}
  assert.ok(reads>0);s.dispose();
 }
});
test('unsolicited or malformed304 retires the session without returning data',async()=>{
 for(const mode of ['unconditional','wrong-tag','mime','missing-tag','unsigned']){
  const tag=v.cases[0].etag;let count=0;
  const s=session(async(_url,o)=>{count++;const r=response(o.headers.Authorization,304,mode==='missing-tag'?'':mode==='wrong-tag'?'"'+'f'.repeat(64)+'"':tag,'',mode==='mime'?'application/json':null);if(mode==='unsigned')r.headers.delete('X-Aiq-Tab-Proof');return r;});await s.ready;
  await assert.rejects(s.conversation(mode==='unconditional'?query:{...query,etag:tag}));assert.equal(s.snapshot().kind,'terminal');await assert.rejects(s.conversation(query));assert.equal(count,1);s.dispose();
 }
});
test('conditional input is captured before crypto and malformed/getter values cause no read',async()=>{
 let release,entered=false;const gate=new Promise(r=>release=r),subtle={};
 for(const name of ['digest','importKey','sign','verify'])subtle[name]=async(...args)=>{if(name==='digest'&&!entered){entered=true;await gate;}return webcrypto.subtle[name](...args);};
 const requests=[];const s=session(async(_url,o)=>{requests.push(o);return response(o.headers.Authorization,304,v.cases[0].etag,'');},{crypto:deterministic(subtle)});await s.ready;
 const q={...query,etag:v.cases[0].etag},pending=s.conversation(q);while(!entered)await tick();q.etag='changed';release();assert.deepEqual(await pending,{notModified:true,etag:v.cases[0].etag});assert.equal(requests[0].headers['If-None-Match'],v.cases[0].etag);s.dispose();
 let reads=0,getters=0;const invalid=session(async()=>{reads++;throw Error('must not fetch')});await invalid.ready;
 for(const etag of ['',null,5,'W/'+v.cases[0].etag,'"'+'A'.repeat(64)+'"'])await assert.rejects(invalid.conversation({...query,etag}));
 const accessor={...query};Object.defineProperty(accessor,'etag',{get(){getters++;return v.cases[0].etag}});await assert.rejects(invalid.conversation(accessor));assert.equal(reads,0);assert.equal(getters,0);assert.equal(invalid.snapshot().kind,'restoring');invalid.dispose();
});
