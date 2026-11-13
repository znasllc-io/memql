import { useSession } from "../../chrome/access";
import { AppLogsSection } from "../../logs/AppLogsSection";
import { Head, Notice } from "../../kit";
import { holds } from "../../system/roles";
import type { OsAppProps } from "../../system/registry";
import { AuditSection } from "../cluster/audit/AuditSection";
import { KeysSection } from "../settings/KeysSection";
import { PolicyPanel } from "../settings/PolicyPanel";
import { TokensSection } from "../settings/TokensSection";
import { AccessRequestsSection } from "../users/AccessRequestsSection";
import { useRoleCatalog } from "../users/useRoles";

/** Identity administers sign-in for the installation. Personal /me pages
 * belong to Profile; the normal identity service and its checks are unchanged. */
export function IdentityApp({ sectionId, intent, consumeIntent }: OsAppProps) {
  const { access } = useSession();
  if (sectionId === "keys") return <KeysSection />;
  if (sectionId === "service-tokens") return <TokensSection />;
  if (sectionId === "requests") return <AccessRequests />;
  if (sectionId === "audit") return <AuditSection />;
  if (sectionId === "logs") return <AppLogsSection app="identity" subjectConcepts={["v1:identity:auditEvent"]} intent={intent} consumeIntent={consumeIntent} />;
  return <div className="os-app-stack"><Head title="Sign-in policy" /><PolicyPanel enabled={access?.role === "owner" || access?.role === "admin"} />{access?.role !== "owner" && access?.role !== "admin" ? <Notice tone="info" sentence="Sign-in policy is managed by the cluster owner or an administrator." /> : null}</div>;
}

function AccessRequests() {
  const { access } = useSession();
  const catalog = useRoleCatalog();
  return access?.everyAccount === true
    ? <AccessRequestsSection catalog={catalog} viewerRole={access.role ?? ""} canReview={holds("create", "principal")} />
    : <Notice tone="info" sentence="Access requests are reviewed by cluster administrators." />;
}
