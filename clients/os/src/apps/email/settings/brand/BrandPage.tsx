import { useCallback, useEffect, useMemo, useRef, useState, type FocusEvent, type ReactNode } from "react";
import { Mail } from "lucide-react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";

import { AutoRefresh, Button, ContentSkeleton, Details, EmptyState, Head, Notice } from "../../../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../../../kit/ActionBar";
import type { UploadProvider } from "../../../../items/upload";
import { useSaveBrandKit } from "../../actions";
import type { BrandKitRecord } from "../../rows";
import { Preview, type PreviewPlace } from "../../templates/Preview";
import { useBrandPreview, type Reading } from "../../useEmail";
import { FooterGroup, ReplyGroup, SocialGroup } from "./LinkGroups";
import { ColoursGroup, FontsGroup } from "./LookGroups";
import type { BrandPictures } from "./PictureField";
import { LogoGroup, SignatureGroup } from "./PictureGroups";
import { draftFromKit, draftProblem, kitFromDraft, sameKit, type BrandDraft, type ChangeDraft } from "./kit";

// AN ORGANIZATION'S BRAND, ITS PREVIEW, AND ITS ONE ACT (Task 24).
//
// The page a Brand row opens, in the pane Settings gives way to (as a sender's
// page is): the kit's editor, grouped as Fleet's machine Settings are; a
// preview of an email in the brand as it is now, beside it on a wide window and
// under it on a narrow one; and the bar on the window's bottom edge (DESIGN.md
// rule 12) with Save, the only act that writes, offered only when there is
// something to save that can be saved.
//
// THE DRAFT IS THE PERSON'S. It starts from the kit that was read, and a kit
// read again (a save somewhere else, a save here coming back with its pictures
// kept) replaces it only while nothing here has changed. A save is held to the
// revision the draft started from, so one that would overwrite somebody else's
// is refused in the engine's words, and the draft stays; Discard changes reads
// the kit again and starts from it.
//
// THE PREVIEW is asked for 400 ms after the last change, never on every
// keystroke, and only for a kit the page could send: a colour that is not one
// leaves the last email on screen. It fills the height the pane shows, its
// frame keeping its own scroll, and keeps in view the part of the email the
// person is editing. What the engine says about the kit -- a contrast below AA,
// pictures that leave an email little room -- stands under the frame, so it
// never moves it, and never stops a save.
//
// THE PART BEING EDITED IS DRAWN ON A LAYOUT THAT HAS IT. No one built-in
// layout carries every brand part: Article has the full-width header, the reply
// and the footer, and no follow line, social links or coloured panel; Letter
// has the logo as it is, the signature, the follow line and the social links;
// Confirmation has the logo on a coloured panel. So the group in focus, and
// within Logo the picture in focus, chooses the layout.

const PREVIEW_PAUSE_MS = 400;

const ARTICLE = "builtin:article";
const LETTER = "builtin:letter";
const CONFIRMATION = "builtin:confirmation";

/** The layout each group's part is drawn on: one that has it. */
const LAYOUT_OF_GROUP: Readonly<Record<string, string>> = {
  Colours: ARTICLE,
  Fonts: ARTICLE,
  Logo: LETTER,
  Signature: LETTER,
  Social: LETTER,
  Reply: ARTICLE,
  Footer: ARTICLE,
};

/** Within Logo, the two pictures another layout draws (by the picture field's name). */
const LAYOUT_OF_PICTURE: Readonly<Record<string, string>> = {
  "logo for coloured panels": CONFIRMATION,
  "full-width header": ARTICLE,
};

/**
 * Where each group's part sits in the email the preview draws it on: the
 * look and the logo from its start, the signature, the social links, the
 * reply and the footer at its end.
 */
const PART_OF_GROUP: Readonly<Record<string, PreviewPlace["part"]>> = {
  Colours: "start",
  Fonts: "start",
  Logo: "start",
  Signature: "end",
  Social: "end",
  Reply: "end",
  Footer: "end",
};

/** A value that follows another once it has stopped changing for `ms`; the first value at once. */
function useSettled<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    if (Object.is(value, settled)) return;
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, settled, ms]);
  return settled;
}

/** What the draft started from: the kit as read (null for none), its revision, and the draft of it. */
interface Base {
  record: BrandKitRecord | null;
  draft: BrandDraft;
}

const baseOf = (record: BrandKitRecord | null): Base => ({ record, draft: draftFromKit(record?.kit ?? null) });

