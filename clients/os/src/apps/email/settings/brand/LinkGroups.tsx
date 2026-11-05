import { useEffect, useState } from "react";
import { Trash2 } from "lucide-react";

import { DetailsSection, Input, SettingRow, TextArea } from "../../../../kit";
import { AddButton } from "../../../../kit/AddButton";
import { IconButton } from "../../../../kit/IconButton";
import { PageHelp } from "../../../../kit/pageHelp";
import { PictureField, type BrandPictures } from "./PictureField";
import { FIXED_FOOTER_LINKS, entryKey, groundOf, type BrandDraft, type ChangeDraft } from "./kit";

// WHERE THE BRAND LEADS: its social links (with the follow line that stands
// above them in every layout that has both), its footer and its invitation to
// reply. The two collections are edited in place, an Add control after their
// entries and a Remove on each entry the person may remove (DESIGN.md,
// "Editable collections"). Removing is a control of the field, not an act of
// the page, so it is not in the bar.

interface GroupProps {
  draft: BrandDraft;
  change: ChangeDraft;
}

/**
 * Brings the keyboard to a field once it is drawn: an entry added is where the
 * next keystroke goes. (An entry removed hands it to its collection's Add,
 * which is already there.)
 */
function useFocusWhenDrawn(): (id: string) => void {
  const [target, setTarget] = useState("");
  useEffect(() => {
    if (target === "") return;
    document.getElementById(target)?.focus();
    setTarget("");
  }, [target]);
  return setTarget;
}

const SOCIAL_ADD = "email-brand-social-add";
const FOOTER_ADD = "email-brand-footer-add";
const focusOn = (id: string) => document.getElementById(id)?.focus();

export function SocialGroup({ draft, change, pictures }: GroupProps & { pictures: BrandPictures }) {
  const focusWhenDrawn = useFocusWhenDrawn();
  const setEntry = (key: string, patch: Partial<BrandDraft["social"][number]>) =>
    change((held) => ({ ...held, social: held.social.map((entry) => (entry.key === key ? { ...entry, ...patch } : entry)) }));
  return (
    <DetailsSection title="Social">
      <SettingRow label="Follow line">
        <Input id="email-brand-follow" label="Follow line" value={draft.follow} onChange={(follow) => change((held) => ({ ...held, follow }))} />
      </SettingRow>
      {draft.social.map((entry, index) => {
        const which = `social link ${index + 1}`;
        return (
          <div className="email-brand-entry" key={entry.key} role="group" aria-label={`Social link ${index + 1}`}>
            <SettingRow label="Network">
              <div className="email-brand-line">
                <Input id={`email-brand-${entry.key}-network`} label={`Network of ${which}`} value={entry.network} onChange={(network) => setEntry(entry.key, { network })} />
                <IconButton
                  label={`Remove ${which}`}
                  onClick={() => {
                    focusOn(SOCIAL_ADD);
                    change((held) => ({ ...held, social: held.social.filter((one) => one.key !== entry.key) }));
                  }}
                >
                  <Trash2 size={16} aria-hidden />
                </IconButton>
              </div>
            </SettingRow>
            <SettingRow label="Address">
              <Input id={`email-brand-${entry.key}-href`} label={`Address of ${which}`} placeholder="https://" value={entry.href} onChange={(href) => setEntry(entry.key, { href })} />
            </SettingRow>
            <SettingRow label="Icon">
              <PictureField name={`icon of ${which}`} value={entry.icon} pictures={pictures} ground={groundOf(draft, "canvas")} onChange={(icon) => setEntry(entry.key, { icon })} />
            </SettingRow>
          </div>
        );
      })}
      <SettingRow label="">
        <AddButton
          id={SOCIAL_ADD}
          label="Add a social link"
          onClick={() => {
            const key = entryKey("social");
            change((held) => ({ ...held, social: [...held.social, { key, network: "", href: "", icon: "" }] }));
            focusWhenDrawn(`email-brand-${key}-network`);
          }}
        />
      </SettingRow>
    </DetailsSection>
  );
}

