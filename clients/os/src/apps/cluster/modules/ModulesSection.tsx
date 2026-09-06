import { useCallback, useMemo, useState } from "react";
import { ModulesClient, type ModulesInventory } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Chip, Head, Notice, Panel, Row, Subhead } from "../../../kit";
import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";
import { ModuleDetail } from "./ModuleDetail";
import {
  groupModules,
  moduleStateNeedsAttention,
  moduleStateSentence,
  moduleStateTone,
} from "./rows";

// Modules: what this cluster is MADE OF, as the answering binary knows it.
//
// ===========================================================================
// THE ATTRIBUTION IS NOT DECORATION
// ===========================================================================
// `listModules` answers from the registries and the environment of the ONE
// node that handled the request, and a `scope: "node"` row is that binary's
// own truth. The same question asked a second later can land on a sibling
// replica and answer differently -- one restarted after a key was seeded and
// one not is the ordinary case, not a fault. So the reporting node is on the
// Head, beside the title, every time: a page that showed this inventory
// without saying whose it is would present a per-node reading as a cluster
// fact, and the disagreement it hides is the single most useful thing an
// operator could learn from it.
//
// ===========================================================================
// RULE 11: THE DETAIL REPLACES THE LIST
// ===========================================================================
// Both shapes in DESIGN.md rule 11 are right and the rule says the choice is
// "how tall the detail is". A module's detail is its manifest-declared
// environment, and a real one -- identity's, the bff's -- is dozens of
// entries each carrying a description sentence, a scope, a requiredFor list
// and a value. That does not fit in the Bin's 380px column at a readable
// measure, and squeezing it there would wrap every description to four lines.
//
// The other half of the choice is DESIGN.md rule 12: the pack switch is a
// lifecycle act, it belongs on ONE bar on the window's bottom edge with the
// state in words beside it, and an ActionBar is a property of a PANE -- it
// cannot sit under a 380px column beside a list that has its own scroller.
//
// So this is the `DeployablePage` shape: the detail replaces the list and
// carries a quiet `<- Modules` in its Head. The list is grouped and scanned
// rather than browsed row by row, so losing your place costs little.

export function ModulesSection() {
  const connection = useOsConnection();

  // The client is constructed from the dispatcher, not from `query`: the
  // module registry is its own surface with its own authorization tier
  // (reads owner/admin, the write owner-only), which is why the SDK keeps it
  // out of the generated query vocabulary.
  const modules = useMemo(
    () => (connection === null ? null : new ModulesClient(connection.dispatcher)),
    [connection],
  );

  const read = useCallback(
    (signal: AbortSignal): Promise<ModulesInventory> => {
      if (modules === null) return Promise.reject(new Error("not connected"));
      return modules.listModules({ signal });
    },
    [modules],
  );

  const inventory = useReading<ModulesInventory>(
    "cluster:modules",
    modules === null ? null : read,
  );

  const [openKey, setOpenKey] = useState("");

  const groups = useMemo(
    () => groupModules(inventory.value?.modules ?? []),
    [inventory.value],
  );

  const open = useMemo(() => {
    if (openKey === "") return null;
    return (inventory.value?.modules ?? []).find((m) => `${m.kind}/${m.name}` === openKey) ?? null;
  }, [inventory.value, openKey]);

  if (open !== null && modules !== null) {
    return (
      <ModuleDetail
        client={modules}
        module={open}
        reportingNodeId={inventory.value?.reportingNodeId ?? ""}
        reportingNodeType={inventory.value?.reportingNodeType ?? ""}
        onBack={() => setOpenKey("")}
        onFlipped={() => inventory.reread()}
      />
    );
  }

  const answeredBy =
    inventory.value === null
      ? null
      : `answered by ${inventory.value.reportingNodeId || "an unnamed node"} (${
          inventory.value.reportingNodeType || "unknown type"
        })`;

  return (
    <div className="os-cluster">
      {/* The one control on the Head, and it is quiet rather than primary:
          this reading is not live, so "look again" is the honest companion to
          printing when we last looked. It also re-asks after a reconnect,
          which the reading's key deliberately does not do on its own. */}
      <Head title="Modules" meta={answeredBy}>
        <Button
          tone="quiet"
          busy={inventory.state === "reading"}
          busyLabel="Reading"
          onClick={() => inventory.reread()}
        >
          Read again
        </Button>
      </Head>

      {inventory.state === "failed" ? (
        // The server's own sentence, verbatim. A wrapper of ours ("could not
        // load modules") replaces the message naming the actual refusal with
        // one naming the surface.
        <Notice
          tone="error"
          sentence="The cluster did not answer the module inventory."
          detail={inventory.error}
        />
      ) : null}

      {inventory.state === "reading" && inventory.value === null ? (
        <Caption>Reading the inventory from the cluster.</Caption>
      ) : null}

      {groups.map((group) => (
        <Panel key={group.kind} label={group.name}>
          <Subhead>{group.name}</Subhead>
          {group.modules.map((module) => {
            const sentence = moduleStateSentence(module.state);
            return (
              <Row
                key={`${module.kind}/${module.name}`}
                name={module.name}
                onOpen={() => setOpenKey(`${module.kind}/${module.name}`)}
                state={
                  <>
                    <Chip tone={moduleStateTone(module.state)} title={sentence || undefined}>
                      {module.state || "unstated"}
                    </Chip>
                    <Chip tone="muted" title={scopeTitle(module.scope)}>
                      {module.scope || "unscoped"}
                    </Chip>
                  </>
                }
              >
                <span className="os-cluster-row-note">
                  {module.stateDetail || module.description}
                </span>
                {moduleStateNeedsAttention(module.state) ? (
                  <span className="os-cluster-row-attention">{sentence}</span>
                ) : null}
              </Row>
            );
          })}
        </Panel>
      ))}

      {inventory.state === "read" && groups.length === 0 ? (
        <Caption>This node reported no modules at all, which is not a state a running engine reaches -- read its logs.</Caption>
      ) : null}

      {inventory.at === null ? null : (
        <Caption>
          Read {inventory.at.toLocaleTimeString()}. This inventory is not live; nothing broadcasts a
          registry change.
        </Caption>
      )}
    </div>
  );
}

/** What a scope word MEANS, one hover away. `node` is the one that matters:
 *  it says this row is the answering binary's own truth rather than the
 *  cluster's. */
function scopeTitle(scope: string): string {
  if (scope === "node") {
    return "This state is the answering binary's own: its registries and its environment. A sibling replica can answer differently.";
  }
  if (scope === "cluster") {
    return "This state is the shared graph's, so every node agrees about it.";
  }
  return "";
}
