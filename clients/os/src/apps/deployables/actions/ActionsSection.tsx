import { Caption, Head, Panel, Subhead } from "../../../kit";
import { CreateSite } from "./CreateSite";

// The write half, in one section (spec section E: sections carry roles).
//
// PRESENTATION ONLY, and the manifest's `roles: { min: "admin" }` on this
// section says the same thing: `createSite` stamps ownership from the verified
// actor, the Go hostname policy decides every claim, and
// `sitePublishFromArtifact` re-resolves both rows under the caller's own actor
// before it reads a byte. Hiding this section from a reader is a courtesy --
// showing somebody a button that always fails teaches nobody who can use it --
// and never the boundary.
//
// PUBLISHING IS NOT HERE, and that is deliberate. It is a thing you do TO a
// deployable, so it lives on that deployable's detail panel, where the
// hostname, the current bundle and the outcome are all on one screen. A picker
// on this section would make somebody carry an id across the app.

export function ActionsSection({ domain }: { domain: string }) {
  return (
    <div className="os-app-stack">
      <Head title="Actions" />
      <CreateSite domain={domain} />

      <Panel label="Publishing">
        <Subhead>Publishing a bundle</Subhead>
        <Caption>
          Open a deployable from Sites or the Map and publish to it there. Everything about the
          publish -- the bundle it is serving now, where that came from, and what the cluster said
          about the zip -- belongs beside the deployable it happened to.
        </Caption>
        <Caption>
          Pausing, rollback and archiving live on each deployable, under Sites.
        </Caption>
        <Caption>
          A client's own domain lives there too. Open a deployable and use its Domains panel: this
          cluster shows you the two DNS records to create, tells you which one is still wrong until
          both check out, and then provisions the route and the certificate itself.
        </Caption>
      </Panel>
    </div>
  );
}
