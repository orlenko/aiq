import test from 'node:test';
import assert from 'node:assert/strict';
import {captureAsks} from './asks_data.js';

const keys=['asks','warnings','other_open','probably_done','closed_dismissed'];
const scope={mode:'all',dir:''};
const limits=(arrayEntries=1000,stringUnits=20000,rows=100)=>({arrayEntries,stringUnits,rows});
const time=()=>({ms:0,known:false});
const quote=(text,c=[])=>({lines:[{kind:'text',table_rows:0,spans:[{c,d:false,t:text}]}],copy:`> ${text}\n\n`});
const item=(extra={})=>({id:'1',provider:'agy',session_id:'00000001-0000-4000-8000-000000000001',
  workspace:'/f',state:'open',raised_at:time(),state_at:time(),repeats:0,state_reason:'',state_text:'',
  refs:['x'],first:quote('first',['i']),latest:quote('latest'),...extra});
function snapshot(rows=[]) {return {scope:{...scope},sections:keys.map((key,i)=>({key,items:i===0?rows:[]}))};}
const report=()=>({known:true,pass_at:time(),ok_at:{ms:0,known:true},error_at:{ms:-42,known:true},
  next_try_at:{ms:Number.MAX_SAFE_INTEGER,known:true},watched:1,waiting:2,skipped:3,calls_today:4,
  last_error:'fault',not_watched:['why']});
const reject=fn=>assert.throws(fn,{name:'TypeError',message:'could not capture asks snapshot'});
const capture=(data,budget=limits(),expected=scope)=>captureAsks(data,expected,budget);

test('empty snapshot has literal five-entry, 54 UTF16-unit boundaries',()=>{
  assert.deepEqual(capture(snapshot(),limits(5,54,1)),{scope:{mode:'all',dir:''},
    sections:[{key:'asks',items:[]},{key:'warnings',items:[]},{key:'other_open',items:[]},
      {key:'probably_done',items:[]},{key:'closed_dismissed',items:[]}],report:{known:false}});
  reject(()=>capture(snapshot(),limits(4,54,1)));
  reject(()=>capture(snapshot(),limits(5,53,1)));
});

test('both variants and every metadata/class occurrence consume cumulative credits',()=>{
  // Empty54 + identity/state/workspace46 + first19 + latest20 + ref1 =140.
  const data=snapshot([item()]);
  const out=capture(data,limits(12,140,1));
  assert.equal(out.sections[0].items[0].first.copy,'> first\n\n');
  assert.equal(out.sections[0].items[0].latest.copy,'> latest\n\n');
  reject(()=>capture(data,limits(11,140,1)));
  reject(()=>capture(data,limits(12,139,1)));
  for(const mutate of [r=>r.latest.copy+=' ',r=>r.workspace+='x',r=>r.state_reason+='x',
    r=>r.first.lines[0].spans[0].t+='😀']) {
    const d=snapshot([item()]);mutate(d.sections[0].items[0]);reject(()=>capture(d,limits(12,140,1)));
  }
  const emoji=snapshot([item()]);emoji.sections[0].items[0].latest.lines[0].spans[0].t='😀';
  assert.equal(capture(emoji,limits(12,136,1)).sections[0].items[0].latest.lines[0].spans[0].t,'😀');
  reject(()=>capture(emoji,limits(12,135,1)));
});

test('row credit aggregates across groups and does not deduplicate identities',()=>{
  const d=snapshot([item()]);d.sections[1].items=[item({workspace:'second'})];
  d.sections[3].items=[item({state:'probably-done'})];
  d.sections[4].items=[item({state:'dismissed'}),item({state:'closed'})];
  const out=capture(d,limits(1000,20000,5));
  assert.deepEqual(out.sections.map(g=>g.items.length),[1,1,0,1,2]);
  assert.deepEqual(out.sections.flatMap(g=>g.items.map(r=>r.id)),['1','1','1','1','1']);
  assert.equal(out.sections[1].items[0].workspace,'second');
  reject(()=>capture(d,limits(1000,20000,4)));
});

