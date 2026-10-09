// Desktop transport adapter. The connection lifecycle is shared with the web host.
import { Connection } from "@znasllc-io/memql-sdk-core/client";
import { WebSocket as NodeWebSocket } from "ws";
import { ConnectionManager as SharedConnectionManager, type DialFn } from "./managerCore.js";
export * from "./managerCore.js";
export function desktopWebSocketFactory(ca?: string): NonNullable<Parameters<typeof Connection.dial>[0]["webSocketFactory"]> {
  return (url, protocols) => new NodeWebSocket(url, protocols, { ca }) as unknown as WebSocket;
}
export function desktopDialWithTrust(certificateFor: (url: string) => Promise<string | undefined>): DialFn {
  return async (opts) => {
    const ca = await certificateFor(opts.endpoint);
    return Connection.dial({
      ...opts,
      webSocketFactory: desktopWebSocketFactory(ca),
    });
  };
}
const desktopDial = desktopDialWithTrust(async () => undefined);
export class ConnectionManager extends SharedConnectionManager {
  constructor(...[dial, credentials, store, contextKeys, reconnect]: ConstructorParameters<typeof SharedConnectionManager>) {
    super(dial ?? desktopDial, credentials, store, contextKeys, reconnect);
  }
}
