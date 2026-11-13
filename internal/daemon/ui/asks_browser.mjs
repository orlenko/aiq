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
window.asksFixture={first,second,copies,announcements,navigations};