test('every consumed array refuses huge sparse length before its first indexed descriptor',()=>{
  for(const place of ['sections','items','refs','first-lines','last-spans','classes','not-watched']) {
    const d=snapshot([item()]);let indexed=0;
    const sparse=new Proxy(new Array(2**32-1),{getOwnPropertyDescriptor(target,key){
      if(key!=='length') indexed++;
      return Reflect.getOwnPropertyDescriptor(target,key);
    }});
    const r=d.sections[0].items[0];
    if(place==='sections') d.sections=sparse;
    if(place==='items') d.sections[0].items=sparse;
    if(place==='refs') r.refs=sparse;
    if(place==='first-lines') r.first.lines=sparse;
    if(place==='last-spans') r.latest.lines[0].spans=sparse;
    if(place==='classes') r.first.lines[0].spans[0].c=sparse;
    if(place==='not-watched') d.report={...report(),not_watched:sparse};
    reject(()=>capture(d));assert.equal(indexed,0,place);
  }
});

test('empty nested variants still consume entries and rows',()=>{
  const empty=()=>item({refs:[],first:{lines:[],copy:'>\n\n'},latest:{lines:[],copy:'>\n\n'}});
  const d=snapshot(Array.from({length:8},empty));
  assert.equal(capture(d,limits(13,20000,8)).sections[0].items.length,8);
  reject(()=>capture(d,limits(12,20000,8)));
  reject(()=>capture(d,limits(13,20000,7)));
});

test('closed states, exact section order and lossless positive int64 IDs',()=>{
  for(const id of ['1','9007199254740993','9223372036854775807'])
    assert.equal(capture(snapshot([item({id})])).sections[0].items[0].id,id);
  for(const id of ['0','01','-1','1.0','1e2','9223372036854775808','100000000000000000000',1])
    reject(()=>capture(snapshot([item({id})])));
  for(const mutate of [d=>d.sections.reverse(),d=>d.sections.pop(),d=>d.sections[0].key='other',
    d=>d.sections[0].items[0].state='closed',d=>d.sections[0].items[0].state='OPEN',
    d=>{d.sections[3].items=[item()];},d=>{d.sections[4].items=[item({state:'probably-done'})];}]) {
    const d=snapshot([item()]);mutate(d);reject(()=>capture(d));
  }
});

test('only exact independent scope matches and supported UUID identity enables navigation',()=>{
  const expected={mode:'directory',dir:'/fixture/A'};
  const d=snapshot([item(),item({provider:'AGY'}),item({session_id:'unavailable'}),
    item({session_id:'ABCDEF01-0000-4000-8000-000000000001'})]);d.scope={...expected};
  const rows=capture(d,limits(),expected).sections[0].items;
  assert.deepEqual(rows.map(r=>r.selectable),[true,false,false,true]);
  assert.equal(rows[3].session_id,'ABCDEF01-0000-4000-8000-000000000001');
  assert.equal(Object.hasOwn(d.sections[0].items[0],'selectable'),false);
  for(const other of [scope,{...expected,dir:'/fixture/A/'},{...expected,dir:'/fixture/B'},
    {mode:'directory',dir:''},{mode:'all',dir:'/fixture/A'},{mode:'ALL',dir:''}])
    reject(()=>capture(d,limits(),other));
});

test('known epoch, signed safe time and complete stored report facts remain distinct',()=>{
  const d=snapshot([item({raised_at:{ms:0,known:true},state_at:{ms:-42,known:true}})]);d.report=report();
  const out=capture(d);
  assert.deepEqual(out.sections[0].items[0].raised_at,{ms:0,known:true});
  assert.deepEqual(out.sections[0].items[0].state_at,{ms:-42,known:true});
  assert.deepEqual(out.report,report());
  const only=snapshot();only.report=report();capture(only,limits(6,62,1));
  reject(()=>capture(only,limits(5,62,1)));reject(()=>capture(only,limits(6,61,1)));
  for(const bad of [{ms:1,known:false},{ms:0.5,known:true},{ms:Infinity,known:true},{ms:0,known:1}]) {
    reject(()=>capture(snapshot([item({raised_at:bad})])));
    const x=snapshot();x.report={...report(),error_at:bad};reject(()=>capture(x));
  }
});

test('absent/inherited reports stay unknown; unknown consumes only its own tag once',()=>{
  let calls=0,knownReads=0;const d=snapshot();
  Object.setPrototypeOf(d,Object.defineProperty({},'report',{get(){calls++;throw Error('unused');}}));
  assert.deepEqual(capture(d).report,{known:false});
  const unknown={known:false};
  for(const key of ['last_error','not_watched','pass_at']) Object.defineProperty(unknown,key,{get(){calls++;throw Error('unused');}});
  Object.defineProperty(d,'report',{value:new Proxy(unknown,{getOwnPropertyDescriptor(target,key){
    if(key==='known') knownReads++;return Reflect.getOwnPropertyDescriptor(target,key);
  }})});
  assert.deepEqual(capture(d).report,{known:false});assert.equal(calls,0);assert.equal(knownReads,1);
  const getter=snapshot();Object.defineProperty(getter,'report',{get(){calls++;return report();}});
  reject(()=>capture(getter));assert.equal(calls,0);
  for(const bad of [null,{},[],{known:0}]) {const x=snapshot();x.report=bad;reject(()=>capture(x));}
});

