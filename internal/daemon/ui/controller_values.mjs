// Invented component facts only; no browser I/O or application globals.
export const ids = {
  A:'00000001-0000-4000-8000-000000000001',
  B:'00000002-0000-4000-8000-000000000002',
  C:'00000003-0000-4000-8000-000000000003',
};
export const scope = name => ({provider:'agy', id:ids[name]});
export const limits = () => ({arrayEntries:1000,stringUnits:20000,rows:20});
export const query = (dir='/fixture/A',workers=false) => ({dir,canonicalDir:dir,workers});
export const row = (name='A',extra={}) => ({...scope(name),cwd:`/invented/${name}`,label:`row ${name}`,
  title:`row title ${name}`,model:'fixture-model',branch:'fixture-branch',updated:'literal-date-text',
  turns:2,tools:3,worker:false,bypass:true,...extra});
export const sessions = (dir='/fixture/A',rows=[row()]) => ({dir,roots_searched:['invented-root'],
  partial:true,scanned:3,of:7,unreadable_count:2,sessions:rows});
const time = () => ({ms:0,known:false});
export function detail(name='A',groups=1) {
  return {build:'invented-build',session:{...scope(name),cwd:`/detail/${name}`,title:`detail title ${name}`,
    model:'detail model',branch:'detail branch',worker:false,bypass:true,started:time(),updated:{ms:0,known:true}},
    earlier_hidden:0,later_hidden:0,window_start:0,reset:true,markup:false,messages:[{
      id:'00000000000000000000000000000001',key:'a'.repeat(64),kind:'you',sub:'prompt',turn:0,at:time(),
      source_known:true,source:'human',account_known:false,account:'',raw:`raw ${name}\n\nhidden **copy**`,
      groups:Array.from({length:groups},(_,ordinal)=>({ordinal,visible:true,leading_break:false,empty_body_line:false,
        head:{mode:'prompt',who:'you',clock_mode:'',clock_text:'',at:time(),duration_known:false,duration_ms:0,tools:0},
        base:[],lines:[{kind:'code',table_rows:0,spans:[{c:[],d:false,t:`paint ${name} group ${ordinal}`}]}]}))}]};
}
