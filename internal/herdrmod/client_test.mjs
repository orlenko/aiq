import {spawnSync} from 'node:child_process';
import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {documentBlocks} from './mod/markdown.js';
import {Selection,groups,rowKey,sessionKey,localTime,mergeSessionRows,omissionNotice,quotaCountdown,quotaResetGrant,quotaRenewals,collapseChains,sessionDivider} from './mod/state.js';
import {Bridge,argsFor,decode,MAX_REPLY,matchesTarget,validTarget} from './mod/protocol.js';
const host='3f0c9a1e5b7d24680aceb13579bdf024',revision='1'.repeat(64);
const row=(id,session='first')=>({id:String(id),host,machine:null,revision,provider:'claude',conversation_id:session,workspace:'/invented/'+session,raised_at_ms:id,state:'open',first:{preview:'Question',bytes:8,truncated:false},latest:{preview:'Question',bytes:8,truncated:false}});
const envelope=(kind,data,source=host)=>({schema:1,ok:true,kind,host:{id:source,name:'invented'},aiq:'invented',generated_at_ms:123,partial:false,omitted:{},data:kind==='hello'?{...data,commands:['hello','asks','ask-detail','ask-action','target','quotas','sessions','conversation','ask-context']}:data});
test('quota countdown switches units and measures the remaining share of each window',()=>{
  const now=Date.UTC(2031,6,1,11,0);
  const at=(seconds,fields={})=>quotaCountdown({resets_at_ms:now+seconds*1000, ...fields},now);
  assert.deepEqual(at(3*86400+2*3600,{kind:'weekly'}),
    {text:'3d 02h',fraction:(3*24+2)/(7*24)});
  assert.deepEqual(at(3600,{kind:'short',window_seconds:5*3600}),{text:'1h 00m',fraction:0.2});
  assert.equal(at(86400).text,'1d 00h');
  assert.equal(at(86399).text,'23h 59m');
  assert.deepEqual(at(3599),{text:'59m 59s',fraction:null});
  assert.equal(at(9).text,'0m 09s');
  assert.equal(at(0.1).text,'0m 01s');
  assert.deepEqual(at(0,{kind:'weekly'}),{text:'Reset due',fraction:0});
  assert.deepEqual(at(-120,{kind:'weekly'}),{text:'Reset due',fraction:0});
  assert.equal(at(5*3600+60,{window_seconds:5*3600}).fraction,1);
  assert.equal(quotaCountdown({resets_at_ms:0,kind:'weekly'},now),null);
});
test('quota cards distinguish reset grants from their quota windows',()=>{
  const time=Date.UTC(2031,6,1,11,0);
  const d=JSON.parse(readFileSync(new URL('../../cmd/aiq/testdata/app/quotas.json',import.meta.url),'utf8'));
  d.data.accounts[0].reset_credit_expires_at=time/1000;
  const a=decode(JSON.stringify(d),'quotas').data.accounts[0];
  assert.equal(quotaResetGrant(a),'1 reset grant · next expires '+localTime(time).slice(5));
  assert.equal(quotaResetGrant({...a,reset_credits:0}),'No reset grants');
  assert.equal(quotaResetGrant({...a,provider:'codex',reset_credits:2,reset_credit_expires_at_ms:0}),'2 reset credits');
});
test('quota renewal hints use fixed commands and a literal account argument',()=>{
  const a={id:'claude/demo',provider:'claude',enabled:true,login:{has_credential:true,cli:'in'},eligible:true};
  for(const poll_error of ['grant expired and invented refresh failure','no poll grant — invented','usage: HTTP 401 — invented']) {
    assert.deepEqual(quotaRenewals({...a,poll_error}),[
      {kind:'authorize',text:'Quota authorization required',command:'aiq account authorize claude/demo'}
    ]);
  }
  const loggedOut={...a,id:"claude/demo'quote",login:{has_credential:false,cli:'out'}};
  assert.equal(quotaRenewals(loggedOut)[0].command,"aiq account login 'claude/demo'\\''quote'");
  assert.equal(quotaRenewals({...loggedOut,id:'claude/demo\nmore'})[0].command,'');
  assert.equal(quotaRenewals({...a,provider:'codex',id:'codex/demo',ineligible:'authentication required'})[0].command,
    'aiq account login codex/demo');
  assert.deepEqual(quotaRenewals({...a,poll_error:'invented HTTP 500'}),[]);
  assert.deepEqual(quotaRenewals({...a,login:{has_credential:false,cli:'unreadable'}}),[]);
  assert.deepEqual(quotaRenewals({...loggedOut,enabled:false}),[]);
});
test('history omission notices give actionable reasons while older pages stay ordinary',()=>{
  assert.equal(omissionNotice({omitted:{older:100,too_large:0}}),'');
  assert.equal(omissionNotice({omitted:{too_large:2,slow:1,metadata_limit:1}}),
    '2 transcripts skipped by the file size limit · 1 transcript skipped by the read time limit · Session details omitted by the metadata limit');
  assert.equal(omissionNotice({omitted:{records:3}}),'3 transcript records skipped by the record size limit');
  assert.equal(omissionNotice({omitted:{rows:1}}),'1 ask omitted by the list limit');
});
test('a legacy workspace target requires the same unlabelled terminal creation time',()=>{
  const witness={terminal_pid:123,label:'',terminal_created_ms:456};
  const target={relation:'workspace_current',address:'t7',core_id:'made-up-core',witness};
  const live={state:'live',address:'t7',pid:123,created_ms:456,labels:{'aiq.status':'idle'}};
  assert.ok(matchesTarget(target,live));
  assert.ok(matchesTarget(target,{...live,address:'invented/t7'},'invented'));
  assert.equal(matchesTarget(target,{...live,address:'another/t7'},'invented'),false);
  for(const change of [{created_ms:457},{created_ms:undefined},{pid:124},{state:'finished'},{address:'t8'},{labels:{'aiq.target':'8'.repeat(32)}}])
    assert.equal(matchesTarget(target,{...live,...change}),false);
  assert.equal(validTarget({...target,relation:'origin'}),false);
  assert.equal(validTarget({...target,witness:{terminal_pid:123,label:''}}),false);
});
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
test('older session windows do not duplicate recorded workspace rows',()=>{
  const workspace={kind:'workspace',provider:'claude',id:'',cwd:'/invented/work',updated_ms:2};
  const first={kind:'session',provider:'claude',id:'one',updated_ms:3};
  const second={kind:'session',provider:'claude',id:'two',updated_ms:1};
  const rows=mergeSessionRows([workspace,first],[{...workspace,updated_ms:1},second]);
  assert.equal(rows.length,3);assert.equal(rows.find(r=>r.kind==='workspace').updated_ms,2);
});
test('navigation rejects kept shells, other generations and other addresses',()=>{
  const t={relation:'origin',address:'t7',core_id:'invented-core',witness:{terminal_pid:123,label:'9'.repeat(32)}};
  const live={address:'t7',state:'live',pid:123,labels:{'aiq.target':'9'.repeat(32)}};
  assert.ok(matchesTarget(t,live));
  for(const changed of [{pid:124},{address:'t8'},{state:'finished'},{labels:{'aiq.target':'8'.repeat(32)}}])assert.equal(matchesTarget(t,{...live,...changed}),false);
});
test('interactive witnesses require live address and pid while long witnesses also require the generation label',()=>{
 const t={relation:'origin',address:'t7',core_id:'invented-core',witness:{terminal_pid:123,label:''}};
 const live={address:'t7',state:'live',pid:123,labels:{}};
 assert.ok(matchesTarget(t,live));
 for(const change of [{pid:124},{state:'finished'},{address:'t8'}])assert.equal(matchesTarget(t,{...live,...change}),false);
 assert.deepEqual(argsFor('target',{id:'',lease_id:'8'}),['__app','target','--lease','8']);
});
test('native formatting never produces resource-bearing or document supplied HTML',()=>{
  const payloads=['![private](https://invalid.example/track)','![reference][id]\n\n[id]: https://invalid.example/pixel','<img src="https://invalid.example/x">','<iframe src=x></iframe>','<script>alert(1)</script>','<a href="command:run">run</a>','<video poster=x>','&#60;img src=x&#62;','```html\n<img src=x>\n```','**<img src=x>**','`<img src=x>`','<style>@import "https://invalid.example"</style>'];
  const allowed=new Set(['p','strong','pre','code','h2']);
  for(const payload of payloads)for(const marks of [false,true]) {
    const tablePayload=payload.replaceAll('\n',' ');
    const html=documentBlocks(`${payload}\n\n## ${tablePayload}\n\n> ${tablePayload}\n\n| Heading | Result |\n| --- | --- |\n| A | ${tablePayload} |\n`,marks)
      .flatMap(block=>block.table ? block.table.flat() : [block.html]).join('\n');
    for(const tag of html.matchAll(/<\/?([^>]+)>/g))assert.ok(allowed.has(tag[1]),tag[0]);
    assert.doesNotMatch(html,/<(?:img|script|a|iframe|style|video)\b/i);
  }
  assert.match(documentBlocks('**Keep this** and `code`')[0].html,/<strong>Keep this<\/strong> and <code>code<\/code>/);
});
test('native reader keeps Markdown tables and quoted paragraphs structured',()=>{
  const raw=[
    '## Tuesday', '',
    '| ID | Question | Choice |', '| ---: | :--- | :---: |',
    '| A1 | Read `a\\|b` and **check** the output | Yes |',
    '| A2 | Waiting on a response |', '',
    '> Use `report --preview` before', '> you publish the report.', '>',
    '> Send a copy to the group.', '',
    'Later:', '',
    'Item | Owner', '--- | ---', 'Report | Sam', '',
    '3. Verify the result',
  ].join('\n');
  const [intro, first, quote, divider, second, ending]=documentBlocks(raw);
  assert.equal(intro.html,'<h2>Tuesday</h2>');
  assert.deepEqual(first.table,[
    ['ID','Question','Choice'],
    ['A1','Read <code>a|b</code> and <strong>check</strong> the output','Yes'],
    ['A2','Waiting on a response',''],
  ]);
  assert.deepEqual(first.align,['right','left','center']);
  assert.ok(first.weights[0]<first.weights[1]);
  assert.deepEqual(quote,{
    html:'<p>Use <code>report --preview</code> before you publish the report.</p>\n<p>Send a copy to the group.</p>',
    quote:true,
  });
  assert.equal(divider.html,'<p>Later:</p>');
  assert.deepEqual(second.table,[['Item','Owner'],['Report','Sam']]);
  assert.equal(ending.html,'<p>3. Verify the result</p>');
  assert.equal(documentBlocks(raw,true)[0].html,'<pre>'+raw.replaceAll('>','&gt;')+'</pre>');
  assert.equal(documentBlocks('```md\n'+raw+'\n```').length,1);
  assert.equal(documentBlocks('| A | B |\n| --- |').some(block=>block.table),false);
});
test('oversized tables preserve all rows without mounting a cell view for each',()=>{
  const raw='| A | B |\n| --- | --- |\n'+Array.from({length:500},(_,i)=>`| Row ${i} | Value |`).join('\n');
  const blocks=documentBlocks(raw);
  assert.equal(blocks.length,1);
  assert.equal(blocks[0].table,undefined);
  assert.match(blocks[0].html,/<p>\| Row 499 \| Value \|<\/p>$/);
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
  for(const [file,kind] of [['hello','hello'],['asks','asks'],['asks.named','asks'],['ask-detail','ask-detail'],['ask-context','ask-context'],['quotas','quotas'],['sessions','sessions'],['conversation','conversation'],['ask-action.ok','ask-action'],['target','target']])decode(readFileSync(new URL(file+'.json',root),'utf8'),kind);
  assert.equal(decode(hello,'hello').schema,1);
});