test('all consumed data must be own descriptors; holes/getters fail without invocation',()=>{
  let calls=0;
  for(const place of ['id','last-copy','last-class','report-time','array-entry']) {
    const d=snapshot([item()]);d.report=report();const r=d.sections[0].items[0];
    const [target,key]=place==='id'?[r,'id']:place==='last-copy'?[r.latest,'copy']:
      place==='last-class'?[r.first.lines[0].spans[0].c,0]:place==='report-time'?[d.report,'pass_at']:[d.sections,4];
    Object.defineProperty(target,key,{get(){calls++;throw Error('private fixture');}});reject(()=>capture(d));
  }
  const hole=snapshot([item()]);delete hole.sections[0].items[0].refs[0];reject(()=>capture(hole));
  const inherited=snapshot([item()]);const r=inherited.sections[0].items[0];delete r.id;Object.setPrototypeOf(r,{id:'1'});
  reject(()=>capture(inherited));assert.equal(calls,0);
});

test('unknown getters, then, toJSON and iterators are never consumed',()=>{
  const d=snapshot([item()]);let calls=0;
  for(const obj of [d,d.sections,d.sections[0],d.sections[0].items[0],d.sections[0].items[0].first.lines])
    for(const key of ['unused','then','toJSON',Symbol.iterator]) Object.defineProperty(obj,key,{get(){calls++;throw Error('unused');}});
  assert.equal(capture(d).sections[0].items[0].id,'1');assert.equal(calls,0);
});

test('closed paint catalog retains duplicates, block grouping and decoration; late errors fail whole',()=>{
  const d=snapshot([item(),item()]),r=d.sections[0].items[1];
  r.latest.lines=['rule','quote','code','code','table','text'].map(kind=>({kind,table_rows:kind==='table'?2:0,
    spans:[{c:['b','b','dim','i','u','inverse','band','you','agent','muted','accent','code'],d:true,t:'literal'}]}));
  assert.deepEqual(capture(d).sections[0].items[1].latest.lines,r.latest.lines);
  for(const mutate of [x=>x.latest.lines[5].kind='future',x=>x.latest.lines[5].spans[0].c.push('future'),
    x=>x.latest.lines[5].spans[0].d=1,x=>x.latest.lines[5].table_rows=-1]) {
    const bad=structuredClone(d);mutate(bad.sections[0].items[1]);reject(()=>capture(bad));
  }
});

test('quoted copy preserves independent complete literals; refuses sanitizer residue and framing gaps',()=>{
  for(const copy of ['>\n\n','> one\n>\n> two\n\n','> \tUnicode 😀\n\n']) {
    const d=snapshot([item()]);d.sections[0].items[0].latest.copy=copy;
    assert.equal(capture(d).sections[0].items[0].latest.copy,copy);
  }
  const bad=['','> one\n','one\n\n','> one\n\n\n','> one\nplain\n\n','> one\n\n> two\n\n'];
  for(const cp of [0,1,8,11,12,13,31,127,128,159,0x061c,0x200e,0x200f,0x2028,0x2029,0x202a,0x202e,0x2066,0x2069])
    bad.push('> a'+String.fromCodePoint(cp)+'b\n\n');
  for(const copy of bad) for(const key of ['first','latest']) {
    const d=snapshot([item(),item()]);d.sections[0].items[1][key].copy=copy;reject(()=>capture(d));
  }
});

