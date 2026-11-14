import test from 'node:test';
import assert from 'node:assert/strict';
import {captureSessions, captureDetail, captureDetailResponse, captureDirectories, captureState, captureLong} from './controller_data.js';

const id = '00000001-0000-4000-8000-000000000001';
const scope = {provider:'agy', id};
const limits = (arrayEntries=1000, stringUnits=10000, rows=10) => ({arrayEntries,stringUnits,rows});
const time = () => ({ms:0,known:false});
const header = () => ({...scope,cwd:'/f',title:'T',model:'M',branch:'B',started:time(),updated:time(),worker:false,bypass:true});
function detail() {
  return {build:'b',session:header(),earlier_hidden:0,later_hidden:0,window_start:0,reset:true,markup:false,
    messages:[{id:'1'.padStart(32,'0'),key:'a'.repeat(64),kind:'you',sub:'prompt',turn:0,at:time(),
      source_known:true,source:'human',account_known:false,account:'',raw:'r',groups:[{
        ordinal:0,visible:true,leading_break:false,empty_body_line:false,
        head:{mode:'',who:'',clock_mode:'',clock_text:'',at:time(),duration_known:false,duration_ms:0,tools:0},
        base:['b','b'],lines:[{kind:'text',table_rows:0,spans:[{c:['i','u'],d:false,t:'x'}]}]}]}]};
}
function row(more={}) {
  return {provider:'agy',id,cwd:'/fixture',label:'complete label',title:'title',model:'model',branch:'branch',
    updated:'2026-01-02T03:04:05.123456789Z',turns:2,tools:3,worker:false,bypass:true,...more};
}
const sessions = (rows=[]) => ({dir:'/x',roots_searched:['r'],partial:true,scanned:3,of:7,unreadable_count:2,sessions:rows});
const reject = fn => assert.throws(fn,{name:'TypeError',message:'could not capture navigator response'});

test('workspace metadata owns nullable fields, scope, knownness and aggregate admission',()=>{
  const value={build:'b',kind:'workspace',requested_dir:'/A/sub',workspace:'/A',workspace_known:true,state:'unknown',partial:true,unmapped_launches:1,history_present:true,observed:time(),
    lease:null,current:null,current_spans:[],earlier:[{key:'a'.repeat(64),provider:'claude',session_id:'',transcript_available:false,workspace_known:false,last:time(),spans:[{account:'Invented A',launcher:'fixture',from:time(),until:time()}]}],
    workspaces:[],note:{available:false,exists:false,updated:time(),archived:0}};
  const out=captureLong(value,'/A/sub',true,limits());assert.deepEqual(out,value);value.earlier[0].spans[0].account='mutated';assert.equal(out.earlier[0].spans[0].account,'Invented A');
  for(const change of [{current:{provider:'claude',id}},{requested_dir:'/B'},{history_present:false},{kind:'list'},{workspace:'relative'},{state:'ended'}])reject(()=>captureLong({...value,...change},'/A/sub',true,limits()));
  reject(()=>captureLong(value,'/A/sub',true,limits(1)));reject(()=>captureLong(value,'/A/sub',true,limits(1000,2)));
  let calls=0;Object.defineProperty(value,'lease',{get(){calls++;return null;}});reject(()=>captureLong(value,'/A/sub',true,limits()));assert.equal(calls,0);
});
test('workspace current requires positive backend and closed supported identity',()=>{
  const value={build:'b',kind:'workspace',requested_dir:'/A',workspace:'/A',workspace_known:true,state:'live',partial:false,unmapped_launches:0,history_present:false,observed:time(),
    lease:{id:'7',started:'1',provider:'agy',account:'Invented',launcher:'fixture',pane:'%1',live:'live',drain:'waiting',working_known:false,working:false,turn_started:time()},current:scope,current_spans:[],earlier:[],workspaces:[],note:{available:false,exists:false,updated:time(),archived:0}};
  assert.deepEqual(captureLong(value,'/A',false,limits()),value);
  for(const change of [{live:'unknown'},{drain:'server shell text'},{working_known:true},{working:true}])reject(()=>captureLong({...value,lease:{...value.lease,...change}},'/A',false,limits()));
  reject(()=>captureLong({...value,current:{provider:'herdr',id}},'/A',false,limits()));
  reject(()=>captureLong({...value,note:{...value.note,available:true}},'/A',false,limits()));
});

