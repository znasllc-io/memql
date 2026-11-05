import { Caption, DetailsSection, Select, SettingRow, Switch } from "../../../../kit";
import { PageHelp } from "../../../../kit/pageHelp";
import { ColourField } from "./ColourField";
import { BRAND_COLOURS, BRAND_FONTS, type BrandDraft, type ChangeDraft } from "./kit";

// THE BRAND'S LOOK: its eight colours and its two fonts. What each colour is
// for is said once, in the page's help, rather than under every field.

export function ColoursGroup({ draft, change }: { draft: BrandDraft; change: ChangeDraft }) {
  return (
    <DetailsSection title="Colours">
      <PageHelp title="Colours">
        <p>
          Page is behind the email, and canvas is the email's own column. Headings, buttons and links in the text take
          primary; footer links take secondary; a highlighted statement stands on accent; quieter panels take tint. Text is
          for the email's words, and muted for its quieter lines, such as the footer's.
        </p>
        <p>Contrast that may be hard to read is listed beside the preview. It never stops a save: a brand's own colours win.</p>
      </PageHelp>
      {BRAND_COLOURS.map(({ key, name }) => (
        <SettingRow key={key} label={name}>
          <ColourField
            id={`email-brand-${key}`}
            name={name}
            value={draft.colours[key]}
            onChange={(value) => change((held) => ({ ...held, colours: { ...held.colours, [key]: value } }))}
          />
        </SettingRow>
      ))}
    </DetailsSection>
  );
}

/** The fonts offered, and a font the kit holds that they do not, so it is never silently changed. */
function FontSelect({ id, label, value, onChange }: { id: string; label: string; value: string; onChange: (font: string) => void }) {
  const fonts = value === "" || BRAND_FONTS.includes(value) ? BRAND_FONTS : [...BRAND_FONTS, value];
  return (
    <Select id={id} label={label} value={value} onChange={onChange}>
      {fonts.map((font) => (
        <option key={font} value={font}>
          {font}
        </option>
      ))}
    </Select>
  );
}

export function FontsGroup({ draft, change }: { draft: BrandDraft; change: ChangeDraft }) {
  return (
    <DetailsSection title="Fonts">
      <SettingRow label="Headings">
        <FontSelect id="email-brand-heading-font" label="Heading font" value={draft.headingFont} onChange={(headingFont) => change((held) => ({ ...held, headingFont }))} />
      </SettingRow>
      <SettingRow label="Body">
        <FontSelect id="email-brand-body-font" label="Body font" value={draft.bodyFont} onChange={(bodyFont) => change((held) => ({ ...held, bodyFont }))} />
      </SettingRow>
      <SettingRow label="Web fonts">
        <div className="email-preference">
          <Switch checked={draft.webFonts} onChange={(webFonts) => change((held) => ({ ...held, webFonts }))}>
            <span className="os-sr-only">Web fonts</span>
          </Switch>
          <Caption>Mail apps that allow it load these fonts from Google Fonts. The rest use a similar font.</Caption>
        </div>
      </SettingRow>
    </DetailsSection>
  );
}
