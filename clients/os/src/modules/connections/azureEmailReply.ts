// WHAT THE CLUSTER SAYS ABOUT AN ORGANIZATION'S AZURE EMAIL, and what follows
// from it. No hooks here: these read a reply, for any page or pure helper.

export interface AzureEmailScope {
  subscriptionId: string; resourceGroup: string; dataLocation: string;
  createResourceGroup?: boolean; resourceGroupLocation?: string;
}
export interface AzureEmailPlan extends AzureEmailScope {
  domain: string; emailService: string; communicationService: string;
  /** Azure's own domain for the installation, whose one sender Azure made. */
  managedDomain?: boolean;
}
export interface AzureEmailReply {
  requestedSender?: { username: string; displayName: string; replyTo?: string };
  /** Every sender Azure has registered on the domain: address -> the display name Azure holds. */
  senders?: Record<string, string>; tenantId?: string;
  /** Where replies to each sender's mail go: address -> reply address. Absent when not known. */
  replies?: Record<string, string>;
  /**
   * What the organization has recorded at each address, registered or not: the
   * sender's display name and reply address. A domain's Add a sender starts from
   * it, because adding an address recorded here updates that sender.
   */
  recorded?: Record<string, { fromName: string; replyTo: string }>;
  status: string; applicationReady?: boolean; capture?: boolean;
  sessionId?: string; userCode?: string; verificationUri?: string; interval?: number;
  subscriptionId?: string; resourceGroup?: string; dataLocation?: string;
  createResourceGroup?: boolean; resourceGroupLocation?: string;
  choices?: { id: string; name: string }[];
  /** `sender` is the default: the address mail that names none goes out as. */
  plan?: AzureEmailPlan; planId?: string; sender?: string; replyTo?: string;
  records?: { purpose: string; name: string; type: string; value: string; status: string }[];
  deliveryFeedback?: "not-configured" | "pending" | "ready";
  operations?: { operationId: string; status: string; detail: string; submittedAt: string; checkedAt?: string; deliveryStatus?: string; deliveryAt?: string }[];
}
export const AZURE_EMAIL_LOCATIONS = ["Africa", "Asia Pacific", "Australia", "Brazil", "Canada", "Europe", "France", "Germany", "India", "Japan", "Korea", "Norway", "Switzerland", "United Arab Emirates", "United Kingdom", "United States"];

/**
 * Whether senders are added on this organization's Azure domain: it is sending,
 * and it is the organization's own domain. Azure's own domain for the
 * installation has the one sender Azure made, and takes no others.
 */
export function addsSendersOnAzure(state: AzureEmailReply | null | undefined): boolean {
  return state?.status === "ready" && !!state.plan?.domain && !state.plan.managedDomain;
}

/**
 * The one sender of an organization whose mail goes out through Azure's own
 * domain, which registers no other; "" when its mail does not.
 */
export function azureOwnDomainSender(state: AzureEmailReply | null | undefined): string {
  return state?.status === "ready" && state.plan?.managedDomain ? state.sender ?? "" : "";
}

/**
 * The part of an address Azure registers on this domain: the username, in lower
 * case as every add sends it, when the address is on the domain; "" when it is
 * not, because Azure registers a domain's senders on that domain alone.
 */
export function usernameOnDomain(address: string, domain: string): string {
  const [username, on] = address.trim().split("@");
  return username && on && on.toLowerCase() === domain.trim().toLowerCase() ? username.toLowerCase() : "";
}
