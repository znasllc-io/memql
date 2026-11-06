import { Caption, ContentSkeleton, Notice } from "../../kit";
import { DnsRecord } from "../../kit/DnsRecord";
import { PageHelp } from "../../kit/pageHelp";
import type { AzureEmailReply } from "./azureEmailSetup";

const purposes: Record<string, string> = {
  Domain: "Domain ownership", SPF: "Sender authorization (SPF)",
  DKIM: "Email signature (DKIM 1)", DKIM2: "Email signature (DKIM 2)",
};
const statuses: Record<string, string> = {
  NotStarted: "Not verified", VerificationRequested: "Checking", VerificationInProgress: "Checking",
  Verified: "Verified", VerificationFailed: "Check failed", CancellationRequested: "Stopping check",
};

/** The same copyable record strips used when binding a Deployable domain. */
export const STILL_CHECKING_MS = 5 * 60_000;

export function AzureEmailDomainRecords({ domain, records, lastChecked, stillChecking = false }: {
  domain: string; records: AzureEmailReply["records"];
  /** When Azure last answered about these records (epoch ms), if it has. */
  lastChecked?: number | null;
  /** The records have said "Checking" for a long while: say what usually holds them up. */
  stillChecking?: boolean;
}) {
  return <>
    <Caption>Add these records at the DNS provider for {domain}. Select a name or value to copy it.</Caption>
    {!records ? <ContentSkeleton kind="detail" label="Reading domain verification records"/> : records.length === 0 ?
      <Notice sentence="Azure has not returned the DNS records yet."/> : records.map(record =>
      <DnsRecord key={record.purpose} record={{ kind: record.type, name: record.name, value: record.value, purpose: purposes[record.purpose] || record.purpose }} status={statuses[record.status] || "Not verified"}/>,
    )}
    <Caption>Merge SPF into the domain's one SPF record and end it with -all, because Azure does not verify ~all. Do not create a second SPF record. Keep existing mail and website records.</Caption>
    {lastChecked ? <Caption>Last checked {new Date(lastChecked).toLocaleTimeString()}. This page checks again every 30 seconds while it is open.</Caption> : null}
    {stillChecking ? <Caption>Still checking: make sure there is one SPF record and it ends in -all.</Caption> : null}
    <PageHelp title="Entering DNS names"><p>If your provider appends {domain} automatically, use @ for the name {domain}. Keep the DKIM names as shown.</p></PageHelp>
  </>;
}
