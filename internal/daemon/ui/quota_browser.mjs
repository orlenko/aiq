import {createQuotaView} from './quota_view.js';

// Invented values only: 1 agy, 4 Claude and 4 Codex accounts. The reader is
// held open so a driver can settle, fail or overtake each read.
const now=Math.floor(Date.now()/1000);
const win=(extra={})=>({key:'five_hour',label:'5h',kind:'short',scope:'',binding:true,used_pct:83,resets_at:now+7500,window_seconds:18000,source:'probe',observed_at:now-300,...extra});
const week=(extra={})=>win({key:'seven_day',label:'7d',kind:'weekly',used_pct:41,resets_at:now+3*86400,window_seconds:604800,...extra});
const login=(extra={})=>({has_credential:true,cli:'in',cli_identity:'',cli_mismatch:false,checked_at:now-900,problem:'',...extra});
const lease=(extra={})=>({mode:'long',drain:'',depth:0,pane:'%7',pid:11,workspace:'/invented/projects/some-workspace',cwd:'',why:'',...extra});
const account=(id,provider,extra={})=>({id,provider,plan:'max',label:'',identity:id.replace('/','.')+'@invented.test',enabled:true,login:login(),
  eligible:true,ineligible:'',exhausted:false,reason:'',cooldown_until:null,rank:1,score:3.5,rung:0,terms:['5h 83% used','7d 41% used'],
  reset_credits:0,reset_credit_expires_at:null,extra_usage:null,poll_error:'',observed_at:now-60,
  windows:[win(),week()],leases:[],...extra});
export function inventedQuotas(){return {schema:1,generated_at:now,hostname:'invented-host-identity',
  poll:{last_at:now-120,interval_s:300,polling:false,error:''},
  model_scope:[{provider:'claude',scope:'invented-model'}],
  accounts:[
    account('agy/main','agy',{plan:'',windows:[win({used_pct:null,resets_at:null,label:'daily',key:'daily'})],terms:[]}),
    account('claude/alpha','claude',{rank:1,score:3.5,extra_usage:{currency:'USD',balance_minor:1234,used_minor:250,limit_minor:1000,limit_reached:false,note:'spent once plan quota runs out everywhere'},
      reset_credits:1,reset_credit_expires_at:now+86400,windows:[win(),week(),week({key:'seven_day_opus',label:'7d opus',scope:'invented-model',used_pct:12,binding:false})],leases:[lease(),lease({pane:'%9',workspace:'/invented/projects/another'})]}),
    account('claude/beta','claude',{rank:2,score:2.1,login:login({cli:'out'}),eligible:false,ineligible:'logged out'}),
    account('claude/gamma','claude',{rank:3,exhausted:true,eligible:false,reason:'5h at 100% until 14:05',windows:[win({used_pct:100}),week({used_pct:96})],
      login:login({cli:'unreadable',problem:'invented keychain error text'}),poll_error:'invented HTTP 500 body'}),
    account('claude/delta','claude',{rank:4,score:1.2,rung:1,windows:[win({used_pct:20}),week({used_pct:55})]}),
    account('codex/one','codex',{rank:1,score:4.2,windows:[win({used_pct:10}),week({used_pct:30})],leases:[lease({mode:'worker',pane:'',pid:4242,cwd:'/invented/cwd',workspace:'',why:'invented why'})]}),
    account('codex/two','codex',{rank:2,score:3.1,reset_credits:2,windows:[win({used_pct:65}),week({used_pct:70})]}),
    account('codex/three','codex',{rank:3,cooldown_until:now+600,windows:[win({used_pct:null,resets_at:null}),week()]}),
    account('codex/four','codex',{rank:4,enabled:false})],
  rankings:[
    {key:'agy/interactive',entries:[{id:'agy/main',score:null,rung:0,eligible:true,reason:'',terms:[]}]},
    {key:'claude/interactive',entries:[{id:'claude/alpha',score:3.5,rung:0,eligible:true,reason:'',terms:['5h 83% used']},{id:'claude/delta',score:1.2,rung:1,eligible:true,reason:'',terms:['7d 55% used']},{id:'claude/beta',score:null,rung:0,eligible:false,reason:'logged out',terms:[]}]},
    {key:'claude/worker',entries:[]},
    {key:'codex/interactive',entries:[{id:'codex/one',score:4.2,rung:0,eligible:true,reason:'',terms:['5h 10% used']},{id:'codex/two',score:3.1,rung:0,eligible:true,reason:'',terms:['5h 65% used']}]}]};}

const reads=[],copies=[],announcements=[];
const view=createQuotaView(document,{window,limits:{arrayEntries:1000,stringUnits:100000,rows:100},
  readQuotas:(query,signal)=>new Promise((resolve,reject)=>{reads.push({query,signal,resolve,reject});}),
  announce:text=>announcements.push(text),copyText:text=>{copies.push(text);}});
document.getElementById('fixture').append(view.fragment);
window.quotaFixture={view,reads,copies,announcements,inventedQuotas,
  settle:(i,data=inventedQuotas())=>reads[i].resolve({data}),fail:i=>reads[i].reject(new Error('invented'))};
