import {createQuotaView} from './quota_view.js';

// Invented values only. The reader is held open so a driver can settle,
// fail or overtake each read; nothing leaves the page.
const now=Math.floor(Date.now()/1000);
const win=(extra={})=>({key:'five_hour',label:'5h',kind:'short',scope:'',binding:true,used_pct:83,resets_at:now+7500,window_seconds:18000,source:'probe',observed_at:now-300,...extra});
const login=(extra={})=>({has_credential:true,cli:'in',cli_identity:'',cli_mismatch:false,checked_at:now-900,problem:'',...extra});
const account=(id,provider,extra={})=>({id,provider,plan:'max',label:'',identity:id.replace('/','.')+'@invented.test',enabled:true,login:login(),
  eligible:true,ineligible:'',exhausted:false,reason:'',cooldown_until:null,rank:1,score:3.5,rung:0,terms:['5h 83% used','7d unknown'],
  reset_credits:0,reset_credit_expires_at:null,extra_usage:null,poll_error:'',observed_at:now-60,
  windows:[win(),win({key:'seven_day',label:'7d',kind:'weekly',used_pct:null,resets_at:null,binding:false,scope:'invented-model'})],
  leases:[{mode:'long',drain:'',depth:0,pane:'%7',pid:11,workspace:'/invented/a/very/long/workspace/path/that/should/wrap/inside/the/card/without/scrolling',cwd:'',why:''}],...extra});
export function inventedQuotas(){return {schema:1,generated_at:now,hostname:'invented-host-identity',
  poll:{last_at:now-120,interval_s:300,polling:false,error:'invented poll error text'},
  model_scope:[{provider:'claude',scope:'invented-model'}],
  accounts:[
    account('claude/alpha','claude',{extra_usage:{currency:'USD',balance_minor:1234,used_minor:250,limit_minor:1000,limit_reached:false,note:'invented note'},reset_credits:1,reset_credit_expires_at:now+86400}),
    account('claude/beta','claude',{rank:2,score:null,rung:1,login:login({cli:'out'}),eligible:false,ineligible:'invented: not logged in',
      extra_usage:{currency:'JPY',balance_minor:null,used_minor:300,limit_minor:0,limit_reached:true,note:''}}),
    account('claude/gamma','claude',{exhausted:true,eligible:false,reason:'invented: 5h at 100%',windows:[win({used_pct:100})],leases:[],
      login:login({cli:'unreadable',problem:'invented keychain error text'}),poll_error:'invented HTTP 500 body'}),
    account('codex/delta','codex',{cooldown_until:now+600,leases:[{mode:'worker',drain:'requested',depth:2,pane:'',pid:4242,workspace:'',cwd:'/invented/cwd',why:'invented why line one\ninvented why line two'}]})],
  rankings:[
    {key:'claude/interactive',entries:[{id:'claude/alpha',score:3.5,rung:0,eligible:true,reason:'',terms:['5h 83% used']},{id:'claude/beta',score:null,rung:0,eligible:false,reason:'invented: not logged in',terms:[]}]},
    {key:'claude/worker',entries:[]},
    {key:'codex/interactive',entries:[{id:'codex/delta',score:1.25,rung:2,eligible:true,reason:'',terms:['invented term']}]}]};}

const reads=[],copies=[],announcements=[];
const view=createQuotaView(document,{window,limits:{arrayEntries:1000,stringUnits:100000,rows:100},
  readQuotas:(query,signal)=>new Promise((resolve,reject)=>{reads.push({query,signal,resolve,reject});}),
  announce:text=>announcements.push(text),copyText:text=>{copies.push(text);}});
document.getElementById('fixture').append(view.fragment);
window.quotaFixture={view,reads,copies,announcements,inventedQuotas,
  settle:(i,data=inventedQuotas())=>reads[i].resolve({data}),fail:i=>reads[i].reject(new Error('invented'))};