test('unmatched inline backtick runs render within a child-process watchdog',()=>{
 const url=new URL('./mod/markdown.js',import.meta.url).href;
 const script=`import {documentBlocks} from ${JSON.stringify(url)};const raw='prefix '+String.fromCharCode(96).repeat(256*1024);if(documentBlocks(raw)[0].html!=='<p>'+raw+'</p>')process.exit(2);`;
 const r=spawnSync(process.execPath,['--input-type=module','-e',script],{timeout:3000,encoding:'utf8'});
 assert.equal(r.error,undefined);assert.equal(r.status,0,r.stderr);
});

test('Copilot quotas do not hide the other configured providers',()=>{
 const d=JSON.parse(readFileSync(new URL('../../cmd/aiq/testdata/app/quotas.json',import.meta.url),'utf8'));
 d.data.accounts.push({...d.data.accounts[0],id:'copilot/invented',provider:'copilot'});
 const result=decode(JSON.stringify(d),'quotas');assert.ok(result.data.accounts.some(a=>a.provider==='copilot'));assert.ok(result.data.accounts.some(a=>a.provider==='claude'));
 const key='m0-100-0123456789abcdef';assert.deepEqual(argsFor('conversation',{provider:'claude',id:'invented-id',anchor:key}),['__app','conversation','--provider','claude','--id','invented-id','--anchor',key]);
});

