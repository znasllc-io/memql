import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { CostEstimate, estimateText } from "../src/kit/CostEstimate";

afterEach(cleanup);

it("exposes the retained context bracket and distinguishes unknown cache counts from reported zero", () => {
  render(<CostEstimate estimate={{ status: "range", currency: "USD", minimum: .04, maximum: 3.015,
    quote: { longContextAbove: 272000, longInputMultiplier: 2, longOutputMultiplier: 1.5 },
    usage: { inputTokens: 300000, outputTokens: 1000, cacheReadTokens: null, cacheWriteShortTokens: 0, known: true, aggregate: true },
  }} />);
  expect(screen.getByText(/above 272,000 input tokens/).textContent).toContain("output rates multiply by 1.5");
  const value = (label: string) => screen.getByText(label).nextElementSibling?.textContent;
  expect(value("Cache read tokens")).toBe("Unavailable");
  expect(value("Short cache write tokens")).toBe("0");
  expect(value("Total input tokens (including cache)")).toBe("300,000");
});

it("keeps historical tariff identity, dates and applicable bands next to the energy estimate", () => {
  render(<CostEstimate label="Electricity estimate" estimate={{ status: "range", currency: "USD", minimum: .0000001, maximum: .0000003,
    tariff: { provider: "Fixture utility", name: "Residential TOU", timezone: "America/Phoenix", effectiveFrom: "2026-01-01", effectiveUntil: "2027-01-01",
      rateBands: [{ name: "Off-peak", perKWh: .1, appliesWhen: "All hours except summer weekday peaks" }] },
  }} />);
  expect(screen.getByText("Residential TOU")).toBeTruthy();
  expect(screen.getByText("America/Phoenix")).toBeTruthy();
  expect(screen.getByText("2027-01-01")).toBeTruthy();
  expect(screen.getByText(/Off-peak: 0.1 USD\/kWh/).textContent).toContain("summer weekday peaks");
  expect(estimateText({ status: "estimated", currency: "USD", amount: .0000001 })).toBe("USD <0.000001");
});
