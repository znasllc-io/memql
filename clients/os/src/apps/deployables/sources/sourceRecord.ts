import type { PackageRow } from "../packages/rows";
import { shortRepo } from "../packages/rows";
import { bare } from "../people";
import type { SourceConnectionRow } from "./connections";
import { isGithubAppGrant, type CredentialRow } from "./rows";

/** A configured repository is the record. Credentials and installation bindings
 * only supply provenance; they never create entries in Sources. */
export function sourceRecord(pkg: PackageRow, credentials: readonly CredentialRow[], bindings: readonly SourceConnectionRow[]) {
  const credential = credentials.find(row => row.id === pkg.credentialId && bare(row.ownerUserId) === bare(pkg.ownerUserId));
  const binding = bindings.find(row => row.id === pkg.sourceConnectionId && row.credentialId === pkg.credentialId && bare(row.ownerUserId) === bare(pkg.ownerUserId));
  const repository = shortRepo(pkg.repoUrl);
  // The URL names a repository owner, never the identity that authorized access.
  const repositoryOwner = repository.includes("/") ? repository.split("/")[0]! : "Unknown target";
  const identity = credential && isGithubAppGrant(credential) && credential.login ? `@${credential.login}` : "Unknown GitHub identity";
  const target = binding?.accountLogin || repositoryOwner;
  const ref = pkg.repoRef || "default branch";
  return { credential, binding, identity, target, repository, ref, provenance: `${identity} · ${target} · ${repository} · ${ref}` };
}
