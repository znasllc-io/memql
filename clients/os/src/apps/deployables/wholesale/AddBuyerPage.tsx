import { useState } from "react";

import { Caption, Field, Head, Input, Notice } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { ReviewsPane } from "../reviews/ReviewsPane";
import type { WholesaleWrites } from "./useWholesale";

// A PERSON OF THE STORE ADDS A BUYER BY HAND (wholesaleAddBuyer). The buyer
// skips nothing: the engine writes one application on their behalf, which
// then goes the way an applicant's does -- the terms first, then somebody's
// approval -- so the page says that, once, beside the three fields.
//
// Nothing is written until the bar's act is pressed, and the act stands only
// once all three are filled. A refusal -- an address that already has an
// application on the store, say -- is the engine's sentence above the bar,
// and what was typed stays.

export function AddBuyerPage({ writes, trail, back, onAdded }: {
  writes: WholesaleWrites;
  /** The trail down to the Buyers tab: the page is added after it. */
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
  /** The new application, by the id the engine gave it. */
  onAdded: (applicationId: string, companyName: string) => void;
}) {
  const [companyName, setCompanyName] = useState("");
  const [applicantName, setApplicantName] = useState("");
  const [applicantEmail, setApplicantEmail] = useState("");
  const adding = writes.busy === "add";
  const filled = companyName.trim() !== "" && applicantName.trim() !== "" && applicantEmail.trim() !== "";

  async function add() {
    const { outcome, applicationId } = await writes.addBuyer({ companyName, applicantName, applicantEmail });
    if (outcome === "done" && applicationId !== undefined) onAdded(applicationId, companyName.trim());
  }

  const acts: Act[] = [
    { label: "Cancel", text: true, onAct: back.onSelect },
    ...(filled ? [{ label: "Add and send the terms", tone: "primary" as const, busy: adding, onAct: () => void add() }] : []),
  ];

  return (
    <ReviewsPane
      heading={<Head navigationKey="wholesale-add-buyer" title="Add buyer" breadcrumbs={[...trail, { label: "Add buyer" }]} back={back} />}
      bar={<ActionBar state="" tone={adding ? "busy" : "none"} acts={acts} />}
      context={{ page: "Wholesale add buyer" }}>
      <div className="os-review-step">
        <Field label="Company">
          <Input id="wholesale-buyer-company" label="Company" value={companyName} onChange={setCompanyName} />
        </Field>
        <Field label="Name">
          <Input id="wholesale-buyer-name" label="Name" value={applicantName} onChange={setApplicantName} />
        </Field>
        <Field label="Email">
          <Input id="wholesale-buyer-email" label="Email" value={applicantEmail} onChange={setApplicantEmail} />
        </Field>
        <Caption>They will be emailed the terms. They can buy at trade prices once they accept and someone approves.</Caption>
      </div>
      {/* THE ENGINE'S REFUSAL, standing on the bar it answers. */}
      {writes.refusal ? <Notice tone="error" sentence={writes.refusal} /> : null}
    </ReviewsPane>
  );
}