test('owned snapshots isolate caller/result mutation and leave limits unchanged',()=>{
  const d=snapshot([item()]);d.report=report();const budget=limits(),out=capture(d,budget);
  d.sections[0].items[0].first.lines[0].spans[0].t='changed';d.sections[0].items[0].latest.copy='changed';d.report.not_watched[0]='changed';
  assert.equal(out.sections[0].items[0].first.lines[0].spans[0].t,'first');
  assert.equal(out.sections[0].items[0].latest.copy,'> latest\n\n');assert.deepEqual(out.report.not_watched,['why']);
  out.sections[0].items[0].refs.push('result');out.report.last_error='result';
  assert.deepEqual(d.sections[0].items[0].refs,['x']);assert.equal(d.report.last_error,'fault');assert.deepEqual(budget,limits());
  for(const key of ['arrayEntries','stringUnits','rows']) {
    for(const bad of [0,-1,0.5,Infinity,NaN,Number.MAX_SAFE_INTEGER+1,'10',undefined]) reject(()=>capture(snapshot(),{...limits(),[key]:bad}));
    let calls=0;const accessor=limits();Object.defineProperty(accessor,key,{get(){calls++;return 1000;}});
    reject(()=>capture(snapshot(),accessor));assert.equal(calls,0);
  }
  reject(()=>capture(snapshot(),Object.create(limits())));
});


test('optional totals preserve absent-report facts with literal shared credits',()=>{
  const d=snapshot();d.totals={utc_day:'2031-04-06',skipped:3,calls_today:7};
  const out=capture(d,limits(5,64,1));
  assert.deepEqual(out.report,{known:false});
  assert.deepEqual(out.totals,{utc_day:'2031-04-06',skipped:3,calls_today:7});
  reject(()=>capture(d,limits(5,63,1)));
  d.totals.utc_day='changed';assert.equal(out.totals.utc_day,'2031-04-06');
  out.totals.calls_today=99;assert.equal(d.totals.calls_today,7);
  assert.equal(Object.hasOwn(capture(snapshot()),'totals'),false);
});

test('totals are own data and agree with known report counters',()=>{
  let calls=0;const inherited=snapshot();
  Object.setPrototypeOf(inherited,Object.defineProperty({},'totals',{get(){calls++;throw Error('inherited');}}));
  assert.equal(Object.hasOwn(capture(inherited),'totals'),false);assert.equal(calls,0);
  const own=snapshot();Object.defineProperty(own,'totals',{get(){calls++;throw Error('accessor');}});
  reject(()=>capture(own));assert.equal(calls,0);
  for(const key of ['utc_day','skipped','calls_today']) {
    const d=snapshot();d.totals={utc_day:'2031-04-06',skipped:3,calls_today:7};
    Object.defineProperty(d.totals,key,{get(){calls++;throw Error('field');}});
    reject(()=>capture(d));assert.equal(calls,0);
  }
  const d=snapshot();d.report=report();d.totals={utc_day:'2031-04-06',skipped:3,calls_today:4};
  assert.equal(capture(d,limits(6,72,1)).totals.calls_today,4);
  reject(()=>capture(d,limits(6,71,1)));
  d.totals.calls_today=7;reject(()=>capture(d));d.totals.calls_today=4;d.totals.skipped=2;reject(()=>capture(d));
});

test('malformed optional totals fail whole without string or numeric repair',()=>{
  for(const value of [null,undefined,[],{}, {utc_day:'',skipped:0,calls_today:0}]) {
    const d=snapshot();d.totals=value;reject(()=>capture(d));
  }
  for(const value of [-1,.5,Infinity,Number.MAX_SAFE_INTEGER+1,'3',null,undefined]) {
    for(const key of ['skipped','calls_today']) {
      const d=snapshot();d.totals={utc_day:'2031-04-06',skipped:0,calls_today:0,[key]:value};reject(()=>capture(d));
    }
  }
});

test('scope open counts must agree with complete supplied rows and all scope has no elsewhere',()=>{
 const d=snapshot([item()]);d.scope_counts={open:1,elsewhere_open:0};assert.deepEqual(capture(d).scope_counts,d.scope_counts);
 d.scope_counts.open=0;reject(()=>capture(d));d.scope_counts.open=1;d.scope_counts.elsewhere_open=-1;reject(()=>capture(d));d.scope_counts.elsewhere_open=7;reject(()=>capture(d));
 const inherited=snapshot();Object.setPrototypeOf(inherited,{scope_counts:{open:99,elsewhere_open:99}});assert.equal(capture(inherited).scope_counts,undefined);
});

test('status line captures only data with fixed remedy vocabulary while allowing repeated reordered codes',()=>{
 const d=snapshot();d.status_line={known:true,text:'stored arbitrary command-looking text',problem:true,remedy:['watch','setup','watch','service_install']};assert.deepEqual(capture(d).status_line,d.status_line);
 d.status_line.remedy.push('reset');reject(()=>capture(d));d.status_line.remedy=[];Object.defineProperty(d.status_line,'text',{get(){throw Error('must not run');}});reject(()=>capture(d));
});
