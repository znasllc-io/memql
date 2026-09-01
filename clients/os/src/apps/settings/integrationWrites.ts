// The WRITE half of the Integrations section (issue #4826 / memql#4825).
//
// It is live. What it rides is `integrationConfigure`, a capability built for
// this surface, and knowing WHY it is not `setGlobalVariable` is what keeps
// somebody from "simplifying" it back to one:
//
//   1. THE ROW NAME IS NOT A PARAMETER. The caller names a manifest SLOT --
//      `senderAddress`, `clientSecret`, the same name the cards key on -- and
//      the engine knows the variable behind it. A client that supplied the
//      name could write MEMQL_EMAIL_SENDR, get a green save, and never be
//      mailed anything again: the resolver looks up names it knows, and that
//      is not one.
//   2. THE ROW ID REUSES THE SEEDER'S DERIVATION, so a save UPDATES the row an
//      installed cluster already has. The id is derived three ways in this
//      tree, and because the resolver looks rows up by NAME a mismatch does
//      not fail -- it writes a SECOND row with the same name and makes "which
//      value is live" a question about query order.
//   3. A SECRET IS SEALED SERVER-SIDE, under a master key that exists on nodes
//      and must never exist in a browser. The plaintext crosses the wire once
//      and is never sent back, which is why a card can say a credential is set
//      and can never show it.
//
// And the gate is OWNER OR DEVELOPER -- stricter than the status read, which
// also admits admin. So every role this section offers itself to can also
// write, and the form needs no refusal state of its own: a refusal that does
// arrive (a node with no engine wired, a session whose role changed under it)
// renders beside the field in the engine's own words.

/**
 * The one caveat that survived, said once per card rather than per field.
 *
 * A write reaches THIS node's resolver: `integrationConfigure` discards the
 * sender this process had resolved, so the change lands on its next send with
 * no restart. It does not reach the other replicas, which each re-resolve on
 * their own next send -- the same eventual consistency the environment path
 * has always had, and worth saying out loud because a console that showed one
 * node's registry could otherwise be read as showing the fleet's.
 *
 * The per-save sentence is NOT here. `takesEffect` comes back on the reply and
 * is rendered verbatim, because only the engine knows whether this node
 * re-resolved or is running from an environment that outranks stored rows.
 */
export const CONFIG_WRITE_NOTE =
  "A save is stored in the cluster and takes effect on the node that answered, on its next send. Other replicas pick it up on their own next send rather than all at once.";
