import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {documentHTML} from './mod/markdown.js';
import {Selection,groups,rowKey,sessionKey,localTime} from './mod/state.js';
import {Bridge,argsFor,decode,MAX_REPLY,matchesTarget} from './mod/protocol.js';
const host='3f0c9a1e5b7d24680aceb13579bdf024',revision='1'.repeat(64);
const row=(id,session='first')=>({id:String(id),host,machine:null,revision,provider:'claude',conversation_id:session,workspace:'/invented/'+session,raised_at_ms:id,state:'open',first:{preview:'Question',bytes:8,truncated:false},latest:{preview:'Question',bytes:8,truncated:false}});
const envelope=(kind,data,source=host)=>({schema:1,ok:true,kind,host:{id:source,name:'invented'},aiq:'invented',generated_at_ms:123,partial:false,omitted:{},data});
test('dismissing stays within the selected session until its last ask',()=>{
  const a=row(1),b=row(2),other=row(9,'other'),s=new Selection();s.choose(a);
  assert.equal(s.reconcile(groups([b,other])).id,'2');assert.equal(s.group,sessionKey(b));
  assert.equal(s.reconcile(groups([other])).id,'9');assert.equal(s.group,sessionKey(other));
});
test('peek ownership changes with ask selection and never follows to the next ask',()=>{
  const s=new Selection();s.choose(row(1));s.peek=rowKey(row(1));const generation=s.generation;
  s.choose(row(2));assert.equal(s.peek,null);assert.ok(s.generation>generation);
  s.choose(row(3,'other'));assert.equal(s.peek,null);
});
test('groups distinguish machine, provider and conversation even in the same path',()=>{
  const a=row(1),b={...row(2),host:'2'.repeat(32)},c={...row(3),provider:'codex'};
  assert.equal(groups([a,b,c]).length,3);assert.equal(groups([a],'missing').length,0);
  assert.match(localTime(1794942000000),/^\d{4}-\d\d-\d\d \d\d:\d\d$/);
});
test('one conversation remains one group when its working directory changes',()=>{
  assert.equal(groups([row(1),{...row(2),workspace:'/invented/elsewhere'}]).length,1);
});
test('navigation rejects kept shells, other generations and other addresses',()=>{
  const t={relation:'origin',address:'t7',core_id:'invented-core',witness:{terminal_pid:123,label:'9'.repeat(32)}};
  const live={address:'t7',state:'live',pid:123,labels:{'aiq.target':'9'.repeat(32)}};
  assert.ok(matchesTarget(t,live));
  for(const changed of [{pid:124},{address:'t8'},{state:'finished'},{labels:{'aiq.target':'8'.repeat(32)}}])assert.equal(matchesTarget(t,{...live,...changed}),false);
});
test('native formatting never produces resource-bearing or document supplied HTML',()=>{
  const payloads=['![private](https://invalid.example/track)','![reference][id]\n\n[id]: https://invalid.example/pixel','<img src="https://invalid.example/x">','<iframe src=x></iframe>','<script>alert(1)</script>','<a href="command:run">run</a>','<video poster=x>','&#60;img src=x&#62;','```html\n<img src=x>\n```','**<img src=x>**','`<img src=x>`','<style>@import "https://invalid.example"</style>'];
  const allowed=new Set(['p','br','strong','pre','code','blockquote']);
  for(const payload of payloads)for(const marks of [false,true]) {
    const html=documentHTML(payload,marks);
    for(const tag of html.matchAll(/<\/?([^>]+)>/g))assert.ok(allowed.has(tag[1]),tag[0]);
    assert.doesNotMatch(html,/<(?:img|script|a|iframe|style|video)\b/i);
  }
  assert.match(documentHTML('**Keep this** and `code`'),/<strong>Keep this<\/strong> and <code>code<\/code>/);
});
test('argv has a closed action vocabulary and never interpolates shell input',()=>{
  assert.deepEqual(argsFor('ask-action',{id:'17',revision,action:'dismiss',host}),['__app','ask-action','--id','17','--revision',revision,'--action','dismiss','--host',host]);
  for(const action of ['rm','dismiss; touch x',''])assert.throws(()=>argsFor('ask-action',{id:'17',revision,action,host}));
  assert.throws(()=>argsFor('conversation',{provider:'claude',id:'$(do something)'}));
});
test('bridge admits only complete bounded responses for the bound source',()=>{
  const raw=JSON.stringify(envelope('asks',{setup:'ready',counts:{known:true},rows:[row(1)]}));
  assert.equal(decode(raw,'asks',host).data.rows.length,1);
  assert.throws(()=>decode(raw.slice(0,-2),'asks',host));
  assert.throws(()=>decode(raw,'asks','2'.repeat(32)),e=>e.code==='source_changed');
  assert.throws(()=>decode(' '.repeat(MAX_REPLY+1),'asks',host));
});
test('read-only remote binary discovery happens before a single write attempt',async()=>{
  const calls=[];
  const bridge=new Bridge(async(command,args)=>{
    calls.push([command,args]);
    if(command==='aiq')throw new Error('not found');
    if(command==='sh')return {code:0,stdout:'/invented/bin/aiq\n'};
    if(args[1]==='hello')return {code:0,stdout:JSON.stringify(envelope('hello',{}))};
    if(args[1]==='ask-action')throw new Error('timeout after write');
    return {code:0,stdout:JSON.stringify(envelope('asks',{setup:'ready',counts:{known:true},rows:[]}))};
  });
  await bridge.read('invented-machine','asks');
  await assert.rejects(bridge.action('invented-machine',row(1),'dismiss'),e=>e.code==='unknown_outcome');
  assert.equal(calls.filter(c=>c[1][1]==='ask-action').length,1);
  assert.equal(calls.filter(c=>c[0]==='sh').length,1);
});
test('source change invalidates action authority without clearing useful data elsewhere',async()=>{
  let changed=false;
  const bridge=new Bridge(async(_c,args)=>({code:0,stdout:JSON.stringify(envelope(args[1],args[1]==='asks'?{setup:'ready',counts:{known:true},rows:[]}:{},changed?'2'.repeat(32):host))}));
  await bridge.read(null,'asks');changed=true;
  await assert.rejects(bridge.read(null,'asks'),e=>e.code==='source_changed');
  await assert.rejects(bridge.action(null,row(1),'dismiss'),e=>e.code==='source_changed');
});
test('superseded queued reads do not start and transport never exceeds four jobs',async()=>{
  const bridge=new Bridge(async()=>{throw new Error('unexpected call');});
  const releases=[];let peak=0,running=0;
  const jobs=Array.from({length:4},()=>bridge.slot(async()=>{running++;peak=Math.max(peak,running);await new Promise(r=>releases.push(r));running--;}));
  bridge.source(null).host=host;let current=true;
  const stale=bridge.read(null,'asks',{},()=>current);await Promise.resolve();current=false;
  const extra=bridge.slot(async()=>{running++;peak=Math.max(peak,running);running--;});
  releases.forEach(r=>r());await Promise.all(jobs);
  await assert.rejects(stale,e=>e.code==='superseded');await extra;assert.equal(peak,4);assert.equal(bridge.active,0);
});
test('native client fixtures match the Go bridge contract',()=>{
  // Once the bridge is present these are the actual golden files, rather than
  // a second independently maintained version of the protocol.
  const root=new URL('../../cmd/aiq/testdata/app/',import.meta.url);
  const hello=readFileSync(new URL('hello.json',root),'utf8');
  for(const [file,kind] of [['hello','hello'],['asks','asks'],['ask-context','ask-context'],['quotas','quotas'],['sessions','sessions'],['conversation','conversation'],['ask-action.ok','ask-action']])decode(readFileSync(new URL(file+'.json',root),'utf8'),kind);
  assert.equal(decode(hello,'hello').schema,1);
});
