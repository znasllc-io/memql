import { deliverRoleTest, receiveRoleTest, isRoleTestWindow, type RoleTestCredential } from "@znasllc-io/memql-sdk-core/identityadmin";
import type { OsAuthSource } from "./source";

let credential: RoleTestCredential | undefined;
let incoming: Promise<RoleTestCredential> | undefined;
export const isRoleTestTab = isRoleTestWindow;
export const currentRoleTest = () => credential;
export function loadRoleTest(): Promise<RoleTestCredential> {
  return incoming ??= receiveRoleTest(location.origin).then(value => credential = value);
}
export const roleTestSource: OsAuthSource = {
  async bearer() {
    if (!credential || credential.expiresAt * 1000 <= Date.now()) throw new Error("The role-test session has ended.");
    return credential.token;
  },
  async refresh() { window.dispatchEvent(new Event("memql-role-test-ended")); throw new Error("Role tests cannot refresh. Start another test from Users."); },
};
let endSession: (() => Promise<void>) | undefined;
export function registerRoleTestEnd(end: () => Promise<void>) { endSession = end; return () => { if (endSession === end) endSession = undefined; }; }
export async function endRoleTestTab() { if (endSession) await endSession(); credential = undefined; }
export function handoffRoleTestEditor(tab: Window, url: string): boolean {
  if (!isRoleTestTab()) return false;
  if (!credential) throw new Error("The role-test session has ended.");
  const target = new URL(url);
  if (target.origin !== `https://vscode.${credential.domain}`) throw new Error("Choose the browser editor for role testing.");
  deliverRoleTest(tab,url,credential);
  return true;
}
