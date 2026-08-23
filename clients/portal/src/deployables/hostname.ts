// The hostname policy, mirrored client-side for LIVE validation.
//
// ===========================================================================
// THE SERVER IS THE AUTHORITY. THIS IS A KEYSTROKE-RATE ANSWER, NOT A GATE.
// ===========================================================================
//
// component/memql/platform_site_hostname_policy.go decides, on every write,
// whether a caller may claim a hostname: the <slug>.<domain> shape, the slug
// bounds, the derived reserved set, cluster-wide uniqueness, and the
// cluster-owner exemption. None of it is expressible in a mutation body, and a
// UI-only check is not a check -- its own header says so.
//
// What this module buys is the half the server cannot: an answer while the
// person is still typing. "shop" is fine and "api" is not is a fact worth
// having before the submit, not after it.
//
// TWO RULES ARE DELIBERATELY NOT MIRRORED, because a browser cannot answer
// them and pretending otherwise would be worse than saying nothing:
//
//   * UNIQUENESS. It needs a read the caller may not even be allowed to make
//     -- the Go probe reads WITHOUT row-authz narrowing precisely so a
//     hostname another user holds still collides. So a taken slug passes here
//     and is refused on submit, and the create form surfaces that refusal
//     verbatim.
//   * THE CLUSTER-OWNER EXEMPTION. An operator may claim a custom apex or a
//     second domain. That hostname needs its own DNS record and its own
//     Certificate, neither of which the portal can create, so this form does
//     not offer it at all -- see DeployablesPage's own note.
//
// The reserved set below is a COPY of a set the Go side DERIVES
// (frontdoor.Roles() + PortalSite + the squat list). A copy of a derived set
// is exactly the shape that goes stale when a role is added, so it is written
// to fail in the safe direction: a label this list forgets is still refused by
// the server, and the person sees the server's message rather than a silent
// success. Do not add a label here without adding it to squatReservedSiteLabels
// -- and if you are adding a ROLE, add it to frontdoor.Roles() and nothing
// else, because that is the list the server derives from.

export const SLUG_MIN_LENGTH = 3;
export const SLUG_MAX_LENGTH = 40;

// Lowercase alphanumerics and hyphens, 3-40 characters. Narrower than DNS on
// purpose (a label may be 63 characters and may carry uppercase in its
// presentation form): the slug is a display handle too, and a case-folded
// collision -- "Shop" against "shop" -- would resolve to one site while
// reading as two rows.
export const SLUG_PATTERN = /^[a-z0-9-]{3,40}$/;

// The front-door roles, the platform's own site, and the three squat labels.
export const RESERVED_LABELS: readonly string[] = [
  "api",
  "identity",
  "mcp",
  "portal",
  "www",
  "admin",
  "mail",
];

// hostnameFor composes the hostname a slug will claim. One label under the
// cluster's domain, because the front door routes every site through one
// `*.<domain>` Ingress rule and a wildcard matches exactly one label.
export function hostnameFor(slug: string, domain: string): string {
  const label = slug.trim().toLowerCase();
  const under = domain.trim().toLowerCase();
  if (label === "" || under === "") return "";
  return `${label}.${under}`;
}

// validateSlug returns "" when the slug is claimable as far as a browser can
// tell, or the sentence to show the person.
//
// The messages name the RULE and the reason, not just the failure: "3 to 40
// characters" is actionable, "invalid" is not.
export function validateSlug(slug: string, domain: string): string {
  const label = slug.trim();
  if (label === "") return "";
  if (label !== label.toLowerCase()) {
    return "Use lowercase only. A hostname is case-folded, so Shop and shop would resolve to one site while reading as two.";
  }
  if (label.length < SLUG_MIN_LENGTH || label.length > SLUG_MAX_LENGTH) {
    return `A name is ${SLUG_MIN_LENGTH} to ${SLUG_MAX_LENGTH} characters long; this one is ${label.length}.`;
  }
  if (label.includes(".")) {
    return "One label only. The front door routes sites through a single `*` wildcard, which matches exactly one label -- a name with a dot in it would resolve nowhere.";
  }
  if (!SLUG_PATTERN.test(label)) {
    return "Use lowercase letters, digits and hyphens only.";
  }
  if (RESERVED_LABELS.includes(label)) {
    return `"${label}" is reserved${domain.trim() === "" ? "" : ` -- ${hostnameFor(label, domain)} is where this cluster serves the platform itself`}. Reserved names: ${RESERVED_LABELS.join(", ")}.`;
  }
  return "";
}
