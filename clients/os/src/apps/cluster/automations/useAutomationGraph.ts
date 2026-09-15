import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";
import type { Automation } from "./layout";
export interface LoopStop { id?: string; runId: string; automationName: string; finishedAt?: string; reason?: string; depth?: number; cap?: number; chain: { automation: string; runId: string }[] }
export function useAutomationGraph() {
  const connection = useOsConnection();
  const graph = useReading<Automation[]>("cluster:automations", connection === null ? null : async signal => (await connection.query.automationGraph({}, {signal})).rows() as unknown as Automation[]);
  // Failure here is absence of evidence, never evidence of zero stops.
  const stops = useReading<LoopStop[]>("cluster:automation-stops", connection === null ? null : async signal => (await connection.query.automationLoopStops({}, {signal})).rows() as unknown as LoopStop[]);
  return {graph, stops, reread: () => {graph.reread(); stops.reread();}};
}
