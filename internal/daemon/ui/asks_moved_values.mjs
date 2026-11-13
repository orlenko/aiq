import {snapshot} from './asks_values.mjs';
// Invented full-view facts; ordinary duplicate open IDs remain intentionally present.
export function movedSnapshot() {
  const data=snapshot();
  data.sections[3].items[0].state_at={ms:0,known:true};
  Object.assign(data.sections[4].items[0],{state:'closed',state_reason:'rule',state_at:{ms:1000,known:true}});
  data.moved_today={local_day:'1970-01-01',start_at:{ms:0,known:true},ids:['5','4']};
  return data;
}
