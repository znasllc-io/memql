import { Palette } from "lucide-react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";

import { AutoRefresh, Button, Notice, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../../../kit";
import type { BrandKitRecord } from "../../rows";
import type { Reading } from "../../useEmail";

// BRAND: the look and the standing parts every email the organization makes
// takes (Task 24). A subject of Settings, after Senders, for the organization
// the page is on: ONE row for its kit, saying only whether it is set up. The
// row holds no act; it opens the Brand page, which takes the pane as a
// sender's page does, and the window's trail leads back.
//
// The kit is read by the page that holds this (SettingsSection), because the
// Brand page reads the same one: a save there is what this row then says.
// Nothing broadcasts a kit, so the read is kept current at the kit's quiet
// pace while the row is on screen.

export function BrandPanel({ brand, onOpen }: {
  brand: Reading<BrandKitRecord | null>;
  onOpen: () => void;
}) {
  const known = brand.readAt !== "";
  return (
    <section className="os-app-stack" aria-label="Brand">
      <Subhead>Brand</Subhead>
      <AutoRefresh onRefresh={brand.reload} busy={brand.state === "loading"} />
      {known ? (
        <RecordList label="Brand">
          <RecordRow
            icon={<Palette size={16} aria-hidden />}
            name="Brand"
            state={brand.value === null ? "Not set up" : "Set up"}
            tone={brand.value === null ? "warn" : "accent"}
            current={brand.value !== null}
            onOpen={onOpen}
          />
        </RecordList>
      ) : brand.state === "error" ? null : (
        <RecordListSkeleton rows={1} label="Loading the brand" />
      )}
      {/* A brand that could not be read is not one that is not set up. */}
      {brand.state === "error" && !known ? (
        <Notice tone="error" sentence="This cluster did not return the brand." detail={refusalSentence(brand.error, "brandKitForAccount")}>
          <Button onClick={() => void brand.reload()}>Try again</Button>
        </Notice>
      ) : null}
    </section>
  );
}
