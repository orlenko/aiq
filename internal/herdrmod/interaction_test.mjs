import test from 'node:test';
import assert from 'node:assert/strict';
import AIQ from './mod/main.js';
import {context,setChecking} from './native_mock.mjs';
import {rowKey} from './mod/state.js';
const host='3'.repeat(32),revision='1'.repeat(64);
const row=(id,session='first')=>({host,machine:null,id:String(id),revision,state:'open',section:'asks',provider:'claude',conversation_id:session,workspace:'/invented/'+session,raised_at_ms:Number(id),first:{preview:'Question'},latest:{preview:'Question'}});
const asks=rows=>({host:{id:host},generated_at_ms:1,partial:false,data:{rows,setup:'ready',counts:{known:true}}});
function setup(rows){setChecking(true);const cx=context(),view=new AIQ();view.init({},cx);view.sources.set('',{machine:null,layout:'ready',asks:{value:asks(rows),loading:false,error:null}});view.selection.choose(rows[0]);setChecking(false);return {cx,view};}
test('late context replies cannot reopen a closed peek or follow another ask',async()=>{
  const a=row(1),b=row(2),{cx,view}=setup([a,b]);let resolve;
  view.bridge={read:()=>new Promise(r=>{resolve=r;})};
  view.peek(a,cx);assert.equal(view.selection.peek,rowKey(a));
  view.selection.choose(b);view.context=null;
  resolve({data:{id:a.id,revision:a.revision,messages:[{anchor:true}],placement:{confidence:'time+text'}}});await cx.drain();
  assert.equal(view.context,null);assert.equal(view.selection.peek,null);
});
test('a newer ask revision invalidates the old detail and peek',async()=>{
  const a=row(1),{cx,view}=setup([a]);view.detail={revision:a.revision,value:{latest:{text:'Old'}}};view.selection.peek=rowKey(a);view.context={value:{messages:[]}};
  const newer={...a,revision:'2'.repeat(64)};view.sources.get('').asks.value=asks([newer]);
  view.bridge={read:async()=>({data:{id:a.id,revision:newer.revision,latest:{text:'New'}}})};
  view.reconcile(cx);await cx.drain();assert.equal(view.selection.peek,null);assert.equal(view.detail.value.latest.text,'New');
});
test('a failed refresh retains last known rows and exposes the failure',async()=>{
  const a=row(1),{cx,view}=setup([a]);view.bridge={read:async()=>{throw {code:'busy'};}};
  view.load(view.sources.get(''),'asks',cx);await cx.drain();
  const c=view.sources.get('').asks;assert.equal(c.error,'busy');assert.equal(c.value.data.rows[0].id,'1');assert.equal(c.loading,false);
});
test('quota TODO refreshes and copies renewal commands across the machine filter',async()=>{
  const {cx,view}=setup([row(1)]);
  const remote={machine:'invented-remote',layout:'ready'};
  view.sources.set(remote.machine,remote);
  view.tab='quotas';
  view.machine='';
  assert.equal(view.quotaTodo(),null);

  const cli={id:'codex/demo',provider:'codex',login:{cli:'out'}};
  const grant={id:'claude/demo',provider:'claude',poll_error:'grant expired'};
  let accounts=[cli,grant,{...cli,id:'codex/disabled',enabled:false}];
  const reads=[];
  view.bridge={read:async(machine,kind,_options,keepGoing)=>{
    assert.equal(kind,'quotas');
    assert.ok(keepGoing());
    reads.push(machine);
    return {data:{accounts:machine===remote.machine?accounts:[]}};
  }};
  view.refresh(cx);
  await cx.drain();
  assert.deepEqual(reads,[null,remote.machine]);

  const copied=[];
  const todo=view.quotaTodo();
  const groups=todo.handlers.children[1].handlers.children;
  assert.equal(groups.length,1);
  const commands=groups[0].handlers.children.slice(1);
  for (const command of commands) {
    const button=command.handlers.children[1];
    button.handlers.click({},{
      write_to_clipboard:text=>copied.push(text),
      notify(){},
      timer:{after(){}},
    });
  }
  assert.deepEqual(copied,['aiq account login codex/demo','aiq account authorize claude/demo']);
  accounts=[];
  view.refresh(cx);
  await cx.drain();
  assert.equal(view.quotaTodo(),null);
});
test('dismiss batch stops at uncertainty and never repeats a write',async()=>{
  const a=row(1),b=row(2),c=row(3),{cx,view}=setup([a,b,c]);const actions=[];let rows=[a,b,c];
  view.bridge={action:async(_machine,r)=>{actions.push(r.id);if(r.id==='2')throw {code:'unknown_outcome'};rows=rows.map(x=>x.id===r.id?{...x,state:'dismissed',revision:'2'.repeat(64)}:x);return {data:{id:r.id,state:'dismissed',revision:'2'.repeat(64)}};},read:async()=>asks(rows)};
  view.dismiss([a,b,c],cx);await cx.drain();assert.deepEqual(actions,['1','2']);assert.equal(view.busy,false);assert.equal(view.current().id,'2');assert.match(view.notice,/not applied/);
});
test('dismiss middle ask remains in session before progressing to another session',async()=>{
  const a=row(1),b=row(2),other=row(9,'other'),{cx,view}=setup([a,b,other]);let rows=[a,b,other];
  view.bridge={action:async(_machine,r)=>{rows=rows.map(x=>x.id===r.id?{...x,state:'dismissed',revision:'2'.repeat(64)}:x);return {data:{id:r.id,state:'dismissed',revision:'2'.repeat(64)}};},read:async(_machine,kind,r)=>kind==='asks'?asks(rows):{data:{id:r.id,revision:r.revision,latest:{text:'Question'}}}};
  view.dismiss([a],cx);await cx.drain();assert.equal(view.current().id,'2');
  view.dismiss([b],cx);await cx.drain();assert.equal(view.current().id,'9');
});
test('Escape closes a peek and never changes the selected tab',()=>{
  const a=row(1),{cx,view}=setup([a]);view.selection.peek=rowKey(a);view.context={value:{messages:[]}};
  const rendered=view.render(cx);rendered.handlers.key({key:'escape',keystroke:'escape'},cx);
  assert.equal(view.tab,'asks');assert.equal(view.selection.peek,null);
  rendered.handlers.key({key:'escape',keystroke:'escape'},cx);assert.equal(view.tab,'asks');
});

