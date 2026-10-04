// Desktop transport adapter. The connection lifecycle is shared with the web host.
import { Connection } from "@znasllc-io/memql-sdk-core/client";
import { WebSocket as NodeWebSocket } from "ws";
import { ConnectionManager as SharedConnectionManager, type DialFn } from "./managerCore.js";
export * from "./managerCore.js";
const desktopDial: DialFn = (opts) => Connection.dial({
  ...opts,
  webSocketFactory: (url, protocols) => new NodeWebSocket(url, protocols) as unknown as WebSocket,
});
export class ConnectionManager extends SharedConnectionManager {
  constructor(...[dial, credentials, store, contextKeys, reconnect]: ConstructorParameters<typeof SharedConnectionManager>) {
    super(dial ?? desktopDial, credentials, store, contextKeys, reconnect);
  }
}
