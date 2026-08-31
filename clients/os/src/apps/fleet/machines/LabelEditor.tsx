import { useEffect, useState } from "react";

import { MapEditor } from "../MapEditor";
import { chipsFromMap, type LabelMap } from "../labels";

// The operator-label editor for one machine.
//
// ===========================================================================
// IT EDITS operatorLabels AND NOTHING ELSE
// ===========================================================================
// There is deliberately no edit affordance anywhere for the reported `labels`
// map -- not disabled, not behind a confirm, ABSENT. The cockpit overwrites
// that map wholesale from its Register message on every reconnect, so a value
// written into it survives until the machine's next restart and then
// vanishes, leaving a routing rule that still reads correctly against a
// machine that quietly stopped matching it. A control that writes a value
// with that lifetime is a control that lies, and the honest version of it is
// not a warning: it is its absence.
//
// ===========================================================================
// OPTIMISTIC, WITH A ROLLBACK THE BOOLEAN PAYS FOR
// ===========================================================================
// A chip appears before the row comes back, because the round trip is long
// enough that waiting reads as a dropped click. That is only safe because the
// write RESOLVES to whether it happened: a void write would leave a chip on
// screen claiming a label the router will never match on -- the exact silent
// disagreement the two-map split exists to prevent.

export function LabelEditor({
  operatorLabels,
  busy,
  onSave,
}: {
  operatorLabels: LabelMap;
  busy: boolean;
  /** Resolves false when the cluster refused, which is what rolls the
   *  optimistic chip back. */
  onSave: (labels: LabelMap) => Promise<boolean>;
}) {
  const [local, setLocal] = useState<LabelMap>(operatorLabels);

  // The ROW wins. An edit landing from somewhere else (another tab, the
  // portal) has to reach this editor, and a local map that outranked the row
  // would let an operator save a set assembled from a state the cluster never
  // had. The dependency is the SERIALIZED map rather than the object, because
  // a fresh object arrives on every heartbeat fold and would otherwise reset
  // this on a change that touched nothing here.
  const serialized = JSON.stringify(chipsFromMap(operatorLabels));
  useEffect(() => {
    setLocal(operatorLabels);
    // THE DEP IS `serialized` ON PURPOSE: it is the value-identity of
    // operatorLabels, while the object itself is fresh on every heartbeat
    // fold -- depending on the object would reset the editor constantly.
  }, [serialized]);

  function change(next: LabelMap) {
    const held = local;
    setLocal(next);
    void onSave(next).then((ok) => {
      if (!ok) setLocal(held);
    });
  }

  return (
    <MapEditor
      value={local}
      onChange={change}
      busy={busy}
      label="Operator labels"
      idPrefix="fleet-operator-labels"
    />
  );
}
