import {createAsksView} from './asks_view.js';
import {snapshot,scope,limits} from './asks_values.mjs';

const fixture=document.getElementById('fixture');
const copies=[],announcements=[],navigations=[];
const options={window,expectedScope:scope(),limits:limits(),
  copyText:text=>{copies.push(text);},announce:text=>announcements.push(text),
  onConversation:value=>navigations.push(value)};
function attach(idPrefix) {
  const view=createAsksView(document,snapshot(),{...options,idPrefix});
  fixture.append(view.fragment);
  if(!view.mount())throw Error('invented fixture activation failed');
  return view;
}
const first=attach('fixtureFirst'),second=attach('fixtureSecond');
// Fixture-only diagnostics use invented text and an in-memory copy hook.
function attachTotals(totals,font='') {
  const data=snapshot();data.sections.forEach(s=>{s.items=[];});data.report={known:false};data.totals=totals;
  const view=createAsksView(document,data,{...options,idPrefix:'fixtureTotals'});
  fixture.append(view.fragment);fixture.lastElementChild.style.fontFamily=font;
  if(!view.mount())throw Error('invented totals fixture activation failed');
  return view;
}
window.asksFixture={first,second,copies,announcements,navigations,attachTotals};
