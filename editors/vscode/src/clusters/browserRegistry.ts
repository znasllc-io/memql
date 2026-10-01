// Browser profiles have no ~/.memql. Names live in profile state; credentials
// live only in SecretStorage. Productivity Tools never creates a second registry.
import type { ClusterConfig } from "./model.js";
import { accessTokenSecretKey, type SecretStore, type ClusterWriter } from "../auth/store.js";
import { composeEndpointFromDomain } from "../connection/endpoint.js";
import { validateEditorDomain } from "../connection/api.js";

export interface ProfileState {
  get<T>(key: string, fallback: T): T;
  update(key: string, value: unknown): PromiseLike<void>;
}
const registryKey = "memql.web.clusters.v1";
const accessKey = accessTokenSecretKey;
export class BrowserRegistry {
  private pending: Promise<void> = Promise.resolve();
  private change(action: () => Promise<void>): Promise<void> {
    const result = this.pending.then(action);
    this.pending = result.catch(() => {});
    return result;
  }
  constructor(private readonly state: ProfileState, private readonly secrets: SecretStore) {}
  domains(): string[] { return this.state.get<string[]>(registryKey, []); }
  selected(): string { return this.state.get("memql.web.selected", ""); }
  async select(domain: string): Promise<void> {
    domain = validateEditorDomain(domain);
    return this.change(async () => {
    if (!this.domains().includes(domain)) await this.state.update(registryKey, [...this.domains(), domain]);
    await this.state.update("memql.web.selected", domain);
    });
  }
  async read(domain: string): Promise<ClusterConfig> {
    domain = validateEditorDomain(domain);
    return { name: domain, domain, endpoint: composeEndpointFromDomain(domain), issuer: `https://identity.${domain}`,
      token: await this.secrets.get(accessKey(domain)) };
  }
  readonly write: ClusterWriter = async update => {
    const domain = validateEditorDomain(update.name);
    if (update.refreshToken) throw new Error("VS Code could not store your sign-in securely. Check browser storage permissions and sign in again.");
    if (update.token) throw new Error("Credentials must be stored through MemQL's credential store.");
    // Empty token keys clear legacy disk fields on desktop. This profile has
    // no disk registry; auth/store already owns credential creation/deletion.

  };
  async remove(domain: string): Promise<void> {
    return this.change(async () => {
    await this.secrets.delete(accessKey(domain));
    await this.state.update(registryKey, this.domains().filter(d => d !== domain));
    if (this.selected() === domain) await this.state.update("memql.web.selected", "");
    });
  }
}
