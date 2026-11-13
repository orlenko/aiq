// Invented native-browser acceptance fixture, served only by a test allowlist.
import {createConversationFeed} from './feed.js';
import {fixtureScope, fixtureHead, fixtureLine, fixtureGroup, fixtureMessage, fixtureWindow} from './feed_values.mjs';
const result=document.getElementById('result'), mount=document.getElementById('fixture'), neighbor=document.getElementById('neighbor');
const checks=[];
function check(ok,label) {if(!ok)throw new Error(label);checks.push(label);}
const frame=()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
const options=(idPrefix,copyText=()=>Promise.resolve(),announce=()=>{})=>({window,idPrefix,expectedScope:fixtureScope,copyText,announce});
try {
  const hostile='<img src=x onerror=alert(1)><svg onload=alert(2)>inert</svg>';
  const earlier=fixtureGroup(0,fixtureHead('earlier'),[{kind:'text',table_rows:0,spans:[
    {t:'plain',c:[],d:false},{t:'bold',c:['b'],d:false},{t:'code',c:['code'],d:false},
    {t:'plain',c:[],d:false},{t:'accent',c:['accent'],d:false},{t:'plain',c:[],d:false}]}]);earlier.base=['muted'];
  const code=fixtureGroup(1,{...fixtureHead('reply'),clock_mode:'reply',clock_text:'reply'},[fixtureLine('long'.repeat(80),'code'),fixtureLine('next','code')]);
  const notice=fixtureGroup(2,fixtureHead('notice'),[fixtureLine('· peer agent: notice '+hostile)]);
  const human=fixtureGroup(3,fixtureHead('prompt'),[fixtureLine('prompt')]);
  const final=fixtureGroup(4,{...fixtureHead('final'),duration_known:true,duration_ms:0,tools:1},[fixtureLine('(no reply)')]);
  const data=fixtureWindow([fixtureMessage(1,[earlier,code,notice]),fixtureMessage(2,[human,final])]);
  let resolveCopy,announced=0,copied='';data.messages[0].raw='aside\n\nhidden\ncopy';
  const feed=createConversationFeed(document,data,options('native',raw=>{copied=raw;return new Promise(r=>resolveCopy=r);},()=>announced++));
  const second=createConversationFeed(document,fixtureWindow(),options('second'));
  neighbor.focus();check(document.activeElement===neighbor,'initial focus');
  mount.append(feed.fragment,second.fragment);check(feed.mount()&&second.mount(),'two connected feeds mount');
  await document.fonts.ready;await frame();
  const roots=mount.querySelectorAll('.nav-feed');check(roots.length===2,'two owned regions');
  const first=roots[0],spans=first.querySelectorAll('.nav-md')[0].querySelectorAll('.nav-md-line > span');
  check([...spans].map(n=>getComputedStyle(n).color).join('|')==='rgb(91, 92, 93)|rgb(91, 92, 93)|rgb(11, 12, 13)|rgb(91, 92, 93)|rgb(21, 22, 23)|rgb(91, 92, 93)','native base/span foreground reset');
  check(Number(getComputedStyle(spans[1]).fontWeight)>=700,'native bold inheritance');
  check(getComputedStyle(first.querySelectorAll('.nav-md')[1]).color==='rgb(1, 2, 3)','next cut base isolation');
  check(first.querySelectorAll('h3').length===1&&first.querySelector('h3').textContent==='you','prompt h3 only');
  check([...first.querySelectorAll('.nav-feed-answer')].map(n=>n.textContent).join('|')==='agy · earlier|agy · reply|agy · 0s · 1 tool','per-group reply/final headings');
  check(first.textContent.split('· peer agent: notice').length===2,'notice painted once');
  check(!first.querySelector('img,svg,script,style,iframe'),'hostile text inert in native DOM');
  check(first.getAttribute('aria-label')==='Conversation'&&first.querySelector('.nav-feed-copy').getAttribute('aria-describedby')!==null,'region and copy association');
  const scroll=first.querySelector('.nav-md-scroll');check(scroll.scrollWidth>scroll.clientWidth&&scroll.getAttribute('tabindex')==='0','native connected overflow activation');
  scroll.focus();document.body.classList.add('fixture-zoom');feed.refresh();await frame();
  check(window.visualViewport.scale>=1.9&&document.activeElement===scroll&&scroll.getAttribute('tabindex')==='0','native 2x pinch zoom/text reflow keeps horizontal focus');
  const button=first.querySelector('.nav-feed-copy');button.focus();button.click();button.click();await Promise.resolve();
  check(document.activeElement===button&&button.getAttribute('aria-disabled')==='true'&&copied==='aside\n\nhidden\ncopy','copy busy retains focus and exact raw');
  resolveCopy();await Promise.resolve();await Promise.resolve();
  check(announced===1&&button.getAttribute('aria-disabled')===null&&document.activeElement===button,'copy settlement retains focus');
  check(getComputedStyle(neighbor).color==='rgb(71, 72, 73)','neighbor style sentinel');
  let trustedTypesBlocked=false;try {document.createElement('div').innerHTML='<b>fixture</b>';}catch(error){trustedTypesBlocked=error instanceof TypeError;}
  check(trustedTypesBlocked,'actual CSP Trusted Types enforcement');
  // The isolated CDP driver takes the real accessibility tree while connected.
  window.feedFixtureAXReady=true;
  while(!window.feedFixtureAXDone) await new Promise(resolve=>setTimeout(resolve,10));
  check(window.feedFixtureAXVerified===true,'native accessibility region/heading/copy roles');
  check(window.feedFixtureKeyboardVerified===true,'native Tab and horizontal arrow keys');
  const faults=fixtureWindow([fixtureMessage(1,[fixtureGroup(0,fixtureHead(),[fixtureLine('a','code')]),fixtureGroup(1,fixtureHead(),[fixtureLine('b','code')])])]);
  const bad=createConversationFeed(document,faults,options('failure'));mount.append(bad.fragment);
  const original=window.ResizeObserver;let observe=0,disconnected=0;
  window.ResizeObserver=class {observe(){if(++observe===2)throw new Error('invented observer fault');}disconnect(){disconnected++;}};
  let failed=false;try{bad.mount();}catch{failed=true;}finally{window.ResizeObserver=original;}
  check(failed&&disconnected===2&&!mount.querySelector('#failure-00000000000000000000000000000001'),'native partial mount rollback');
  neighbor.focus();feed.teardown();feed.teardown();await frame();
  check(second.mount()&&document.activeElement===neighbor&&roots[1].isConnected,'teardown isolates neighbor and second feed');
  second.teardown();check(mount.children.length===0,'native owned roots removed');
  result.textContent='PASS '+checks.length+' '+checks.join('; ');result.setAttribute('data-result','pass');
} catch { result.textContent='FAIL '+checks.join('; ');result.setAttribute('data-result','fail'); }
