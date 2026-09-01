// The WRITE half of the Integrations section, and why it is inert (issue
// #4826 / program decision P6).
//
// The section is built against the read side in full: every slot, its
// purpose, whether it is set and where it came from. What it does not do yet
// is save, and this module is the one place that says so, so that turning it
// on is one edit rather than a hunt.
//
// WHAT IS ACTUALLY MISSING, precisely, because "not wired up" is not a
// handoff:
//
//   1. There is no write capability for integration configuration.
//      `integrationStatus` is a READ. `setGlobalSecret` cannot be called from
//      a browser at all -- it takes `encryptedValue` and `fingerprint`, both
//      produced by the backend secret helper -- so there is no path from here
//      to a secret, by construction and on purpose.
//   2. `setGlobalVariable` COULD carry the non-secret half, and deliberately
//      is not used. It takes a row `id` this window would have to derive, and
//      the tree already derives it two ways (`var-global-<slug>` in
//      scripts/secrets and component/memql/default_injector.go, `var-<slug>`
//      in component/memql/provider_config_write.go). A third derivation, in
//      TypeScript, is a third opinion -- and when it disagrees the write
//      lands in a row the resolver never reads, which is a save that reports
//      success and changes nothing.
//   3. It also carries no role gate, while this section is gated
//      owner-or-developer. Making it the OS's configuration write would put
//      cluster-wide mail settings behind a check that is presentation only.
//   4. Even a correct write does not take effect on a node that has already
//      sent mail: `integrations/email/lazy.go` resolves its sender once,
//      behind a `sync.Once`, for the life of the process. A save that flipped
//      the card to "configured" while every send still went to the log is the
//      exact failure `integrations/email/status.go` was written to prevent.
//
// So: no button that silently does nothing, and no invented mutation. The
// form renders, the save is refused in surface with the reason, and each
// credential shows the operator command that DOES work today.

export interface IntegrationWriteSurface {
  /** Whether a save can be offered at all. */
  available: boolean;
  /** What is true now, in the operator's terms. Empty when available. */
  reason: string;
}

export const INTEGRATION_WRITES: IntegrationWriteSurface = {
  available: false,
  reason:
    "The cluster publishes this configuration and has nothing that writes it back: no capability seals a secret under an integration's own key name, and no configuration write is gated to the roles this section is. Set these values in the node's environment, or with the command shown on each credential, until one exists.",
};
