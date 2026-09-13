import { useEffect } from "react";

import { useOs } from "../../chrome/state";
import { accessAdmits } from "../../system/registry";
import { useSetupFacts } from "./context";
import { setupWidget } from "./manifest";

// WHETHER THE SETUP WIDGET IS ON A DESK AT ALL (design record
// 2026-09-07-core-gate-and-honest-install, D4).
//
// ===========================================================================
// DERIVED, NEVER SEEDED
// ===========================================================================
// `seedDocument` runs in a React state initializer, before the role, the
// ladder or the feed has landed, so it could not place a role-gated widget --
// and did not, on every production boot. Fixing the timing would not have been
// enough: a seed is a one-time act, and an existing desk never gains a widget
// because the store adopts the stored document and replaces the seeded
// surfaces wholesale. A migration would be a one-time act again, wrong the
// next time a stop unsettles.
//
// ===========================================================================
// THE DEPS ARE THE FEED, THE LADDER, THE ROLE AND THE ACTIVE DESK
// ===========================================================================
// Deliberately NOT the desk's contents. A person who takes the card off while
// a stop is unsettled gets it back on the next change of the feed or the
// ladder -- the widget is the state -- but not the same instant, which would
// be a card that cannot be moved out of the way even for a moment.
//
// ===========================================================================
// IT ONLY EVER ADDS
// ===========================================================================
// Retiring stays with `SetupGate`, which already does it through the exit
// hold: one beat with every mark lit, which is the only confirmation this
// surface gives that the last stop landed. Putting the retire here too would
// be two owners of one card, and the beat would be the thing that lost.
export function SetupPresence(): null {
  const { actions, state, accessEpoch } = useOs();
  const facts = useSetupFacts();
  // NOT KNOWN IS NOT UNSETTLED. Both hide the card, and only one of them is
  // an answer -- adding it while the feed is still seeding would put a wizard
  // on the desk of a cluster that turns out to be configured.
  const unsettled = facts !== null && facts.known && !facts.configured;
  const deskId = state.shell.activeDeskId;

  useEffect(() => {
    if (!unsettled) return;
    // The manifest's own gate, asked rather than restated (`requires:
    // "app:setup"`, seeded on owner and developer). Fail-closed before the
    // effective set lands: `accessAdmits` answers false until the read is in,
    // and `accessEpoch` below re-runs this the moment it is. `ensureWidget` checks it again at the point
    // of action, which is right for the reason `addWidget` gives -- an action
    // that trusts its callers is an action whose next caller does not know it
    // had to -- and asking here as well is what keeps a reader from paying
    // for a state update that would be refused.
    if (!accessAdmits(setupWidget.requires)) return;
    actions.ensureWidget(setupWidget.id, "ask");
    // `facts` RATHER THAN THE DERIVED BOOLEAN, so this runs whenever the feed
    // or the effective set CHANGES rather than only when the answer flips. That is
    // what makes "the widget is the state" true for somebody who took the card
    // off by hand: it returns on the next reading, not only when a stop
    // happens to settle. `facts` is memoized on the feed, the passkey reading
    // and the role, so a render that changed none of them re-runs nothing --
    // which is why removing it does not undo itself in the same instant.
  }, [accessEpoch, unsettled, facts, deskId, actions]);

  return null;
}