test('full directory catalog owns knownness, default scope and complete text admission',()=>{
  const value={source:'session index',partial:true,metadata_known:false,scanned:1,of:1,unreadable_count:0,unknown_directories:0,
    default_dir:'/A',default_reason:'directory fallback',directories:[{path:'/A',updated_ms:0,updated_known:false,sessions:1,workers:0}]};
  const out=captureDirectories(value,limits(1,35));assert.deepEqual(out,value);assert.notEqual(out.directories,value.directories);
  reject(()=>captureDirectories(value,limits(1,34)));reject(()=>captureDirectories(value,limits(0,35)));
  for(const change of [{partial:false},{metadata_known:true},{default_dir:'/missing'},{default_reason:'server command'},{scanned:2}])reject(()=>captureDirectories({...value,...change},limits()));
  const bad=structuredClone(value);bad.directories[0].updated_ms=1;reject(()=>captureDirectories(bad,limits()));
  let accessed=0;Object.defineProperty(value,'default_dir',{get(){accessed++;return '/A';}});reject(()=>captureDirectories(value,limits()));assert.equal(accessed,0);
});
test('all-session capture requires explicit all intent and an empty echoed directory',()=>{
  const value={...sessions([row()]),dir:''};
  assert.equal(captureSessions(value,'',limits(),true).sessions.length,1);
  reject(()=>captureSessions(value,'',limits()));reject(()=>captureSessions(value,'/x',limits(),true));
  reject(()=>captureSessions(sessions([row()]),'',limits(),true));
});

test('delta replaces the complete changing tail and re-admits the retained prefix',()=>{
  const base=detail();base.selection='A'.repeat(43);base.tail_turn=1;base.tail_key='a'.repeat(64);base.from_turn=0;
  const second=structuredClone(base.messages[0]);second.id='2'.padStart(32,'0');second.turn=1;second.groups[0].ordinal=1;base.messages.push(second);
  const tail={...structuredClone(base),reset:false,from_turn:1,tail_turn:2,tail_key:'b'.repeat(64),messages:[structuredClone(second)]};
  tail.messages[0].raw='replacement raw';const last=structuredClone(second);last.id='3'.padStart(32,'0');last.turn=2;last.groups[0].ordinal=2;tail.messages.push(last);
  const request={fromTurn:1,fromKey:base.tail_key},before=JSON.stringify(base);
  const out=captureDetailResponse(tail,scope,limits(),base,request);
  assert.equal(out.reset,true);assert.equal(out.from_turn,0);assert.equal(out.messages.length,3);assert.equal(out.messages[0].raw,base.messages[0].raw);assert.equal(out.messages[1].raw,'replacement raw');
  assert.equal(JSON.stringify(base),before);assert.notEqual(out.messages[0],base.messages[0]);
  // Each two-message input fits, but the three-message candidate does not.
  captureDetail(base,scope,limits(16));reject(()=>captureDetailResponse(tail,scope,limits(16),base,request));
  for(const change of [{from_turn:0},{window_start:1,earlier_hidden:1},{markup:true},{build:'other'},{later_hidden:1},{tail_turn:0}])reject(()=>captureDetailResponse({...tail,...change},scope,limits(),base,request));
  reject(()=>captureDetailResponse(tail,scope,limits(),base,{...request,fromKey:'c'.repeat(64)}));
  assert.equal(captureDetailResponse(base,scope,limits(),null,{}).messages.length,2);
});

test('detail selection is optional for old hosts, owned, canonical and charged',()=>{
  const d=detail();d.selection='A'.repeat(43);
  assert.equal(captureDetail(d,scope,limits(8,208)).selection,d.selection);
  reject(()=>captureDetail(d,scope,limits(8,207)));
  d.selection='A'.repeat(42)+'B';reject(()=>captureDetail(d,scope,limits()));
  let accessed=0;Object.defineProperty(d,'selection',{get(){accessed++;return 'A'.repeat(43);}});
  reject(()=>captureDetail(d,scope,limits()));assert.equal(accessed,0);
});
test('state belongs to exact retained selection and never publishes mismatch facts',()=>{
  const selected={...scope,provider:'claude',selection:'A'.repeat(43)};
  const base={build:'b',...selected,match:true,cached:true,cached_turns:2,cached_updated:{ms:0,known:true},disk_changed:true,
    live:'live',working_known:true,working:true,elapsed_ms:1000};
  assert.deepEqual(captureState(base,selected,limits()),base);
  for(const mutation of [{selection:'B'.repeat(42)+'A'},{id:'00000002-0000-4000-8000-000000000002'},
    {live:'unknown'},{working_known:false},{working:false},{elapsed_ms:-1},{cached:false},
    {cached_updated:{ms:1,known:false}}])reject(()=>captureState({...base,...mutation},selected,limits()));
  const mismatch={build:'b',...selected,match:false,reason:'selection_changed'};
  const hostile={...mismatch};Object.defineProperty(hostile,'cached_turns',{get(){throw Error('unused');}});
  assert.deepEqual(captureState(hostile,selected,limits()),mismatch);
  const agy={...selected,provider:'agy'};reject(()=>captureState({...base,provider:'agy'},agy,limits()));
  reject(()=>captureState(base,selected,limits(1000,1)));
});

