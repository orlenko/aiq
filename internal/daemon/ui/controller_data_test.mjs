import test from 'node:test';
import assert from 'node:assert/strict';
import {captureSessions, captureDetail, captureDirectories, captureState} from './controller_data.js';

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
