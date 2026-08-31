// The DesktopGateway over the sdk-core Connection (epic memql#4746). It is
// deliberately the thinnest thing that can be: three SDK calls, no policy.
// Every decision about revisions, echoes, debouncing and what to do with an
// unreadable row lives in GraphDesktopStore, where it is testable against a
// fake gateway with no cluster and no browser.

import {
  Concepts,
  rowNumber,
  rowObject,
  type Connection,
} from "@znasllc-io/memql-sdk-core/client";

import type { DesktopDocument } from "../system/store";
import type { DesktopGateway, StoredDesktop } from "../system/graphStore";

export class SdkDesktopGateway implements DesktopGateway {
  constructor(private readonly connection: Connection) {}

  async read(): Promise<StoredDesktop | null> {
    const result = await this.connection.query.myDesktop({});
    // myDesktop is a per-user singleton by construction: the row id is
    // derived from the actor, so there is exactly one row or none.
    const row = result.rows()[0];
    if (!row) return null;
    const document = rowObject(row, "document");
    if (document === null) return null;
    return { revision: rowNumber(row, "revision"), document };
  }

  async write(input: { revision: number; document: DesktopDocument }): Promise<void> {
    await this.connection.query.saveMyDesktop({
      revision: input.revision,
      document: input.document as unknown as Record<string, unknown>,
    });
  }

  watch(onChange: () => void): () => void {
    const subscriptions = this.connection.subscriptions;
    // A connection without a subscription manager is a supported shape (it
    // is what a seeded test harness passes). The desktop still reads and
    // writes; it just does not follow another machine live.
    if (!subscriptions) return () => {};
    // CREATED ONLY, and it is not an omission. saveMyDesktop is an insert{},
    // and the engine publishes graph.node.created for every write it takes
    // -- the overwrites included; only the update() path adds
    // graph.node.updated. Subscribing to `updated` here would listen for an
    // event this concept cannot emit. The matching mesh routing rule
    // (component/node/routing.go) forwards exactly this one.
    return subscriptions.subscribeGraph(() => onChange(), {
      concept: Concepts.OS_DESKTOP,
      actions: ["created"],
    });
  }
}
