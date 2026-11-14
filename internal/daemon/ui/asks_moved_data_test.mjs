import test from 'node:test';
import assert from 'node:assert/strict';
import {captureAsks} from './asks_data.js';
import {snapshot,scope,limits,item} from './asks_values.mjs';
const capture=(data,l=limits())=>captureAsks(data,scope(),l);
const refused=data=>assert.throws(()=>capture(data),{name:'TypeError',message:'could not capture asks snapshot'});
function moved() {
  const data=snapshot();for(const group of data.sections)group.items=[];
  data.sections[3].items=[item({id:'9007199254740992',state:'probably-done',state_reason:'rule',state_at:{ms:1000,known:true}})];
  data.sections[4].items=[item({id:'9007199254740993',state:'closed',state_reason:'rule',state_at:{ms:1000,known:true}}),
    item({id:'1',state:'closed',state_reason:'person',state_at:{ms:2000,known:true}})];
  data.moved_today={local_day:'1970-01-01',start_at:{ms:0,known:true},ids:['9007199254740993','9007199254740992']};return data;
}
const legacy=d=>{const out=structuredClone(d);delete out.moved_today;return out;};

test('missing optional field preserves exact legacy output, duplicate IDs and unknown clocks',()=>{
  const data=snapshot(),out=capture(data);assert.equal(Object.hasOwn(out,'moved_today'),false);
  assert.equal(out.sections[0].items[0].id,out.sections[0].items[1].id);assert.equal(out.sections[3].items[0].state_at.known,false);
  const known=moved(),withOverview=capture(known);delete withOverview.moved_today;assert.deepEqual(withOverview,capture(legacy(known)));
});
test('complete overview is owned and ordered without lossy numeric identity',()=>{
  const data=moved(),out=capture(data);assert.deepEqual(out.moved_today,{local_day:'1970-01-01',start_at:{ms:0,known:true},ids:['9007199254740993','9007199254740992']});
  data.moved_today.ids.reverse();data.moved_today.local_day='changed';data.moved_today.start_at.ms=99;data.sections[4].items[0].id='2';
  assert.equal(out.moved_today.local_day,'1970-01-01');assert.equal(out.moved_today.start_at.ms,0);assert.equal(out.moved_today.ids[0],'9007199254740993');
});
test('empty overview proves zero; missing and empty remain distinct',()=>{
  const data=snapshot();for(const group of data.sections)group.items=[];data.moved_today={local_day:'1970-01-01',start_at:{ms:0,known:true},ids:[]};
  assert.deepEqual(capture(data).moved_today.ids,[]);assert.equal(Object.hasOwn(capture(legacy(data)),'moved_today'),false);
});
test('omitting first, last or every eligible row refuses the whole snapshot',()=>{
  for(const ids of [[],['9007199254740992'],['9007199254740993']]){const d=moved();d.moved_today.ids=ids;refused(d);}
});
test('extra, person, dangling, repeated and reversed references refuse',()=>{
  for(const ids of [['9007199254740993','9007199254740992','1'],['9007199254740993','9007199254740992','99'],
    ['9007199254740993','9007199254740993'],['9007199254740992','9007199254740993']]){const d=moved();d.moved_today.ids=ids;refused(d);}
});
test('identity ambiguity across any groups refuses without deduplication',()=>{
  for(const eligible of [true,false]){const d=moved();d.sections[eligible?3:0].items.push(item({id:'9007199254740993',state:eligible?'probably-done':'open',state_at:{ms:1000,known:true}}));refused(d);}
  const d=moved();d.sections[0].items=[item({id:'3'}),item({id:'3'})];assert.equal(capture(d).sections[0].items.length,2);
});
test('unknown candidate clock cannot prove before-day absence, while known epoch is real',()=>{
  for(const index of [0,1]){const d=moved();const rows=d.sections[3].items.concat(d.sections[4].items);rows[index].state_at={ms:0,known:false};refused(d);}
  const d=moved();d.sections[3].items[0].state_at={ms:0,known:true};assert.equal(capture(d).moved_today.ids.length,2);
  d.moved_today.start_at.ms=1000;d.moved_today.ids=['9007199254740993'];assert.equal(capture(d).moved_today.ids.length,1);
});
test('person, dismissed and open exclusions do not infer unknown clocks',()=>{
  const d=moved();d.sections[4].items[1].state_at={ms:0,known:false};
  d.sections[0].items.push(item({id:'4',state_at:{ms:0,known:false}}));d.sections[4].items.push(item({id:'5',state:'dismissed',state_at:{ms:0,known:false}}));
  assert.equal(capture(d).moved_today.ids.length,2);
  for(const id of ['4','5']){const bad=structuredClone(d);bad.moved_today.ids.push(id);refused(bad);}
});
test('future and negative signed clocks use inclusive lower bound and no upper bound',()=>{
  const d=moved();d.moved_today.start_at.ms=-2000;d.sections[3].items[0].state_at.ms=-2000;d.sections[4].items[0].state_at.ms=9007199254740000;
  assert.deepEqual(capture(d).moved_today.ids,['9007199254740993','9007199254740992']);
  d.sections[3].items[0].state_at.ms=-2001;d.moved_today.ids.pop();assert.equal(capture(d).moved_today.ids.length,1);
});
test('timestamp order precedes decimal ID length then lexical descending ties',()=>{
  const d=moved();d.sections[3].items[0].id='10';d.sections[4].items[0].id='9';d.moved_today.ids=['10','9'];assert.deepEqual(capture(d).moved_today.ids,['10','9']);
  d.sections[4].items[0].state_at.ms=2000;d.moved_today.ids=['9','10'];assert.deepEqual(capture(d).moved_today.ids,['9','10']);
});
test('present malformed object/day/start/list/IDs refuse with fixed text',()=>{
  for(const change of [d=>d.moved_today=null,d=>d.moved_today.local_day='',d=>d.moved_today.local_day=1,
    d=>d.moved_today.start_at={ms:0,known:false},d=>d.moved_today.start_at.ms=1,d=>d.moved_today.start_at.ms=9007199254740992,
    d=>d.moved_today.ids=null,d=>d.moved_today.ids[0]=9007199254740993,d=>d.moved_today.ids[0]='01',d=>delete d.moved_today.ids[0]]){
    const d=moved();change(d);refused(d);
  }
});
test('all optional descriptors reject accessors without invoking getters or DTO then',()=>{
  for(const site of ['root','day','start','id']){
    const d=moved();let calls=0;const getter={get(){calls++;throw Error('invented getter');}};
    if(site==='root')Object.defineProperty(d,'moved_today',getter);if(site==='day')Object.defineProperty(d.moved_today,'local_day',getter);
    if(site==='start')Object.defineProperty(d.moved_today,'start_at',getter);if(site==='id')Object.defineProperty(d.moved_today.ids,'0',getter);
    refused(d);assert.equal(calls,0);
  }
  const d=moved();let reads=0;Object.defineProperty(d.moved_today,'then',{get(){reads++;throw Error('not consumed');}});capture(d);assert.equal(reads,0);
});
test('reference lengths admitted before sparse/proxy indexing',()=>{
  const d=moved();let indexes=0;d.moved_today.ids=new Proxy(new Array(100000),{getOwnPropertyDescriptor(target,k){if(k!=='length')indexes++;return Reflect.getOwnPropertyDescriptor(target,k);}});
  refused(d);assert.equal(indexes,0);
});
test('day and repeated ID units plus reference entries share cumulative credits',()=>{
  // Independently enumerate the schema's string/array occurrences; ignore derived selectable.
  const d=moved();function size(v){if(typeof v==='string')return {units:v.length,entries:0};if(!v||typeof v!=='object')return{units:0,entries:0};
    const values=Object.values(v).map(size);return{units:values.reduce((n,x)=>n+x.units,0),entries:(Array.isArray(v)?v.length:0)+values.reduce((n,x)=>n+x.entries,0)};}
  const total=size(d),base=size(legacy(d));assert.equal(total.units-base.units,42);assert.equal(total.entries-base.entries,2);
  const exact={rows:3,stringUnits:total.units,arrayEntries:total.entries};assert.equal(capture(d,exact).moved_today.ids.length,2);
  for(const key of ['rows','stringUnits','arrayEntries']){const bad={...exact,[key]:exact[key]-1};assert.throws(()=>capture(d,bad),{name:'TypeError',message:'could not capture asks snapshot'});}
});
