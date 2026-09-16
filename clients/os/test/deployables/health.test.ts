import { describe, expect, it } from "vitest";
import { measuredAvailability, healthExplanation } from "../../src/apps/deployables/health";
import { siteFromRow, statusDotTone, statusTone } from "../../src/apps/deployables/rows";
import { siteStateWord } from "../../src/apps/deployables/words";

const now = Date.now();
const site = siteFromRow({id:"one",hostname:"one.example.com",bundleRef:"blob://one/new",status:"live"});
const healthy = {...site,health:{siteId:"one",hostname:site.hostname,bundleRef:site.bundleRef,checkedAt:new Date(now).toISOString(),state:"reachable",reason:"Website answered successfully",httpStatus:200}};

describe("measured availability",()=>{
  it("never treats a saved live flag as proof",()=>{
    expect(siteStateWord(site)).toBe("Unknown");
    expect(statusDotTone(site)).toBe("unknown");
    expect(statusTone(site)).toBe("muted");
  });
  it("agrees on reachable and failed checks across labels and dots",()=>{
    expect(siteStateWord(healthy)).toBe("Live");
    expect(statusDotTone(healthy)).toBe("reachable");
    const failed={...healthy,health:{...healthy.health,state:"unavailable",httpStatus:404}};
    expect(siteStateWord(failed)).toBe("Unavailable");
    expect(statusDotTone(failed)).toBe("unreachable");
  });
  it("expires success and failure and invalidates a different deployment",()=>{
    expect(measuredAvailability(healthy,now+300_001)).toBe("Unknown");
    expect(measuredAvailability({...healthy,bundleRef:"blob://one/next"},now)).toBe("Unknown");
    expect(measuredAvailability({...healthy,hostname:"new.example.com"},now)).toBe("Unknown");
    expect(measuredAvailability({...healthy,health:{...healthy.health,checkedAt:"garbage"}},now)).toBe("Unknown");
    expect(measuredAvailability(healthy,now-60_000)).toBe("Unknown");
    expect(healthExplanation(healthy,now+300_001)).toContain("overdue");
  });
  it("preserves intentional lifecycle states even with an old successful check",()=>{
    expect(siteStateWord({...healthy,status:"draft"})).toBe("Built");
    expect(siteStateWord({...healthy,status:"disabled"})).toBe("Offline");
    expect(siteStateWord({...healthy,status:"archived"})).toBe("Archived");
  });
});
