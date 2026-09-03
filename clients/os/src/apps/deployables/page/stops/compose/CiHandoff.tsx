import { Caption, CopyValue, Fact, Facts } from "../../../../../kit";

// What a CI-pushed deployable hands back (epic memql#4885, design section C).
//
// ===========================================================================
// THREE THINGS, EACH SEPARATELY COPYABLE
// ===========================================================================
// Somebody reading this is a tab away from a CI configuration file, and what
// they need there is an id, a URL and a shell command -- three values that
// go to three different places. The Domains panel learned this first (its two
// DNS records render as Type / Name / Value, each copyable, rather than as
// one line somebody then has to take apart), and the reasoning is the same
// here: a single "copy all" hands over a block that has to be dismantled.
//
// ===========================================================================
// THE COMMAND IS THE DOCUMENTED ONE, VERBATIM
// ===========================================================================
// `memql service-account-token mint` on the identity binary, spelled exactly
// as docs/public/operate/auth/service-account-jwt.md and site-hosting.md
// spell it -- including the `kubectl exec`, because the mint is unauthenticated
// by design and safe only from inside the identity pod. A paraphrase here
// would be a command that does not run, pasted by somebody with no way to
// know which half this build invented.

/** The bundle route, on the API host rather than on the site's own. */
export function bundleRouteFor(siteId: string, clusterDomain: string): string {
  const domain = clusterDomain.trim().toLowerCase();
  const host = domain === "" ? "api.<your cluster domain>" : `api.${domain}`;
  return `POST https://${host}/sites/${siteId}/bundles`;
}

/** The mint, as the runbooks spell it. `--label` is required; the subject names the machine principal. */
export function mintCommandFor(name: string): string {
  const label = slugLabel(name);
  return (
    "kubectl -n memql exec deploy/identity -- " +
    `memql service-account-token mint --label ${label} --subject system:ci-publish`
  );
}

function slugLabel(name: string): string {
  const slug = name
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 40);
  return slug === "" ? "ci-publish" : `${slug}-ci`;
}

export function CiHandoff({
  siteId,
  name,
  clusterDomain,
}: {
  siteId: string;
  name: string;
  clusterDomain: string;
}) {
  return (
    <section className="os-report-part">
      <h4 className="os-report-heading">Where your CI pushes</h4>
      <Facts>
        <Fact label="Site id" value={<CopyValue value={siteId} label="site id" />} />
        <Fact label="Route" value={<CopyValue value={bundleRouteFor(siteId, clusterDomain)} label="bundle route" />} />
        <Fact label="Mint a token" value={<CopyValue value={mintCommandFor(name)} label="mint command" />} />
      </Facts>
      <Caption>
        The route takes multipart form-data, one part per file, named for its path INSIDE the bundle -- the field name,
        not the filename. It answers 201 with the version it produced; log that, it is your rollback record.
      </Caption>
      <Caption>
        The token is a service account&apos;s and lasts an hour with no refresh, so mint one per run rather than
        storing it. A signed-in person&apos;s session is refused here on purpose: this door is for machines.
      </Caption>
    </section>
  );
}
