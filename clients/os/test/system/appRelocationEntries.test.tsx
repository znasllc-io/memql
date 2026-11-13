import { act, renderHook } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import type { ReactNode } from "react";
import { OS_REGISTRY } from "../../src/apps/registry";
import { OsProvider, useOs } from "../../src/chrome/state";
import { clearEffectiveCapabilities, setEffectiveCapabilities } from "../../src/system/roles";

const allow = (...resources: string[]) => setEffectiveCapabilities(resources.map(resource => ({ verb: "read", resource, effect: "allow", source: "user" })));
const wrapper = ({ children }: { children: ReactNode }) => <OsProvider registry={OS_REGISTRY} actorRole="custom" grid={{ cols: 8, rows: 5 }}>{children}</OsProvider>;
afterEach(clearEffectiveCapabilities);

it("the shell resolves a legacy personal link once and refuses a different personal door", () => {
  allow("app:identity");
  const { result } = renderHook(useOs, { wrapper });
  act(() => { result.current.actions.openApp("identity", "tokens"); });
  const windows = Object.values(result.current.state.shell.windows);
  expect(windows).toHaveLength(1);
  expect(windows[0]).toMatchObject({ appId: "profile", sectionId: "tokens" });
  act(() => { result.current.actions.openApp("settings", "appearance"); });
  expect(Object.values(result.current.state.shell.windows)).toEqual(windows);
  act(() => { result.current.actions.openApp("profile", "tokens"); });
  expect(Object.values(result.current.state.shell.windows)).toHaveLength(1);
});

it("an old administrative link cannot open other destination sections", () => {
  allow("app:settings", "app:settings/language");
  const { result } = renderHook(useOs, { wrapper });
  act(() => { result.current.actions.openApp("settings", "language"); });
  const windows = Object.values(result.current.state.shell.windows);
  expect(windows).toHaveLength(1);
  expect(windows[0]).toMatchObject({ appId: "concepts", sectionId: "language" });
  act(() => { result.current.actions.openApp("concepts", "registry"); });
  expect(Object.values(result.current.state.shell.windows)).toEqual(windows);
  act(() => { result.current.actions.openApp("settings", "keys"); });
  expect(Object.values(result.current.state.shell.windows)).toEqual(windows);
});
