import { Caption, DetailsSection, Input, Select, SettingRow } from "../../../../kit";
import { PictureField, type BrandPictures } from "./PictureField";
import { groundOf, type BrandDraft, type ChangeDraft } from "./kit";

// THE BRAND'S PICTURES: the logo, in up to three versions, and the signature.

interface GroupProps {
  draft: BrandDraft;
  change: ChangeDraft;
  pictures: BrandPictures;
}

export function LogoGroup({ draft, change, pictures }: GroupProps) {
  const set = (patch: Partial<BrandDraft["logo"]>) => change((held) => ({ ...held, logo: { ...held.logo, ...patch } }));
  const { logo } = draft;
  // The reversed logo is made for a coloured panel, and is drawn on one.
  const canvas = groundOf(draft, "canvas");
  const panel = groundOf(draft, "primary");
  return (
    <DetailsSection title="Logo">
      <SettingRow label="Picture">
        <PictureField name="logo" value={logo.src} pictures={pictures} ground={canvas} onChange={(src) => set({ src })} />
      </SettingRow>
      <SettingRow label="Description">
        <div className="email-brand-stack">
          <Input id="email-brand-logo-alt" label="Logo description" value={logo.alt} onChange={(alt) => set({ alt })} />
          <Caption>Read out for people who can't see the logo.</Caption>
        </div>
      </SettingRow>
      <SettingRow label="Link">
        <Input id="email-brand-logo-href" label="Logo link" placeholder="https://" value={logo.href} onChange={(href) => set({ href })} />
      </SettingRow>
      <SettingRow label="On coloured panels">
        <PictureField
          name="logo for coloured panels"
          value={logo.onColour}
          pictures={pictures}
          ground={panel}
          hint="A reversed logo for coloured panels."
          onChange={(onColour) => set({ onColour })}
        />
      </SettingRow>
      <SettingRow label="Full-width header">
        <PictureField
          name="full-width header"
          value={logo.wide}
          pictures={pictures}
          ground={canvas}
          hint="A picture across the top of layouts that use one."
          onChange={(wide) => set({ wide })}
        />
      </SettingRow>
    </DetailsSection>
  );
}

export function SignatureGroup({ draft, change, pictures }: GroupProps) {
  const set = (patch: Partial<BrandDraft["signature"]>) => change((held) => ({ ...held, signature: { ...held.signature, ...patch } }));
  const { signature } = draft;
  const canvas = groundOf(draft, "canvas");
  return (
    <DetailsSection title="Signature">
      <SettingRow label="Photo">
        <PictureField name="signature photo" value={signature.photo} pictures={pictures} ground={canvas} onChange={(photo) => set({ photo })} />
      </SettingRow>
      <SettingRow label="Name">
        <div className="email-brand-name">
          <Select
            id="email-brand-name-as"
            label="Show the name as"
            value={signature.nameAs}
            onChange={(nameAs) => set({ nameAs: nameAs === "picture" ? "picture" : "text" })}
          >
            <option value="text">Text</option>
            <option value="picture">A picture</option>
          </Select>
          {signature.nameAs === "text" ? (
            <Input id="email-brand-name" label="Signature name" value={signature.nameText} onChange={(nameText) => set({ nameText })} />
          ) : (
            <PictureField name="name picture" value={signature.nameSrc} pictures={pictures} ground={canvas} onChange={(nameSrc) => set({ nameSrc })} />
          )}
        </div>
      </SettingRow>
      <SettingRow label="Title">
        <Input id="email-brand-title" label="Signature title" value={signature.title} onChange={(title) => set({ title })} />
      </SettingRow>
      <SettingRow label="As one picture">
        <PictureField
          name="signature picture"
          value={signature.picture}
          pictures={pictures}
          ground={canvas}
          hint="The whole signature as one picture. Most layouts use it in place of the parts."
          onChange={(picture) => set({ picture })}
        />
      </SettingRow>
    </DetailsSection>
  );
}
