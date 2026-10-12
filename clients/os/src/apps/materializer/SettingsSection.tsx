import { Head, Select, SetupGroup, Switch } from "../../kit";
import { useSession } from "../../chrome/access";
import { MATERIALIZER_OPENING_SECTIONS, MATERIALIZER_SECTIONS, MATERIALIZER_REQUIRES, MATERIALIZER_WANTS, type MaterializerSettings } from "./settings";
import { FORMATS, formatWord } from "./words";

export function SettingsSection({settings,update}:{settings:MaterializerSettings;update:(patch:Partial<MaterializerSettings>)=>void}) {
  const {readiness}=useSession();
  return <div className="os-settings"><Head title="Settings"/>
    <SetupGroup app="Materializer" requires={MATERIALIZER_REQUIRES} wants={MATERIALIZER_WANTS} readiness={readiness}/>
    <fieldset className="os-field-group"><legend>Preferences</legend>
      <Select id="mz-opening-tab" label="Opening tab" value={settings.defaultSection} onChange={defaultSection=>update({defaultSection})}>
        {MATERIALIZER_SECTIONS.filter(s=>MATERIALIZER_OPENING_SECTIONS.includes(s.id)).map(s=><option key={s.id} value={s.id}>{s.name}</option>)}
      </Select>
      <Select id="mz-default-format" label="Kind of file to start on" value={settings.defaultFormat} onChange={defaultFormat=>update({defaultFormat})}>
        {FORMATS.map(f=><option key={f} value={f}>{formatWord(f)}</option>)}
      </Select>
      <Switch checked={settings.showArchived} onChange={showArchived=>update({showArchived})}>Include archived</Switch>
      <Switch checked={settings.showUnmarkedConcepts} onChange={showUnmarkedConcepts=>update({showUnmarkedConcepts})}>Offer every record type</Switch>
    </fieldset>
  </div>;
}