/** The same JSON value, whatever order its objects' keys arrived in. */
function sameValue(a: unknown, b: unknown): boolean {
  const sorted = (value: unknown): unknown =>
    Array.isArray(value)
      ? value.map(sorted)
      : value !== null && typeof value === "object"
        ? Object.fromEntries(Object.keys(value as object).sort().map((key) => [key, sorted((value as Record<string, unknown>)[key])]))
        : value;
  return JSON.stringify(sorted(a)) === JSON.stringify(sorted(b));
}

export function BrandPage({ accountId, organization, brand, uploads, onBack }: {
  accountId: string;
  /** The organization's name, as Settings resolved it. */
  organization: string;
  /** The organization's kit, as Settings reads it: the row and this page are one reading. */
  brand: Reading<BrandKitRecord | null>;
  uploads: UploadProvider;
  onBack: () => void;
}) {
  const head = <Head navigationKey={`brand:${accountId}`} title="Brand" meta={organization} back={{ label: "Settings", onSelect: onBack }} />;
  const context = JSON.stringify({ page: "Brand", name: organization, id: accountId });

  // The kit is read before anything is drawn to edit it: a form of defaults
  // shown first would be a guess somebody could start changing.
  if (brand.readAt === "") {
    return (
      <div className="os-action-pane" data-os-page-context={context}>
        <div className="os-action-body os-app-stack">
          {head}
          {brand.state === "error" ? (
            <Notice tone="error" sentence="This cluster did not return the brand." detail={refusalSentence(brand.error, "brandKitForAccount")}>
              <Button onClick={() => void brand.reload()}>Try again</Button>
            </Notice>
          ) : (
            <ContentSkeleton kind="form" label="Loading the brand" />
          )}
        </div>
      </div>
    );
  }
  return <BrandEditor key={accountId} accountId={accountId} head={head} context={context} brand={brand} uploads={uploads} />;
}

