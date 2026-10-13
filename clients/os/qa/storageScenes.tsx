import { Result } from "@znasllc-io/memql-sdk-core/client";
import { StorageHealthFeed, StoragePanel, StorageWarning } from "../src/cluster/storage/StorageHealth";
import { Head } from "../src/kit";

export function StorageScene({ state }: { state: "warning" | "healthy" | "missing" | "stale" | "loading" }) {
  const query = { clusterStorageStatus: async () => {
    if (state === "loading") return new Promise<Result>(() => {});
    if (state === "missing") throw new Error("The database storage report could not be read.");
    return new Result({ data: [{ available: true, observedAt: Date.now() / 1000 - (state === "stale" ? 3600 : 60), dataBytes: (state === "healthy" ? 18 : 28) * 1024 ** 3, dataCapacityBytes: 32 * 1024 ** 3, walBytes: 1 * 1024 ** 3, walCapacityBytes: 8 * 1024 ** 3, issues: state === "healthy" ? [] : ["data-volume-over-80-percent"] }] } as never);
  } };
  return <StorageHealthFeed query={query} connected><div className="os-cluster"><Head title="Readiness" /><StoragePanel /></div><StorageWarning /></StorageHealthFeed>;
}
