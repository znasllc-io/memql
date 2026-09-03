import { useState } from "react";

import { rowString } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, ChoiceStack, FormRow, Input, Panel, Select, Subhead, useLiveView } from "../../../kit";
import { useNewPackage } from "./actions";
import { ProblemNotice } from "./ReportView";
import { useZipArtifacts } from "../actions/useZipArtifacts";

// Adding a package: point the cluster at a repository, or at a zip already in
// Files.
//
// ===========================================================================
// THE SECRET FIELD TAKES A NAME, AND SAYS SO
// ===========================================================================
// A private repository needs a token, and this form never sees one. The field
// asks for the NAME of a secret already stored in this cluster, which is the
// same pattern a storefront's token uses -- the value is read once, at the
// moment of a fetch, and never lands on a row, a snapshot or a log.
//
// That is worth stating on the surface rather than only in a doc, because a
// field labelled "token" would be filled with a token by anyone who did not
// read further, and the paste would be the mistake.
//
// ===========================================================================
// NOTHING IS DEPLOYED BY ADDING
// ===========================================================================
// Adding a package tracks it. The first deploy is a separate, deliberate act
// that shows its report first -- so this form's own copy has to avoid implying
// that finishing it puts anything on the internet.

export function NewPackage({ onDone, onCancel }: { onDone: (packageId: string) => void; onCancel: () => void }) {
  const { busy, refusal, create } = useNewPackage();
  const [sourceKind, setSourceKind] = useState<"repo" | "artifact">("repo");
  // The Library read runs ONLY while the zip branch is showing. A form that
  // seeded somebody's whole Library because it opened would be a read nobody
  // asked for, which is the contract useZipArtifacts states with its null key.
  const artifacts = useZipArtifacts(sourceKind === "artifact");
  const zips = useLiveView(artifacts.feed.source, "new-package:zips", (rows) =>
    rows.map((r) => ({ id: rowString(r, "id"), name: rowString(r, "name") })).filter((z) => z.id !== ""),
  );
  const zipRows = zips?.snapshot.rows ?? [];

  const [name, setName] = useState("");
  const [repoUrl, setRepoUrl] = useState("");
  const [repoRef, setRepoRef] = useState("");
  const [credentialId, setCredentialId] = useState("");
  const [artifactId, setArtifactId] = useState("");

  const ready =
    name.trim() !== "" &&
    (sourceKind === "repo" ? repoUrl.trim() !== "" : artifactId !== "");

  return (
    <Panel label="Add a package">
      <div className="os-head">
        <Subhead>Add a package</Subhead>
        <div className="os-head-actions">
          <Button tone="quiet" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            tone="primary"
            disabled={!ready}
            busy={busy}
            busyLabel="Adding"
            onClick={() => {
              void create({
                name: name.trim(),
                sourceKind,
                repoUrl: sourceKind === "repo" ? repoUrl.trim() : "",
                repoRef: sourceKind === "repo" ? repoRef.trim() : "",
                credentialId: sourceKind === "repo" ? credentialId.trim() : "",
                artifactId: sourceKind === "artifact" ? artifactId : "",
              }).then((id) => {
                if (id !== "") onDone(id);
              });
            }}
          >
            Add
          </Button>
        </div>
      </div>

      <Caption>Adding a package tracks it. Nothing is deployed until you say so, and you see what it would do first.</Caption>

      <ChoiceStack
        name="os-package-source"
        label="Where the source lives"
        value={sourceKind}
        onChange={(next) => setSourceKind(next === "artifact" ? "artifact" : "repo")}
        options={[
          {
            value: "repo",
            label: "A GitHub repository",
            description:
              "The repository stays the source of truth. This cluster notices when something newer lands there.",
          },
          {
            value: "artifact",
            label: "A zip from Files",
            description: "A complete snapshot with nothing upstream. It deploys in exactly the same way.",
          },
        ]}
      />

      <FormRow>
        <label className="os-form-label" htmlFor="os-pkg-name">
          Name
        </label>
        <Input
          id="os-pkg-name"
          label="Package name"
          value={name}
          onChange={setName}
          placeholder="acme-storefront"
        />
      </FormRow>
      <Caption>What to call it here. The package's own manifest replaces this the first time it is read.</Caption>

      {sourceKind === "repo" ? (
        <>
          <FormRow>
            <label className="os-form-label" htmlFor="os-pkg-repo">
              Repository
            </label>
            <Input
              id="os-pkg-repo"
              label="Repository URL"
              value={repoUrl}
              onChange={setRepoUrl}
              placeholder="https://github.com/acme/storefront"
            />
          </FormRow>

          <FormRow>
            <label className="os-form-label" htmlFor="os-pkg-ref">
              Branch or tag
            </label>
            <Input id="os-pkg-ref" label="Branch or tag" value={repoRef} onChange={setRepoRef} placeholder="main" />
          </FormRow>
          <Caption>Leave it empty to follow the repository's default branch.</Caption>

          <FormRow>
            <label className="os-form-label" htmlFor="os-pkg-secret">
              Secret name
            </label>
            <Input
              id="os-pkg-secret"
              label="Name of the stored secret holding the access token"
              value={credentialId}
              onChange={setCredentialId}
              placeholder="acme-repo-token"
            />
          </FormRow>
          <Caption>
            For a private repository: the NAME of a secret already stored in this cluster, not the token itself. The
            value is read only when the source is fetched, and is never written down. Leave it empty for a public
            repository.
          </Caption>
        </>
      ) : (
        <>
          <FormRow>
            <label className="os-form-label" htmlFor="os-pkg-zip">
              Zip in Files
            </label>
            <Select id="os-pkg-zip" label="Zip in Files" value={artifactId} onChange={setArtifactId}>
              <option value="">Choose a zip</option>
              {zipRows.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </Select>
          </FormRow>
          <Caption>
            {zipRows.length === 0
              ? "No zips in Files yet. Upload one there and it will appear here."
              : "The zip is a tree with memql-package.yaml at its root -- the same tree a repository holds."}
          </Caption>
        </>
      )}

      {refusal ? <ProblemNotice problem={{ ...refusal, fatal: true }} tone="error" /> : null}
    </Panel>
  );
}
