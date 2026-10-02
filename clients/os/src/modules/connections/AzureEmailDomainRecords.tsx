import { Caption, ContentSkeleton, Notice } from "../../kit";
import { DnsRecord } from "../../kit/DnsRecord";
import { InfoDetail } from "../../kit/InfoDetail";
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
export function AzureEmailDomainRecords({ domain, records }: { domain: string; records: AzureEmailReply["records"] }) {
  return <>
    <Caption>Add these records at the DNS provider for {domain}. Select a name or value to copy it.</Caption>
    {!records ? <ContentSkeleton kind="detail" label="Reading domain verification records"/> : records.length === 0 ?
      <Notice sentence="Azure has not returned the DNS records yet."/> : records.map(record =>
      <DnsRecord key={record.purpose} record={{ kind: record.type, name: record.name, value: record.value, purpose: purposes[record.purpose] || record.purpose }} status={statuses[record.status] || "Not verified"}/>,
    )}
    <Caption>Merge SPF into its existing record; do not create a second SPF record. Keep existing mail and website records.</Caption>
    <InfoDetail title="Entering DNS names"><p>If your provider appends {domain} automatically, use @ for the name {domain}. Keep the DKIM names as shown.</p></InfoDetail>
  </>;
}
