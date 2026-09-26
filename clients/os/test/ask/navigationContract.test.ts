import { readFileSync, writeFileSync } from "node:fs";
import { expect, it } from "vitest";
import { OS_REGISTRY } from "../../src/apps/registry";
import { navigationCatalog } from "../../src/system/navigation";

it("the engine navigation catalog matches the actual app and section registry", () => {
 const path = process.cwd() + "/../../component/memql/os_navigation.json";
 const catalog = navigationCatalog(OS_REGISTRY);
 if (process.env.MEMQL_UPDATE_NAVIGATION === "1") writeFileSync(path, JSON.stringify(catalog, null, 2) + "\n");
 expect(JSON.parse(readFileSync(path,"utf8")), "Regenerate with MEMQL_UPDATE_NAVIGATION=1 npm test -- test/ask/navigationContract.test.ts").toEqual(catalog);
 expect(catalog.length).toBeGreaterThan(10);
 for (const app of catalog) for (const record of app.records) {
  expect(app.sections.some(section => section.id === record.section)).toBe(true);
  expect(record.query).toMatch(/^[a-zA-Z]+\.[a-zA-Z]+$/);
  expect(record.idField).toMatch(/Id$/);
 }
});
