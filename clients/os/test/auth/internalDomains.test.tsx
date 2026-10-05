import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { OwnershipWizard } from "../../src/auth/OwnershipWizard";

afterEach(cleanup);

function installation(prefill = "") {
  const submit = vi.fn();
  render(<OwnershipWizard data={{ Local: true, PrefillOrgName: "Example", PrefillOwnerFirstName: "Ada",
    PrefillOwnerLastName: "Owner", PrefillOwnerEmail: "ada@example.test", PrefillDomain: "memql.localhost",
    PrefillInternalDomains: prefill }} busy={false} error="" submit={submit} />);
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  if (!prefill) fireEvent.click(screen.getByText("Internal team defaults"));
  return submit;
}

const entry = () => screen.getByRole("textbox", { name: "Internal email domains (optional)" });
const type = (value: string) => fireEvent.change(entry(), { target: { value } });
const next = () => fireEvent.click(screen.getByRole("button", { name: "Continue" }));
const back = () => fireEvent.click(screen.getByRole("button", { name: "Back" }));

it("adds by Enter or plus, deduplicates, removes and submits the existing wire format", () => {
  const submit = installation(" Example.test, EXAMPLE.test, old.test ");
  const list = () => screen.getByRole("list", { name: "Internal email domains (optional)" });
  expect(within(list()).getAllByRole("listitem")).toHaveLength(2);
  type("  TEAM.example.test  ");
  fireEvent.keyDown(entry(), { key: "Enter" });
  expect(entry()).toHaveProperty("value", "");
  expect(document.activeElement).toBe(entry());
  type("EXAMPLE.TEST");
  fireEvent.click(screen.getByRole("button", { name: "Add internal domain" }));
  expect(within(list()).getAllByRole("listitem")).toHaveLength(3);
  fireEvent.click(screen.getByRole("button", { name: "Remove old.test" }));
  expect(document.activeElement).toBe(entry());
  next();
  back();
  expect(within(list()).getAllByRole("listitem")).toHaveLength(2);
  next();
  fireEvent.click(screen.getByRole("button", { name: "Continue to passkey" }));
  expect(submit).toHaveBeenCalledWith("/setup", expect.objectContaining({ internal_domains: "example.test, team.example.test" }));
});

it("preserves an unadded draft across Back and saves it when continuing", () => {
  installation();
  type(" team.example.test ");
  back(); next();
  expect(entry()).toHaveProperty("value", " team.example.test ");
  next(); back();
  expect(entry()).toHaveProperty("value", "");
  expect(screen.getByRole("button", { name: "Remove team.example.test" })).toBeTruthy();
});

it.each(["person@example.test", "https://example.test", "example.test/path", "*.example.test",
  "example.test,team.test", "bad..test", "-bad.test", "bad-.test", `${"a".repeat(64)}.test`])(
  "keeps invalid input %s out of the list and prevents Continue until corrected", invalid => {
    const submit = installation();
    type(invalid);
    fireEvent.click(screen.getByRole("button", { name: "Add internal domain" }));
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    expect(entry().getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByRole("alert").textContent).toContain("example.com");
    expect(screen.queryByRole("list", { name: "Internal email domains (optional)" })).toBeNull();
    expect(submit).not.toHaveBeenCalled();
    type("correct.test");
    fireEvent.click(screen.getByRole("button", { name: "Add internal domain" }));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("button", { name: "Remove correct.test" })).toBeTruthy();
  });

it("allows an empty optional list after removing its last domain", () => {
  const submit = installation("example.test");
  fireEvent.click(screen.getByRole("button", { name: "Remove example.test" }));
  expect(screen.queryByRole("list", { name: "Internal email domains (optional)" })).toBeNull();
  fireEvent.keyDown(entry(), { key: "Enter" });
  expect(screen.queryByRole("alert")).toBeNull();
  next();
  fireEvent.click(screen.getByRole("button", { name: "Continue to passkey" }));
  expect(submit).toHaveBeenCalledWith("/setup", expect.objectContaining({ internal_domains: "" }));
});
