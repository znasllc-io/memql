import { Button, Caption, moduleActFor, useAppReach } from "../../kit";
import type { ModuleId } from "../../system/modules";
import type { Verdict } from "../../system/readinessFold";

// A module stop's body: the one act that resolves it, and nothing else.
//
// THE NAME AND THE STATE ARE ON THE RAIL LINE ABOVE, so this draws neither
// (interface rule 7). The Set up group renders the same decision as a cell in
// a three-column row because a Settings group has no rail to carry them; here
// the rail already says "Storage -- Not set up", and repeating it inside the
// disclosure would be the same sentence three times on one card.
//
// The DECISION is `moduleActFor`, shared with that group. What differs is the
// drawing.

export function ModuleStop({
  id,
  verdict,
}: {
  id: ModuleId;
  verdict: Verdict | null;
}) {
  const reach = useAppReach("settings");
  const act = moduleActFor({
    id,
    verdict,
    sections: reach.sections,
    canOpenWindows: reach.canOpenWindows,
  });

  if (act.kind === "none") return null;

  return (
    <div className="os-setup-stop">
      {act.kind === "deployment" ? (
        <>
          <Caption>Set in the deployment, not from here.</Caption>
          {act.variables.length > 0 ? <p className="os-setup-vars">{act.variables.join(" ")}</p> : null}
        </>
      ) : act.kind === "open" ? (
        <div className="os-setup-stop-act">
          <Button tone="primary" onClick={() => reach.open(act.section, actIntent(id))}>
            Open {act.name}
          </Button>
        </div>
      ) : (
        <Caption>Settings, under {act.name}.</Caption>
      )}
    </div>
  );
}

/** What the destination section should open on, when it can act on a hint. */
function actIntent(id: ModuleId): Record<string, unknown> | undefined {
  return id === "email" ? { integration: "email" } : undefined;
}