export function FooterGroup({ draft, change }: GroupProps) {
  const focusWhenDrawn = useFocusWhenDrawn();
  const { footer } = draft;
  const setLink = (key: string, patch: Partial<BrandDraft["footer"]["links"][number]>) =>
    change((held) => ({ ...held, footer: { ...held.footer, links: held.footer.links.map((link) => (link.key === key ? { ...link, ...patch } : link)) } }));
  return (
    <DetailsSection title="Footer">
      <PageHelp title="Footer">
        <p>
          Every email carries a link to the person's preferences, one to unsubscribe and one to read it in a browser. MemQL
          writes each address for each person, so those three stay; their words are yours to change.
        </p>
        <p>Leave the separator empty to use the layouts' own.</p>
      </PageHelp>
      <SettingRow label="Lines">
        <TextArea
          id="email-brand-footer-lines"
          label="Footer lines"
          rows={3}
          value={footer.lines}
          onChange={(lines) => change((held) => ({ ...held, footer: { ...held.footer, lines } }))}
        />
      </SettingRow>
      <SettingRow label="Links">
        <div className="email-brand-links">
          {footer.links.map((link, index) => {
            const fixed = link.fixed ? FIXED_FOOTER_LINKS.find((one) => one.href === link.href) : undefined;
            if (fixed !== undefined) {
              return (
                <div className="email-brand-link" key={link.key}>
                  <Input id={`email-brand-${link.key}-label`} label={`Label of the ${fixed.name} link`} value={link.label} onChange={(label) => setLink(link.key, { label })} />
                  <span className="email-brand-fixed">{fixed.does}</span>
                </div>
              );
            }
            const which = `footer link ${index + 1}`;
            return (
              <div className="email-brand-link" key={link.key}>
                <Input id={`email-brand-${link.key}-label`} label={`Label of ${which}`} value={link.label} onChange={(label) => setLink(link.key, { label })} />
                <Input id={`email-brand-${link.key}-href`} label={`Address of ${which}`} placeholder="https://" value={link.href} onChange={(href) => setLink(link.key, { href })} />
                <IconButton
                  label={`Remove ${which}`}
                  onClick={() => {
                    focusOn(FOOTER_ADD);
                    change((held) => ({ ...held, footer: { ...held.footer, links: held.footer.links.filter((one) => one.key !== link.key) } }));
                  }}
                >
                  <Trash2 size={16} aria-hidden />
                </IconButton>
              </div>
            );
          })}
          <div>
            <AddButton
              id={FOOTER_ADD}
              label="Add a footer link"
              onClick={() => {
                const key = entryKey("link");
                change((held) => ({ ...held, footer: { ...held.footer, links: [...held.footer.links, { key, label: "", href: "", fixed: false }] } }));
                focusWhenDrawn(`email-brand-${key}-label`);
              }}
            />
          </div>
        </div>
      </SettingRow>
      <SettingRow label="Separator">
        <div className="email-brand-short">
          <Input
            id="email-brand-separator"
            label="Separator"
            placeholder="|"
            value={footer.separator ?? ""}
            onChange={(separator) => change((held) => ({ ...held, footer: { ...held.footer, separator: separator === "" ? null : separator } }))}
          />
        </div>
      </SettingRow>
    </DetailsSection>
  );
}

export function ReplyGroup({ draft, change }: GroupProps) {
  const set = (patch: Partial<BrandDraft["reply"]>) => change((held) => ({ ...held, reply: { ...held.reply, ...patch } }));
  const { reply } = draft;
  return (
    <DetailsSection title="Reply">
      <PageHelp title="Reply">
        <p>An invitation to write back, in layouts that end with one. Leave it empty to leave it out.</p>
      </PageHelp>
      <SettingRow label="Heading">
        <Input id="email-brand-reply-heading" label="Reply heading" value={reply.heading} onChange={(heading) => set({ heading })} />
      </SettingRow>
      <SettingRow label="Button">
        <Input id="email-brand-reply-label" label="Button label" value={reply.label} onChange={(label) => set({ label })} />
      </SettingRow>
      <SettingRow label="Button link">
        <Input id="email-brand-reply-href" label="Button link" value={reply.href} onChange={(href) => set({ href })} />
      </SettingRow>
      <SettingRow label="Note">
        <Input id="email-brand-reply-note" label="Reply note" value={reply.note} onChange={(note) => set({ note })} />
      </SettingRow>
    </DetailsSection>
  );
}
