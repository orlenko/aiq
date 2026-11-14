// Public invented transport facts, shared by Node and native-browser fixtures.
export const fixtureScope = {provider:'agy', id:'00000001-0000-4000-8000-000000000001'};
export const unknownTime = () => ({ms:0, known:false});
export function fixtureHead(mode = '') {
  return {mode, who: mode === 'prompt' ? 'you' : 'agy', clock_mode:'', clock_text:'', at:unknownTime(),
    duration_known:false, duration_ms:0, tools:0};
}
export const fixtureLine = (t, kind = 'text', c = []) => ({kind, table_rows:0, spans:[{t,c,d:false}]});
export function fixtureGroup(ordinal, head = fixtureHead(), lines = [fixtureLine('body')]) {
  return {ordinal, visible:true, leading_break:false, empty_body_line:false, head, base:[], lines};
}
export function fixtureMessage(n, groups) {
  return {id:n.toString(16).padStart(32,'0'), key:'a'.repeat(64), kind:'you', sub:'prompt', turn:0,
    at:unknownTime(), source_known:true, source:'human', account_known:false, account:'', raw:'raw **body**', groups};
}
export function fixtureWindow(messages = [fixtureMessage(1,[fixtureGroup(0,fixtureHead('prompt'))])]) {
  return {build:'invented-build', session:{...fixtureScope}, earlier_hidden:0, later_hidden:0,
    window_start:0, reset:true, markup:false, messages};
}