test('directory catalog owns complete metadata before publication and rejects wrong scope shapes',()=>{
  const input={source:'launch metadata',directories:[{path:'/invented/A',updated_ms:20},{path:'/invented/B',updated_ms:10}]};
  const out=captureDirectories(input,limits());input.directories[0].path='/changed';assert.equal(out.directories[0].path,'/invented/A');
  const invalid=[{source:'complete catalog',directories:[]},{source:'launch metadata',directories:[{path:'relative',updated_ms:1}]},
    {source:'launch metadata',directories:[{path:'/a/../b',updated_ms:1}]},{source:'launch metadata',directories:[{path:'/a',updated_ms:0}]},
    {source:'launch metadata',directories:[{path:'/a',updated_ms:1},{path:'/a',updated_ms:1}]},
    {source:'launch metadata',directories:[{path:'/a',updated_ms:1},{path:'/b',updated_ms:2}]}];
  for(const value of invalid)reject(()=>captureDirectories(value,limits()));
  const ties={source:'launch metadata',directories:[{path:'/\ue000',updated_ms:1},{path:'/😀',updated_ms:1}]};assert.equal(captureDirectories(ties,limits()).directories.length,2);
});
test('directory catalog charges the complete array and UTF16 strings without reading accessors',()=>{
  const input={source:'launch metadata',directories:[{path:'/😀',updated_ms:1}]};
  assert.equal(captureDirectories(input,limits(1,18)).directories[0].path,'/😀');
  reject(()=>captureDirectories(input,limits(1,17)));reject(()=>captureDirectories(input,limits(0,18)));
  let accessed=0;const bad={source:'launch metadata',directories:[Object.defineProperty({},'path',{get(){accessed++;return '/x';}})]};
  reject(()=>captureDirectories(bad,limits()));assert.equal(accessed,0);
});

test('list has independent literal text, structure and row admission boundaries',()=>{
  assert.deepEqual(captureSessions(sessions(),'/x',limits(1,3,1)),
    {dir:'/x',roots_searched:['r'],partial:true,scanned:3,of:7,unreadable_count:2,sessions:[]});
  reject(()=>captureSessions(sessions(),'/x',limits(1,2,1)));
  reject(()=>captureSessions({...sessions(),roots_searched:['r','']},'/x',limits(1,3,1)));
  const input=sessions([row(),row({label:'second'})]);
  assert.equal(captureSessions(input,'/x',limits(3,10000,2)).sessions.length,2);
  reject(()=>captureSessions(input,'/x',limits(3,10000,1)));
  reject(()=>captureSessions(input,'/x',limits(2,10000,2)));
});

test('detail charges literal 165 UTF16 units and eight cumulative array entries',()=>{
  // 45 build/header +111 message facts +2 base +4 kind +2 span classes +1 paint.
  const out=captureDetail(detail(),scope,limits(8,165));
  assert.equal(out.messages[0].raw,'r');
  assert.deepEqual(out.messages[0].groups[0].base,['b','b']);
  assert.deepEqual(out.messages[0].groups[0].lines,[{kind:'text',table_rows:0,spans:[{c:['i','u'],d:false,t:'x'}]}]);
  reject(()=>captureDetail(detail(),scope,limits(7,165)));
  reject(()=>captureDetail(detail(),scope,limits(8,164)));
  const wide=detail();wide.messages[0].groups[0].lines[0].spans[0].t='😀';
  reject(()=>captureDetail(wide,scope,limits(8,165)));
  assert.equal(captureDetail(wide,scope,limits(8,166)).messages[0].groups[0].lines[0].spans[0].t,'😀');
});

