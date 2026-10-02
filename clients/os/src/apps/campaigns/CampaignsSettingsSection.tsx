import { useId } from "react";
import { Check, Field, Head, Panel, Select, Subhead } from "../../kit";
import { AzureEmailConnections } from "../../modules/connections/AzureEmailConnections";
import { CAMPAIGNS_SECTIONS, type CampaignsSettings } from "./settings";
import "./settings.css";

export function CampaignsSettingsSection({ settings, update }: {
  settings: CampaignsSettings;
  update: (patch: Partial<CampaignsSettings>) => void;
}) {
  const id = useId();
  return <div className="os-settings os-settings-wide campaigns-settings">
    <AzureEmailConnections header={<Head title="Settings" />} backLabel="Settings">
      <Panel label="Campaign preferences">
        <Subhead>Preferences</Subhead>
        <fieldset className="os-field-group campaigns-preferences">
          <legend className="os-sr-only">Campaign preferences</legend>
          <div className="campaigns-preference">
            <div><span className="campaigns-preference-name">Opening page</span><p className="os-caption">When you open a new Campaigns window.</p></div>
            <Field label="Opening page"><Select id={`${id}-opening-page`} label="Opening page" value={settings.defaultSection} onChange={defaultSection => update({ defaultSection })}>
              {CAMPAIGNS_SECTIONS.map(section => <option key={section.id} value={section.id}>{section.name}</option>)}
            </Select></Field>
          </div>
          <div className="campaigns-preference">
            <div><span className="campaigns-preference-name">Archived items</span><p className="os-caption">Include finished campaigns, archived audiences and templates, and retired senders.</p></div>
            <Check checked={settings.showFiled} onChange={showFiled => update({ showFiled })}>Show archived items</Check>
          </div>
          <div className="campaigns-preference">
            <div><span className="campaigns-preference-name">Tracking</span><p className="os-caption">Track opens and clicks on new campaigns. Existing campaigns stay unchanged.</p></div>
            <Check checked={settings.trackByDefault} onChange={trackByDefault => update({ trackByDefault })}>Enable tracking</Check>
          </div>
        </fieldset>
        <p className="os-caption">Preferences are saved in this browser.</p>
      </Panel>
    </AzureEmailConnections>
  </div>;
}
