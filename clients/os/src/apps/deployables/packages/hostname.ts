// The Packages surface reuses the app's OWN slug rules rather than restating
// them (`../hostname.ts`): the create-site form and the first-deploy picker are
// choosing the same kind of name for the same kind of row, and two copies of a
// mirrored policy is two chances to disagree with the Go that decides.
//
// The SUGGESTION that used to live here -- the app's own name, slugified --
// was retired on 2026-09-05: every source declares `storefront` and `web`, so
// the first person on a cluster took those names and the second found them
// taken at the end of the flow. An address now starts as a generated name
// (`nickname.ts`, through `compose.ts`'s `seedAddress`).

export { hostnameFor } from "../hostname";
