import test from 'node:test';
import assert from 'node:assert/strict';
import {findPeekPassage} from './peek.js';
test('peek locates literal text across prose spans, line breaks and whitespace',()=>{
 const text='Earlier text.\nPlease review the\n invented change before continuing.\nLater text.';
 const match=findPeekPassage(text,'Please review the invented change before continuing.');
 assert.equal(text.slice(match.start,match.end),'Please review the\n invented change before continuing.');
});
test('peek can locate a substantial question line without claiming the whole excerpt',()=>{
 const text='The invented workflow is paused while we wait for a decision.';
 const excerpt='Status:\nThe invented workflow is paused while we wait for a decision.\nOther omitted information.';
 assert.deepEqual(findPeekPassage(text,excerpt),{start:0,end:text.length});
 assert.equal(findPeekPassage(text,'Status:'),null);assert.equal(findPeekPassage(text,'A different question with no matching passage here.'),null);
});
test('peek preserves UTF-16 offsets and refuses unbounded passage input',()=>{
 const text='🌳 İNVENTED release needs a careful review before proceeding.';
 const match=findPeekPassage(text,'İnvented release needs a careful review before proceeding.');
 assert.equal(text.slice(match.start,match.end),'İNVENTED release needs a careful review before proceeding.');
 assert.equal(findPeekPassage('x'.repeat(250001),'x'.repeat(30)),null);
});
