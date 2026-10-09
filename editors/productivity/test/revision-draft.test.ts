import {test} from "node:test";
import assert from "node:assert/strict";
import {revisionDraftParts} from "../src/revisionDraft.js";

test("partial proposals reveal replacement Markdown and withhold incomplete escapes",()=>{
 const raw=JSON.stringify({summary:'Do not show this',edits:[{before:'Old',after:'# Heading\n\nA "quoted" finding. 😀',reason:'Private request context'}]});
 for(let i=0;i<raw.length;i++){
  const parts=revisionDraftParts(raw.slice(0,i));
  for(const part of parts){assert.ok('# Heading\n\nA "quoted" finding. 😀'.startsWith(part.after));assert.equal(part.before,'Old');assert.ok(!part.after.includes('Private'));}
 }
 assert.deepEqual(revisionDraftParts(raw),[{before:'Old',after:'# Heading\n\nA "quoted" finding. 😀',complete:true}]);
 assert.deepEqual(revisionDraftParts('{"edits":[{"before":"","after":"Line\\nnext\\u26'),[{before:'',after:'Line\nnext',complete:false}]);
 assert.deepEqual(revisionDraftParts('"after\\\":\\\"not a field"'),[]);
 assert.deepEqual(revisionDraftParts('x'.repeat(2*1024*1024+1)),[]);
});
