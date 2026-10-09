import {test} from "node:test";
import assert from "node:assert/strict";
import {appearanceSettings, readEditorAppearance} from "../src/editorAppearance.js";
import {EDITOR_COLOR_KEYS} from "../../../brand/editorAppearance.js";

const input = {version:1,mode:"light",colors:Object.fromEntries(EDITOR_COLOR_KEYS.map(k=>[k,"#aabbcc"]))};
test("appearance accepts only a bounded color snapshot",()=>{
  const appearance=readEditorAppearance(JSON.stringify(input));
  assert.equal(appearance?.mode,"light");
  for(const bad of ["{", "x".repeat(2000),JSON.stringify({...input,version:2}),JSON.stringify({...input,mode:"system"}),JSON.stringify({...input,colors:{...input.colors,ground:"url(https://example.com)"}}),JSON.stringify({...input,colors:{}})]) assert.equal(readEditorAppearance(bad),undefined);
});
test("the OS palette reaches chrome and content without changing other themes",()=>{
  for(const mode of ["light","dark"]){
    const appearance=readEditorAppearance(JSON.stringify({...input,mode}))!;
    const existing={"editor.fontSize":19,"[Other Theme]":{"editor.background":"#123456"},[`[MemQL ${mode==="dark"?"Dark":"Light"}]`]:{"custom.color":"#abcdef"}};
    const next=appearanceSettings(appearance,existing);
    const overrides=next.colors[`[${next.name}]`] as Record<string,string>;
    for(const key of ["editor.background","sideBar.background","button.background","textLink.foreground"]) assert.equal(overrides[key],"#aabbcc");
    assert.deepEqual(next.colors["[Other Theme]"],existing["[Other Theme]"]);
    assert.equal(overrides["custom.color"],"#abcdef");
    assert.equal(next.colors["editor.fontSize"],19);
  }
});
