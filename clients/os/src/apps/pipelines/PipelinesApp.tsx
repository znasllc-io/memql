import type { OsAppProps } from "../../system/registry";
import { DeployablesApp } from "../deployables/DeployablesApp";
export { PIPELINES_SECTIONS } from "./settings";

export function PipelinesApp(props: OsAppProps) {
  return <DeployablesApp {...props} surface="pipelines" />;
}