test('Review asks selects the conversation when launch cwd is beneath its workspace',async()=>{
 const a={...row(1,'first'),workspace:'/invented/repo'},other=row(2,'other');
 const {cx,view}=setup([a,other]);view.refresh=()=>{};
 view.bridge={read:async()=>({data:{id:a.id,revision:a.revision,latest:{text:'Question'}}})};
 view.showSessionAsks({host,machine:null,provider:'claude',id:'first',cwd:'/invented/repo/subdir',workspace:'/invented/repo'},cx);
 await cx.drain();view.reconcile(cx);await cx.drain();
 assert.equal(view.current().id,a.id);assert.ok(view.groupList().some(g=>g.rows.some(r=>r.id===a.id)));
 assert.equal(view.query,'');
});

test('appearance choice is remembered and leaves ask selection intact',t=>{
  const values = new Map();
  globalThis.localStorage = {
    getItem: key => values.get(key),
    setItem: (key, value) => values.set(key,value),
  };
  t.after(()=>delete globalThis.localStorage);

  const a=row(1), {cx,view}=setup([a]);
  assert.equal(view.appearance,'Herdr');
  view.appearanceOpen = true;
  const choices = view.appearancePicker().handlers.content.handlers.children;
  choices[1].handlers.click({},cx);
  assert.equal(view.appearance,'Light');
  assert.equal(view.appearanceOpen,false);
  assert.equal(view.current().id,a.id);
  assert.equal(setup([a]).view.appearance,'Light');
});

test('a collapsed chain reads the whole chain and marks where each session begins',async()=>{
  const {cx,view}=setup([row(1)]);
  const id='00000002-0000-4000-8000-000000000000';
  const sessions=[{provider:'claude',conversation_id:'00000000-0000-4000-8000-000000000000',started_ms:1,available:false},
    {provider:'codex',conversation_id:'00000001-0000-4000-8000-000000000000',started_ms:2,available:true},
    {provider:'claude',conversation_id:id,started_ms:3,available:true}];
  const msg=(session,n)=>({key:`s${session}-m${n}-0-${'a'.repeat(16)}`,role:'user',text:'t',at_ms:n,session});
  const seen=[];
  view.bridge={read:async(_machine,kind,options)=>{seen.push(options);return {data:{sessions,messages:[msg(1,0),msg(1,1),msg(2,2)],cursor:{before:'',after:''}},omitted:{sessions:1}};}};
  const chained={kind:'session',host,machine:null,provider:'claude',id,readChain:true,chain:{key:'a'.repeat(16),seq:3,size:3,members:[]}};
  view.openConversation(chained,cx);await cx.drain();
  view.openConversation(chained,cx,{before:'s1-m0-0-'+'a'.repeat(16)});await cx.drain();
  view.openConversation({...chained,readChain:false},cx);await cx.drain();
  assert.equal(seen[0].chain,true);assert.equal(seen[1].chain,true);assert.equal(seen[1].before,'s1-m0-0-'+'a'.repeat(16));assert.equal(seen[2].chain,undefined);
  assert.equal(view.conversation.omissions,'1 earlier session could not be read');
  // At the start of the chain: dividers for the unread first session, the codex one and
  // the last; a later window names only the sessions it holds.
  const list=view.messageList({sessions,messages:[msg(1,0),msg(1,1),msg(2,2)],cursor:{before:''}},view.scroll,'conversation');
  assert.equal(list.handlers.children.length,3+3);
  const later=view.messageList({sessions,messages:[msg(1,1),msg(2,2)],cursor:{before:'s1-m0-0-'+'a'.repeat(16)}},view.scroll,'conversation');
  assert.equal(later.handlers.children.length,2+2);
  const single=view.messageList({messages:[{key:'m0-0-'+'a'.repeat(16),role:'user',text:'t',at_ms:1}]},view.scroll,'conversation');
  assert.equal(single.handlers.children.length,1);
});