function BrandEditor({ accountId, head, context, brand, uploads }: {
  accountId: string;
  head: ReactNode;
  context: string;
  brand: Reading<BrandKitRecord | null>;
  uploads: UploadProvider;
}) {
  const saving = useSaveBrandKit();
  const [base, setBase] = useState<Base>(() => baseOf(brand.value));
  const [draft, setDraft] = useState<BrandDraft>(base.draft);
  const [lost, setLost] = useState(false);
  const change: ChangeDraft = useCallback((edit) => setDraft((held) => edit(held)), []);

  const changed = !sameKit(draft, base.draft);
  const problem = draftProblem(draft);
  const exists = base.record !== null;
  const offerSave = problem === "" && (changed || !exists);

  // A kit read again stands in for the draft only while nothing here has
  // changed -- or when what is stored is the draft itself: a save whose answer
  // was lost, which landed.
  const incoming = brand.value;
  useEffect(() => {
    if (brand.readAt === "") return;
    if (incoming?.revision === base.record?.revision && sameValue(incoming?.kit ?? null, base.record?.kit ?? null)) return;
    const landed = incoming !== null && sameValue(incoming.kit, kitFromDraft(draft));
    if (changed && !landed) return;
    const next = baseOf(incoming);
    setBase(next);
    setDraft(next.draft);
    if (landed) setLost(false);
    // Only a new reading is a reason to look; the draft moving is not.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [incoming]);

  // The pictures picked on this page, drawn while the kit names their uploads.
  const [seen, setSeen] = useState<Record<string, string>>({});
  const pictures: BrandPictures = useMemo(
    () => ({ uploads, seen, saw: (value, picture) => setSeen((held) => (picture === "" ? held : { ...held, [value]: picture })) }),
    [uploads, seen],
  );

  // The kit the preview draws: the draft as it would be sent, once it has
  // stopped changing, and only when it could be sent. The same kit is the same
  // object, so a change that sends nothing new asks for nothing.
  const sendable = problem === "" ? kitFromDraft(draft) : null;
  const sendableKey = sendable === null ? "" : JSON.stringify(sendable);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const stable = useMemo(() => sendable, [sendableKey]);
  const [layout, setLayout] = useState(ARTICLE);
  const preview = useBrandPreview(accountId, useSettled(stable, PREVIEW_PAUSE_MS), layout);
  const drawable = draft.logo.src !== "" && draft.logo.alt.trim() !== "";

  // THE PART BEING EDITED, ON A LAYOUT THAT HAS IT, KEPT IN VIEW. Focus in a
  // group, or in one of Logo's pictures, draws the email on the layout that
  // has its part. Focus entering a group asks the preview for that group's
  // part of the email; moving between the fields of one group is staying in
  // it. Asked again on every entry, so coming back to a group the person
  // scrolled away from brings its part back.
  const [place, setPlace] = useState<PreviewPlace>({ part: "start", asked: 0 });
  function entered(event: FocusEvent<HTMLDivElement>) {
    const target = event.target instanceof Element ? event.target : null;
    const group = target?.closest(".os-details-section") ?? null;
    if (target === null || group === null) return;
    const name = group.getAttribute("aria-label") ?? "";
    const picture = target.closest("[data-picture]")?.getAttribute("data-picture") ?? "";
    const drawnOn = LAYOUT_OF_PICTURE[picture] ?? LAYOUT_OF_GROUP[name];
    if (drawnOn !== undefined) setLayout(drawnOn);
    if (event.relatedTarget instanceof Node && group.contains(event.relatedTarget)) return;
    const part = PART_OF_GROUP[name];
    if (part !== undefined) setPlace((held) => ({ part, asked: held.asked + 1 }));
  }

  // A refusal stands at the top of the editor; the person pressed Save at the bottom.
  const refusal = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (saving.error !== "") refusal.current?.scrollIntoView?.({ block: "nearest" });
  }, [saving.error]);

  async function save() {
    const sent = draft;
    const kit = kitFromDraft(sent);
    setLost(false);
    const revision = await saving.save(accountId, kit, base.record?.revision ?? "");
    if (revision !== "") {
      setBase({ record: { kit, revision }, draft: sent });
      // Settings' row, and the kit as the engine keeps it, pictures and all.
      void brand.reload();
      return;
    }
    if (saving.lost()) {
      setLost(true);
      void brand.reload();
    }
  }

  function discard() {
    saving.reset();
    setLost(false);
    setDraft(base.draft);
    // A kit saved somewhere else since is what the draft starts from now.
    void brand.reload();
  }

  const acts: Act[] = [];
  if (changed && !saving.busy) acts.push({ label: "Discard changes", text: true, onAct: discard });
  if (offerSave) acts.push({ label: "Save", tone: "primary", busy: saving.busy, onAct: () => void save() });
  const state = saving.busy ? "Saving" : changed ? "Not saved yet" : exists ? "Saved" : "Not set up";
  const tone: ActionBarTone = saving.busy ? "busy" : changed ? "none" : exists ? "live" : "paused";

  const warnings = preview.value?.warnings ?? [];
  const previewFailure =
    preview.state === "error" ? (
      <Notice tone="warn" sentence="This can't be previewed yet." detail={refusalSentence(preview.error, "campaignBrandPreview")}>
        <Button onClick={() => void preview.reload()}>Try again</Button>
      </Notice>
    ) : null;

  return (
    <div className="os-action-pane" data-os-page-context={context}>
      <div className="os-action-body email-brand-body">
        <AutoRefresh onRefresh={brand.reload} busy={brand.state === "loading"} />
        <div className="email-brand">
          <div className="email-brand-editor" onFocus={entered}>
            {head}
            <div ref={refusal}>
              {lost ? (
                <Notice tone="warn" sentence="No answer came back, so the brand may have been saved." next="Saving again is safe." detail={saving.error} />
              ) : saving.error !== "" ? (
                <Notice tone="error" sentence="The brand wasn't saved." next="Your changes are still here." detail={saving.error} />
              ) : null}
            </div>
            <Details>
              <ColoursGroup draft={draft} change={change} />
              <FontsGroup draft={draft} change={change} />
              <LogoGroup draft={draft} change={change} pictures={pictures} />
              <SignatureGroup draft={draft} change={change} pictures={pictures} />
              <SocialGroup draft={draft} change={change} pictures={pictures} />
              <FooterGroup draft={draft} change={change} />
              <ReplyGroup draft={draft} change={change} />
            </Details>
          </div>
          <section className="email-brand-preview" aria-label="Preview">
            {drawable ? (
              <>
                <Preview
                  html={preview.value?.html ?? ""}
                  loading={preview.value === null && previewFailure === null}
                  failed={preview.value === null ? previewFailure : null}
                  place={place}
                />
                {/* What is said about the email stands under it: coming and
                    going while somebody types, it never moves the frame. */}
                {preview.value === null ? null : previewFailure}
                {warnings.length === 0 ? null : warnings.length === 1 ? (
                  <Notice tone="warn" sentence={warnings[0]!.message} />
                ) : (
                  <Notice tone="warn">
                    <ul className="email-issues">
                      {warnings.map((warning, at) => (
                        <li key={`${warning.code}:${at}`}>{warning.message}</li>
                      ))}
                    </ul>
                  </Notice>
                )}
              </>
            ) : (
              <EmptyState icon={Mail} title="No preview yet">
                Add a logo and describe it to see an email in this brand.
              </EmptyState>
            )}
          </section>
        </div>
      </div>

      <ActionBar state={state} detail={saving.busy ? undefined : problem || undefined} tone={tone} acts={acts} />
    </div>
  );
}