test('oversized sparse arrays refuse before the first indexed descriptor',()=>{
  for(const kind of ['roots','rows','messages']) {
    let indexed=0;
    const sparse=new Proxy(new Array(2**32-1),{getOwnPropertyDescriptor(target,key){
      if(key!=='length') indexed++;
      return Reflect.getOwnPropertyDescriptor(target,key);
    }});
    if(kind==='messages') {const d=detail();d.messages=sparse;reject(()=>captureDetail(d,scope,limits()));}
    else {const d=sessions();d[kind==='roots'?'roots_searched':'sessions']=sparse;reject(()=>captureSessions(d,'/x',limits()));}
    assert.equal(indexed,0);
  }
});

test('empty nested structures consume aggregate credit even with no painted text',()=>{
  const d=detail(),original=d.messages[0].groups[0];
  d.messages[0].groups=Array.from({length:9},(_,ordinal)=>({...original,ordinal,visible:false,base:[],lines:[]}));
  assert.equal(captureDetail(d,scope,limits(10)).messages[0].groups.length,9);
  reject(()=>captureDetail(d,scope,limits(9)));
});

test('raw, hidden and header text are charged without truncation',()=>{
  for(const mutate of [d=>d.messages[0].raw+='x',d=>d.session.title+='x',
    d=>{d.messages[0].groups[0].visible=false;d.messages[0].groups[0].lines[0].spans[0].t+='x';}]) {
    const d=detail();mutate(d);reject(()=>captureDetail(d,scope,limits(8,165)));
    const out=captureDetail(d,scope,limits(8,166));
    assert.equal(out.messages[0].raw,d.messages[0].raw);assert.equal(out.session.title,d.session.title);
  }
});

test('full detail header preserves known epoch and unknown independently of list dates',()=>{
  const d=detail();d.session.started={ms:0,known:true};d.session.updated={ms:-42,known:true};
  assert.deepEqual(captureDetail(d,scope,limits()).session,
    {provider:'agy',id,cwd:'/f',title:'T',model:'M',branch:'B',started:{ms:0,known:true},updated:{ms:-42,known:true},worker:false,bypass:true});
  const listed=captureSessions(sessions([row()]),'/x',limits());
  assert.equal(listed.sessions[0].updated,'2026-01-02T03:04:05.123456789Z');
  for(const mutate of [s=>s.started={ms:1,known:false},s=>s.updated={ms:0.5,known:true},
    s=>s.title=0,s=>s.bypass='true',s=>delete s.model]) {
    const bad=detail();mutate(bad.session);reject(()=>captureDetail(bad,scope,limits()));
  }
});

test('current wrong directory is refused against an independent canonical identity',()=>{
  const input={...sessions([row()]),dir:'/fixture/B'};
  assert.equal(captureSessions(input,'/fixture/B',limits()).dir,'/fixture/B');
  reject(()=>captureSessions(input,'/fixture/A',limits()));
  reject(()=>captureSessions(input,'/fixture/B/',limits()));
  for(const bad of ['',null,{},undefined]) reject(()=>captureSessions(input,bad,limits()));
  // Raw-query normalization belongs to the later trusted host, not this pure API.
  assert.equal(input.dir,'/fixture/B');
});

test('duplicate rows and unavailable identity text remain complete and ordered',()=>{
  const d=sessions([row({label:'first'}),row({label:'second'}),row({id:'unavailable',label:'third'}),
    row({provider:'AGY',label:'fourth'}),row({id:'ABCDEF01-0000-4000-8000-000000000001',label:'fifth'})]);
  const out=captureSessions(d,'/x',limits());
  assert.deepEqual(out.sessions.map(r=>[r.label,r.selectable]),
    [['first',true],['second',true],['third',false],['fourth',false],['fifth',true]]);
  assert.equal(out.sessions[4].id,'ABCDEF01-0000-4000-8000-000000000001');
  assert.deepEqual([out.partial,out.scanned,out.of,out.unreadable_count],[true,3,7,2]);
  assert.equal(Object.hasOwn(d.sessions[0],'selectable'),false);
});

test('consumed accessors, array holes and inherited fields fail without getter calls',()=>{
  let calls=0;
  for(const kind of ['header','last-span','row','array-entry']) {
    const d=kind==='row'?sessions([row(),row()]):detail();
    const [object,key]=kind==='header'?[d.session,'title']:kind==='last-span'?[d.messages[0].groups[0].lines[0].spans[0],'t']:
      kind==='row'?[d.sessions[1],'label']:[d.messages,'0'];
    Object.defineProperty(object,key,{get(){calls++;throw new Error('invented getter');}});
    reject(()=>kind==='row'?captureSessions(d,'/x',limits()):captureDetail(d,scope,limits()));
  }
  assert.equal(calls,0);
  const hole=detail();delete hole.messages[0];reject(()=>captureDetail(hole,scope,limits()));
  const inherited=detail();delete inherited.session.title;
  Object.setPrototypeOf(inherited.session,{title:'inherited'});reject(()=>captureDetail(inherited,scope,limits()));
});

