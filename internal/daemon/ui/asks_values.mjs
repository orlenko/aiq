// Invented snapshot facts; shared by component fixtures, never a live reader.
export const sourceID = '00000001-0000-4000-8000-000000000001';
export const scope = () => ({mode:'directory',dir:'/invented/project'});
export const limits = () => ({arrayEntries:1000,stringUnits:50000,rows:20});
export const firstCopy = '> original **quote**\n>\n> literal <img fixture>\n\n';
export const latestCopy = '> latest **quote**\n>\n> exact revised wording\n\n';
const time = () => ({ms:0,known:false});
const line = (kind,t,c=[],d=false,table_rows=0) => ({kind,table_rows,spans:[{c,d,t}]});
export function item(extra={}) {
  return {id:'9007199254740993',provider:'agy',session_id:sourceID,workspace:'/invented/project',
    state:'open',raised_at:{ms:0,known:true},state_at:time(),repeats:2,state_reason:'',state_text:'',
    refs:['https://example.invalid/inert-reference'],
    first:{lines:[line('rule','---',[],true),line('code','original code '+ 'ABCDEFGHIJKLMNOPQRSTUVWXYZ'.repeat(12)),
      line('table','A | B | '+ 'wide-cell-'.repeat(25),[],false,2),line('quote','original quote'),
      line('text','<img src=x onerror=fixture> literal **marks**',['b','b'])],copy:firstCopy},
    latest:{lines:[line('code','latest code '+ 'ZYXWVUTSRQPONMLKJIHGFEDCBA'.repeat(12)),
      line('quote','latest quote'),line('text','revised <script>fixture</script>',['i'])],copy:latestCopy},...extra};
}
export function snapshot() {
  return {scope:scope(),sections:[
    {key:'asks',items:[item(),item({provider:'unsupported',session_id:'unavailable'})]},
    {key:'warnings',items:[item({id:'2'})]},
    {key:'other_open',items:[item({id:'3'})]},
    {key:'probably_done',items:[item({id:'4',state:'probably-done',state_reason:'agent',state_text:'stored explanation'})]},
    {key:'closed_dismissed',items:[item({id:'5',state:'dismissed',state_reason:'person',state_text:'stored decision'})]},
  ],report:{known:true,pass_at:time(),ok_at:{ms:0,known:true},error_at:{ms:-42,known:true},
    next_try_at:{ms:Number.MAX_SAFE_INTEGER,known:true},watched:1,waiting:2,skipped:3,calls_today:4,
    last_error:'stored <error> text',not_watched:['invented stored reason']}};
}
