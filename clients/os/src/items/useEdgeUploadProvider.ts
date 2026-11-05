import { useMemo, useRef } from "react";

import { useAuthSource } from "../auth/context";
import { useOsConnection } from "../live/connection";
import { EdgeUploadProvider, libraryArtifactResolver } from "./edgeUpload";
import type { UploadProvider } from "./upload";

/** The shell's one upload provider, able to resolve a Library entry that was
 *  not visible yet when the server answered (read through a ref so a
 *  reconnect never rebuilds the provider mid-upload). */
export function useEdgeUploadProvider(injected?: UploadProvider): UploadProvider {
  const authSource = useAuthSource();
  const connection = useOsConnection();
  const live = useRef(connection);
  live.current = connection;
  return useMemo(
    () =>
      injected ??
      new EdgeUploadProvider(() => authSource.bearer(), undefined, undefined, {
        resolveArtifact: libraryArtifactResolver(() => live.current),
      }),
    [injected, authSource],
  );
}