test('unknown commands, getters, then, toJSON and iterators are never read',()=>{
  let calls=0;const getter=()=>{calls++;throw new Error('unused');};
  const d=detail(),l=sessions([row()]);
  for(const object of [d,d.session,d.messages,d.messages[0],l,l.sessions[0]]) {
    for(const key of ['then','toJSON','unused',Symbol.iterator]) Object.defineProperty(object,key,{get:getter});
  }
  Object.defineProperty(l.sessions[0],'commands',{get:getter});Object.defineProperty(l.sessions[0],'started',{get:getter});
  assert.equal(captureDetail(d,scope,limits()).messages[0].raw,'r');
  assert.equal(captureSessions(l,'/x',limits()).sessions[0].label,'complete label');
  assert.equal(calls,0);
});

test('every closed class is preserved and unknown late base or span classes fail whole',()=>{
  const allowed=['b','dim','i','u','inverse','band','you','agent','muted','accent','code'];
  const d=detail();d.messages[0].groups[0].base=[...allowed,...allowed];
  d.messages[0].groups[0].lines[0].spans[0].c=[...allowed];
  const out=captureDetail(d,scope,limits());assert.deepEqual(out.messages[0].groups[0].base,[...allowed,...allowed]);
  assert.deepEqual(out.messages[0].groups[0].lines[0].spans[0].c,allowed);
  for(const place of ['base','span']) {
    const bad=detail();const late=structuredClone(bad.messages[0]);late.id='2'.padStart(32,'0');late.groups[0].ordinal=1;bad.messages.push(late);
    if(place==='base') late.groups[0].base.push('future-role');else late.groups[0].lines[0].spans[0].c.push('future-role');
    reject(()=>captureDetail(bad,scope,limits()));
  }
});

test('owned captures resist caller and result mutation without paint/raw flattening',()=>{
  const d=detail(),l=sessions([row()]);d.messages[0].raw='raw\n**different**';
  const a=captureDetail(d,scope,limits()),b=captureSessions(l,'/x',limits());
  d.session.title='changed';d.messages[0].raw='changed';d.messages[0].groups[0].lines[0].spans[0].t='changed';l.sessions[0].label='changed';
  assert.equal(a.session.title,'T');assert.equal(a.messages[0].raw,'raw\n**different**');
  assert.equal(a.messages[0].groups[0].lines[0].spans[0].t,'x');assert.equal(b.sessions[0].label,'complete label');
  a.session.model='result';b.roots_searched[0]='result';assert.equal(d.session.model,'M');assert.deepEqual(l.roots_searched,['r']);
});

test('existing reset, scope, unique IDs, ordinal, kind and source validation remains active',()=>{
  for(const mutate of [d=>d.reset=false,d=>d.session.id='00000002-0000-4000-8000-000000000002',
    d=>d.messages.push(structuredClone(d.messages[0])),d=>d.messages[0].groups[0].ordinal=2,
    d=>d.messages[0].kind='future-kind',d=>d.messages[0].source='unknown',d=>d.earlier_hidden=1]) {
    const d=detail();mutate(d);reject(()=>captureDetail(d,scope,limits()));
  }
});

test('limits require own positive safe-integer credits and are never consumed in place',()=>{
  const budget=limits(8,165,1);captureDetail(detail(),scope,budget);assert.deepEqual(budget,{arrayEntries:8,stringUnits:165,rows:1});
  for(const key of ['arrayEntries','stringUnits','rows']) {
    for(const bad of [0,-1,0.5,Infinity,NaN,Number.MAX_SAFE_INTEGER+1,'8',undefined]) {
      reject(()=>captureDetail(detail(),scope,{...limits(),[key]:bad}));
    }
    const inherited=Object.create(limits());reject(()=>captureDetail(detail(),scope,inherited));
    let calls=0;const accessor=limits();Object.defineProperty(accessor,key,{get(){calls++;return 1000;}});
    reject(()=>captureDetail(detail(),scope,accessor));assert.equal(calls,0);
  }
});
