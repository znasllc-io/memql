// Turning `sitePublishFromArtifact`'s machine-readable refusal into a sentence
// somebody can act on.
//
// ===========================================================================
// WHY A TABLE AND NOT THE SERVER'S OWN STRING
// ===========================================================================
// Every refusal path in `integrations/library/site_publish.go` names itself
// with a STABLE reason -- `artifact_not_a_zip`, `bundle_missing_index` -- and
// that is deliberate on both ends: the audit row records the reason so it can
// be searched, and a surface renders it so nobody is shown a token. The
// server's `Detail` half is prose written for an operator reading a log and may
// be edited at any time; the REASON is the contract.
//
// So this module keys on the reason and writes the sentence itself. What it
// must never do is print the token: "artifact_not_a_zip" tells somebody who
// picked the wrong file nothing about what to do next.
//
// THE TOKEN ARRIVES INSIDE A WRAPPED MESSAGE -- the engine hands the SDK an
// error string, `executeNamed` prefixes it with the call name, and the
// capability's own Error() renders `sitePublishFromArtifact refused: <reason>
// -- <detail>`. Rather than parse that exact composition, which would break the
// first time a layer added a prefix, this scans the whole message for any known
// reason as a WHOLE WORD. The reasons are underscored identifiers that appear
// in no English sentence, so the scan cannot collide with prose.
//
// AN ERROR CARRYING NO KNOWN REASON IS NOT A REFUSAL: it is a transport
// failure, an unauthenticated stream, an engine fault. Those fall through with
// their own message intact, because inventing a friendly sentence for an
// unknown failure is how a real fault gets mistaken for a user error.
//
// It is a SECOND COPY of the portal's table (clients/portal/src/deployables),
// and deliberately not an import: the two clients share no package and
// `clients/` surfaces are independently deletable by design. The reasons
// themselves come from the Go file, which is the one both copies answer to.

export const PUBLISH_REFUSALS: Readonly<Record<string, string>> = {
  missing_argument:
    "The publish call was incomplete -- it named no deployable or no bundle. Pick a bundle and try again.",

  // The two "not found" reasons cover being REFUSED as much as being absent,
  // and saying so is the honest form: both reads are owner-scoped, so somebody
  // else's row resolves to zero rows rather than to a permission error. A
  // message that only said "does not exist" would send a person looking for a
  // typo that is not there.
  site_not_found:
    "This deployable is not one you can publish to. It may have been deleted, or it may belong to someone else.",
  artifact_not_found:
    "That bundle is not in your Library any more. It may have been archived or deleted, or it may belong to someone else.",

  artifact_archived: "That bundle is archived. Restore it in the Library, or pick another one.",
  artifact_not_a_file:
    "That Library entry has no file behind it -- a note, a to-do or a memory is a record, not bytes. Pick an uploaded zip.",
  file_not_found: "The file behind that Library entry is gone, so there is nothing to publish.",
  file_archived: "The file behind that Library entry is archived, so there is nothing to publish.",
  artifact_not_a_zip:
    "A deployable's bundle has to be a zip. Zip the built site -- index.html at the top level, not inside a folder -- and upload that.",

  // A deployment fault rather than anything the person did. Named as one, so
  // they stop trying and tell an operator.
  storage_not_configured:
    "This cluster has no object storage configured, so a bundle cannot be read or published. That is an operator setting, not something to retry.",

  bundle_unreadable:
    "The bundle's bytes could not be read from storage. Try again; if it keeps failing, the upload may not have completed.",
  bundle_not_a_zip:
    "The file is not a readable zip, whatever its type says. Re-zip the built site and upload it again.",
  bundle_path_invalid:
    "The zip contains a path that escapes its own root (a `..` entry or an absolute path). It was refused rather than cleaned up. Re-zip from inside the built directory.",
  bundle_too_many_files:
    "The bundle has more than 20,000 files. Build it for production, and leave source maps and node_modules out of the zip.",
  bundle_file_too_large:
    "One file inside the bundle is over 25 MB. Move anything that big out of the bundle.",
  bundle_too_large: "The bundle expands to more than 500 MB.",
  bundle_empty: "The zip has no files in it.",
  bundle_missing_index:
    "The bundle has no index.html at its top level. That usually means the zip contains a folder that contains the site -- zip the CONTENTS of the build directory, not the directory.",

  // The publish itself failed AFTER validation passed. Worth saying plainly
  // that nothing changed, because the natural fear here is a half-deployed
  // site: the publisher writes a whole new version prefix and only then flips
  // the row, so a failure leaves the deployable serving exactly what it was.
  publish_failed:
    "The bundle was valid but the publish did not complete. Nothing changed -- this deployable is still serving what it was serving. Try again.",
};

/** The stable reason carried anywhere in a message, or "" when there is none. */
export function publishRefusalReason(message: string): string {
  for (const reason of Object.keys(PUBLISH_REFUSALS)) {
    // WHOLE WORD: `bundle_too_large` must not match inside
    // `bundle_too_large_something`, and the reasons share prefixes with each
    // other (`artifact_not_a_file` / `artifact_not_a_zip`).
    if (new RegExp(`(^|[^a-z_])${reason}([^a-z_]|$)`).test(message)) return reason;
  }
  return "";
}

/** What a surface renders: a known refusal becomes its sentence, anything else
 *  keeps its own message. */
export function describePublishFailure(err: unknown): string {
  const message = err instanceof Error ? err.message : String(err);
  const reason = publishRefusalReason(message);
  const known = reason === "" ? undefined : PUBLISH_REFUSALS[reason];
  return known ?? message;
}
