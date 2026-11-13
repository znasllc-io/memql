import { useAsk } from "../../ask/AskProvider";
import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { Caption, Details, DetailsSection, Fact, Facts, Head, SettingRow, Switch } from "../../kit";
import { ThemeSwitch } from "../../kit/ThemeSwitch";
import { RoleIdentity } from "../../modules/profile/RoleIdentity";
import { themePacks } from "../../themes/registry";

export function AboutSection() {
  const { access, config } = useSession();
  return <div className="os-app-stack">
    <Head title="About" />
    <Details><DetailsSection title="This session"><Facts>
      <Fact label="Cluster" value={config.domain || "Not reported"} copyValue={config.domain || undefined} />
      <Fact label="Signed in as" value={access?.primaryEmail || "Not reported"} copyValue={access?.primaryEmail || undefined} />
      <Fact label="Cluster role" value={<RoleIdentity access={access} />} />
      <Fact label="Shell" value="MemQL OS" />
      <Fact label="Shell build" value={__OS_BUILD__} copyValue={__OS_BUILD__} />
    </Facts></DetailsSection></Details>
  </div>;
}

export function AppearanceSection() {
  const { state, actions } = useOs();
  return <div className="os-app-stack">
    <Head title="Appearance" />
    <Details>
      <DetailsSection title="Display"><SettingRow label="Mode"><ThemeSwitch /></SettingRow></DetailsSection>
      <DetailsSection title="Theme"><SettingRow label="Installed themes">
        <div className="os-choice-row" role="radiogroup" aria-label="Theme">
          {themePacks(state.installedPacks).map(pack => <button key={pack.id} type="button" role="radio"
            aria-checked={state.themePack === pack.id} className="os-choice" onClick={() => actions.setThemePack(pack.id)}>
            {pack.builtIn ? `${pack.name} (built in)` : pack.name}
          </button>)}
        </div>
        <Caption>Themes in the Launcher lets you preview and add themes.</Caption>
      </SettingRow></DetailsSection>
    </Details>
  </div>;
}

/** Personal voice preferences retain the shared Ask state and transport;
 * moving their controls does not create a second preference store. */
export function AskSection() {
  const { settings, updateSettings } = useAsk();
  return <div className="os-app-stack">
    <Head title="Ask" />
    <Details>
      <DetailsSection title="Dictation"><SettingRow label="When you stop talking">
        <div className="os-choice-row" role="radiogroup" aria-label="When you stop talking">
          {([["send", "Send it"], ["review", "Put it in the box"]] as const).map(([value, label]) =>
            <button key={value} type="button" role="radio" aria-checked={settings.commit === value}
              className="os-choice" onClick={() => updateSettings({ commit: value })}>{label}</button>)}
        </div>
      </SettingRow></DetailsSection>
      <DetailsSection title="Voice"><SettingRow label="MemQL’s voice">
        <div className="os-choice-row" role="radiogroup" aria-label="MemQL’s voice">
          {(["female", "male"] as const).map(voice => <button key={voice} type="button" role="radio"
            aria-checked={(settings.voice ?? "female") === voice} className="os-choice"
            onClick={() => updateSettings({ voice })}>{voice === "female" ? "Female" : "Male"}</button>)}
        </div>
      </SettingRow></DetailsSection>
      <DetailsSection title="Keyboard"><SettingRow label="Hold Space to talk">
        <Switch checked={settings.spaceToTalk} onChange={spaceToTalk => updateSettings({ spaceToTalk })}>Hold Space to talk</Switch>
      </SettingRow></DetailsSection>
    </Details>
  </div>;
}