test('saved session names label and filter ask groups without replacing their workspace identity',()=>{
 const d=decode(readFileSync(new URL('../../cmd/aiq/testdata/app/asks.named.json',import.meta.url),'utf8'),'asks');
 const rows=d.data.rows.map(r=>({...r,host:d.host.id,machine:null}));
 const g=groups(rows,'Harp');assert.equal(g.length,1);assert.equal(g[0].name,'Harp');assert.equal(g[0].workspace,'/invented/work');assert.equal(g[0].key,sessionKey(rows[0]));
});

const chainID=i=>`0000000${i}-0000-4000-8000-000000000000`;
const chainOf=(seq,size=3,key='a'.repeat(16))=>({key,seq,size});
test('a rollover chain shows as its latest loaded session, read as the whole chain',()=>{
  const r=(id,chain,h=host)=>({kind:'session',host:h,provider:'claude',id,updated_ms:1,...(chain?{chain}:{})});
  const rows=collapseChains([r('a',chainOf(1)),r('c',chainOf(3)),r('b',chainOf(2)),r('alone'),r('x',chainOf(1),'2'.repeat(32)),r('y',chainOf(2,2,'b'.repeat(16)))]);
  assert.deepEqual(rows.map(x=>x.id),['c','alone','x','y']);
  assert.equal(rows[0].readChain,true);assert.equal(rows[1].readChain,undefined);
});
test('chain fields decode only when well formed, and the chain read takes no anchor',()=>{
  const sessions={window:{cursor:''},rows:[{kind:'session',provider:'claude',id:chainID(2),cwd:'/w',name:'n',updated_ms:1,chain:chainOf(3)}]};
  assert.equal(decode(JSON.stringify(envelope('sessions',sessions)),'sessions').data.rows[0].chain.seq,3);
  for(const bad of [{...chainOf(3),seq:4},{...chainOf(3),key:'x'},{...chainOf(3),size:1},{...chainOf(3),seq:0}])
    assert.throws(()=>decode(JSON.stringify(envelope('sessions',{...sessions,rows:[{...sessions.rows[0],chain:bad}]})),'sessions'));
  const list=[{provider:'claude',conversation_id:chainID(0),started_ms:0,available:false},{provider:'codex',conversation_id:chainID(1),started_ms:5,available:true}];
  const message=(session)=>({key:'s'+session+'-m0-0-'+'a'.repeat(16),role:'user',text:'x',at_ms:1,session});
  assert.equal(decode(JSON.stringify(envelope('conversation',{sessions:list,messages:[message(1)],cursor:{}})),'conversation').data.sessions.length,2);
  assert.throws(()=>decode(JSON.stringify(envelope('conversation',{sessions:list,messages:[message(2)],cursor:{}})),'conversation'));
  assert.throws(()=>decode(JSON.stringify(envelope('conversation',{messages:[message(0)],cursor:{}})),'conversation'));
  assert.deepEqual(argsFor('conversation',{provider:'claude',id:chainID(2),chain:true,before:'s1-m0-0-'+'a'.repeat(16)}),
    ['__app','conversation','--provider','claude','--id',chainID(2),'--before','s1-m0-0-'+'a'.repeat(16),'--chain','1']);
  assert.throws(()=>argsFor('conversation',{provider:'claude',id:chainID(2),chain:true,at:5}));
  assert.equal(sessionDivider(list[0],0,2),'Session 1 of 2 · claude · session 00000000 · transcript not available');
  assert.equal(sessionDivider(list[1],1,2),'Session 2 of 2 · codex · session 00000001 · started '+localTime(5));
  assert.equal(omissionNotice({omitted:{sessions:1}}),'1 earlier session could not be read');
  assert.equal(omissionNotice({omitted:{chain_limit:3,chain_unknown:1}}),'3 earlier sessions not shown, by the session limit · Other sessions of this chain could not be looked up');
});
