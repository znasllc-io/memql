import { describe, expect, it } from "vitest";
import { addressFromManifest, configuredManifest, domainNames, manifestYaml } from "../../src/apps/deployables/packages/manifest";
import { placementsFrom } from "../../src/apps/deployables/page/compose";
import { placementsPayload } from "../../src/apps/deployables/packages/calls";
import type { PackageManifest, ReportDeployable } from "../../src/apps/deployables/packages/rows";

const app: ReportDeployable = {name:"storefront",displayName:"Fylo Storefront",kind:"shopify_storefront",path:"clients/storefront",output:"dist",buildPlan:"npm run build",prebuilt:false,deployment:{slug:"quiet-cedar",domains:["shop.example.com","www.example.com"]}};
const manifest: PackageManifest = {formatVersion:1,name:"fylo",deployables:[{name:app.name,displayName:app.displayName,path:app.path,kind:app.kind,deployment:app.deployment,binding:{store:"fylo.myshopify.com"},build:{command:"npm ci && npm run build",output:"dist"},assets:[{path:"video.mp4",source:"https://example.com/video.mp4",sha256:"a".repeat(64),size:12}]}]};

describe("manifest deployment configuration", () => {
  it("prefills the wizard and sends all domains under the stable identity", () => {
    const draft=addressFromManifest(app,"organization");
    expect(draft).toEqual({slug:"quiet-cedar",accountId:"organization",ownDomain:"shop.example.com, www.example.com"});
    const wire=placementsPayload(placementsFrom([app.name],{[app.name]:draft},"memql.localhost"));
    expect(wire).toEqual({storefront:{hostname:"quiet-cedar.memql.localhost",accountId:"organization",domains:["shop.example.com","www.example.com"]}});
  });
  it("sends explicit opt-out after clearing manifest domains", () => {
    const draft={...addressFromManifest(app,"organization"),ownDomain:""};
    expect(placementsPayload(placementsFrom([app.name],{[app.name]:draft},"memql.localhost"))["storefront"]?.["domains"]).toEqual([]);
  });
  it("generates only an omitted address and preserves unrelated declarations on export", () => {
    const draft=addressFromManifest(undefined,"organization");
    expect(draft.slug).toMatch(/^[a-z]+-[a-z]+$/);
    const exported=configuredManifest(manifest,{storefront:draft},[],"memql.localhost");
    expect(exported.deployables[0]).toEqual({...manifest.deployables[0],deployment:{slug:draft.slug,domains:[]}});
    expect(manifest.deployables[0]?.deployment?.slug).toBe("quiet-cedar");
    const yaml=manifestYaml(exported);
    expect(yaml).toContain('displayName: "Fylo Storefront"');
    expect(yaml).toContain('store: "fylo.myshopify.com"');
    expect(yaml).toContain('path: "video.mp4"');
    expect(yaml).toContain(`slug: "${draft.slug}"`);
  });
  it("exports the actual existing address after redeploy, never a different default", () => {
    const exported=configuredManifest(manifest,{},[{name:"storefront",siteId:"same-site",hostname:"existing.memql.localhost",bundleRef:"blob://x",version:"v",created:false}],"memql.localhost");
    expect(exported.deployables[0]?.deployment?.slug).toBe("existing");
  });
  it("normalizes a list without throwing away malformed values before validation", () => {
    expect(domainNames("SHOP.EXAMPLE.COM, www.example.com\nshop.example.com")).toEqual(["shop.example.com","www.example.com"]);
  });
});
