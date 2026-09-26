import { afterEach, describe, expect, it } from "vitest";
import { captureShopifyInstallation, takeShopifyInstallation, rememberShopifyDestination, takeShopifyDestination, authorizeShopifyInstallation } from "../../src/modules/connections/shopifyInstallation";
import { builtinReply, fakeConnection } from "../deployables/harness";
import { vi } from "vitest";
afterEach(() => { sessionStorage.clear(); history.replaceState(null,"","/"); });
describe("Shopify-led installation", () => {
 it("preserves every signed field across sign-in, removes it from the address, and consumes it once", () => {
  const signed = "shop=client.myshopify.com&hmac=signature&timestamp=123&host=abc&locale=en";
  history.replaceState(null,"","/?"+signed);
  captureShopifyInstallation(window);
  expect(location.search).toBe("");
  expect(takeShopifyInstallation()).toBe(signed);
  expect(takeShopifyInstallation()).toBeNull();
 });
 it("does not intercept OAuth callbacks", () => {
  history.replaceState(null,"","/?shop=x&hmac=y&timestamp=123&code=authorization&state=state");
  captureShopifyInstallation(window);
  expect(takeShopifyInstallation()).toBeNull();
  expect(location.search).toContain("code=authorization");
 });
 it("returns to the initiating settings surface, defaulting to global settings for direct installations", () => {
  rememberShopifyDestination("deployables");
  expect(takeShopifyDestination()).toBe("deployables");
  expect(takeShopifyDestination()).toBe("settings");
 });
 it("sends the signed launch to the server instead of trusting the selected shop", async () => {
  const connection = fakeConnection();
  vi.spyOn(connection.query,"executeNamed").mockResolvedValue(builtinReply("shopifyAccountConnectBegin",[{reason:"ok",authorizeUrl:"https://client.myshopify.com/admin/oauth/authorize?state=nonce"}]));
  const signed = "shop=client.myshopify.com&hmac=signature&timestamp=123";
  expect(await authorizeShopifyInstallation(connection.query,signed,"deployables")).toContain("client.myshopify.com");
  expect(connection.query.executeNamed).toHaveBeenCalledWith("shopifyAccountConnectBegin", expect.stringContaining("signedQuery:"));
 });
 it("refuses a forged authorization destination", async () => {
  const connection = fakeConnection();
  vi.spyOn(connection.query,"executeNamed").mockResolvedValue(builtinReply("shopifyAccountConnectBegin",[{reason:"ok",authorizeUrl:"https://attacker.test/admin/oauth/authorize"}]));
  await expect(authorizeShopifyInstallation(connection.query,"shop=client.myshopify.com","settings")).rejects.toThrow("invalid connection address");
 });
});
