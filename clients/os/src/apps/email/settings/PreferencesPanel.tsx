import { Caption, Field, Select, Switch } from "../../../kit";
import { accessAdmits } from "../../../system/registry";
import { EMAIL_OPENING_TABS, type EmailSettings } from "../settings";

// PREFERENCES: how this browser opens and lists Email.
//
// Not per organization, so they come last, after everything an organization
// owns. A group, named by its legend, in the settings grammar (DESIGN.md rule
// 8): the fieldset keeps its semantics and the legend dresses as a subject's
// heading. Every preference is named ONCE -- by its switch's words, or by its
// field's label -- and is a control of its own kind: a true on/off is a switch.
//
// They are kept in this browser (`LocalEmailSettingsStore`), which the last line
// says, because a different browser starts from the defaults.

export function PreferencesPanel({ settings, update }: {
  settings: EmailSettings;
  update: (patch: Partial<EmailSettings>) => void;
}) {
  // A tab is offered only if this person can open it. Decided on every draw, and
  // the page that draws this is redrawn when the person's access changes.
  const tabs = EMAIL_OPENING_TABS.filter((tab) => accessAdmits(tab.requires));
  // A saved tab this person can no longer open is not shown as chosen: the
  // window opens on the first, which is what the picker then says.
  const opening = tabs.some((tab) => tab.id === settings.defaultSection) ? settings.defaultSection : (tabs[0]?.id ?? "");

  return (
    <fieldset className="os-field-group">
      <legend>Preferences</legend>
      <div className="os-app-stack">
        <Switch checked={settings.includeArchived} onChange={(includeArchived) => update({ includeArchived })}>
          Include archived
        </Switch>
        <div className="email-preference">
          <Switch checked={settings.trackByDefault} onChange={(trackByDefault) => update({ trackByDefault })}>
            Track opens and clicks
          </Switch>
          <Caption>New campaigns start with this on. Existing campaigns stay unchanged.</Caption>
        </div>
        <div className="email-field">
          <Field label="Opening tab">
            <Select id="email-opening-tab" label="Opening tab" value={opening} onChange={(defaultSection) => update({ defaultSection })}>
              {tabs.map((tab) => (
                <option key={tab.id} value={tab.id}>{tab.name}</option>
              ))}
            </Select>
          </Field>
        </div>
        <Caption>Saved in this browser.</Caption>
      </div>
    </fieldset>
  );
}
