import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { useMemo, useState } from "react";
import type { Concept } from "@znasllc-io/memql-sdk-core/client";

import { Head, RecordList, RecordRow, Notice, Panel, Refine, Select, Subhead } from "../../kit";
import { ConceptPage } from "./ConceptPage";
import { domainCounts, groupConcepts, originBadgeFor, originBadgeLabel } from "./registry";
import { useConceptRegistry } from "./useConceptRegistry";
import type { ConceptsSettings } from "./settings";

// The registry: every concept this cluster declares, and the way into one.
//
// ===========================================================================
// ONE VIEW AT A TIME (DESIGN.md rule 11)
// ===========================================================================
// A concept page carries a schema and a row window, which is tall. So the
// page REPLACES the list and carries a quiet back control in its own Head,
// the shape `DeployablePage` uses -- rather than being appended beneath the
// list it was selected from, which is what puts two Heads in one scroller.
//
// The list and the page are one `view` state for the same reason
// DeployablesSection holds one: two independent "is something selected"
// booleans is how a surface ends up rendering both.
type RegistryView = { kind: "list" } | { kind: "concept"; conceptId: string };

export function RegistrySection({
  settings,
  openConceptId,
}: {
  settings: ConceptsSettings;
  /** A concept named in the address bar, if this window was opened on one. */
  openConceptId?: string;
}) {
  const registry = useConceptRegistry();
  const [search, setSearch] = useState("");
  const [domain, setDomain] = useState("");
  const [view, setView] = useState<RegistryView>(() =>
    openConceptId && openConceptId.trim() !== ""
      ? { kind: "concept", conceptId: openConceptId.trim() }
      : { kind: "list" },
  );

  const domains = useMemo(() => domainCounts(registry.concepts), [registry.concepts]);
  const groups = useMemo(
    () => groupConcepts(registry.concepts, search, domain),
    [registry.concepts, search, domain],
  );

  if (view.kind === "concept") {
    const concept = registry.concepts.find((c) => c.id === view.conceptId) ?? null;
    return (
      <ConceptPage
        conceptId={view.conceptId}
        concept={concept}
        registryState={registry.state}
        settings={settings}
        onBack={() => setView({ kind: "list" })}
      />
    );
  }

  const shown = groups.reduce((n, g) => n + g.concepts.length, 0);
  const total = registry.concepts.length;

  return (
    <div className="os-app-stack os-concepts">
      <Head
        title="Concepts"
        meta={registry.state === "live" ? shown : undefined}
      >
        {/* Search and the domain facet both ride the one Refine affordance
            (rule 2). The portal stood a horizontal domain chip rail above
            the list permanently, which is exactly the furniture that rule
            exists to remove -- it was filter chrome over the content, in
            every session, whether or not anybody was asking a question. */}
        <Refine
          search={search}
          onSearch={setSearch}
          placeholder="Concept, domain or description"
          label="Search concepts"
          chips={
            domain === ""
              ? []
              : [{ id: "domain", label: domain, onRemove: () => setDomain("") }]
          }
        >
          <Select
            id="concepts-domain"
            label="Domain"
            value={domain}
            onChange={setDomain}
          >
            <option value="">Every domain</option>
            {domains.map((d) => (
              <option key={d.domain} value={d.domain}>
                {d.domain} ({d.count})
              </option>
            ))}
          </Select>
        </Refine>
      </Head>

      {registry.state === "failed" ? (
        <Notice
          tone="error"
          sentence="The concept registry is not readable on this connection."
          detail={registry.error}
        />
      ) : null}

      {registry.state === "seeding" && total === 0 ? (
        <RecordListSkeleton label="Loading the registry from the cluster" />
      ) : null}

      {registry.state !== "failed" && total > 0 && shown === 0 ? (
        <Notice
          tone="info"
          sentence="No concept matches what you asked for."
          next="Clear the search or the domain to see the whole registry."
        />
      ) : null}

      {groups.map((group) => (
        <Panel key={group.domain} label={group.domain}>
          <Subhead meta={registry.state === "live" ? group.concepts.length : undefined}>{group.domain}</Subhead>
          <RecordList as="ul" label={group.domain}>
            {group.concepts.map((concept) => (
              <ConceptLine
                key={concept.id}
                concept={concept}
                onOpen={() => setView({ kind: "concept", conceptId: concept.id })}
              />
            ))}
          </RecordList>
        </Panel>
      ))}
    </div>
  );
}

function ConceptLine({ concept, onOpen }: { concept: Concept; onOpen: () => void }) {
  const badge = originBadgeFor(concept);
  return (
    <RecordRow name={concept.entity} secondary={concept.id} onOpen={onOpen}
      state={badge.kind === "none" ? undefined : originBadgeLabel(badge)}
      tone={badge.kind === "mirror" ? "accent" : "muted"}>
      {concept.description}
    </RecordRow>
  );
}
