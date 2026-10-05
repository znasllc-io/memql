import { useId } from "react";
import type { LocalCockpitInstall } from "../localInstall";
import { Caption, Switch, Field, Input, Notice, type ChoiceOption } from "../../../../kit";
import type { Draft } from "../flow";
import { INSTALL_PLATFORMS, INSTALL_PLATFORM_LABEL, type InstallPlatform } from "../install";

// THIS MACHINE: what it is called, and what it will run. The form, and the
// only stop the person answers by typing.
//
// The two choices are INPUTS to the mint -- they decide which install
// command the next stop shows -- so they stay with the name that produces
// it. They are checkboxes in a form, which is what rule 10 of the interface
// language reserves them for: a choice being stated, not in-surface state.

const PLATFORMS: readonly (ChoiceOption & { value: InstallPlatform })[] = INSTALL_PLATFORMS.map(
  (platform) => ({ value: platform, label: INSTALL_PLATFORM_LABEL[platform] }),
);

export function MachineStop({
  draft,
  localTest: configuredTest = null,
  onDraft,
  connected,
  mintError,
  onMint,
}: {
  draft: Draft;
  localTest?: LocalCockpitInstall | null;
  onDraft: (patch: Partial<Draft>) => void;
  connected: boolean;
  /** The server's refusal, verbatim, or "". */
  mintError: string;
  /** Enter in the name field. The bar's act decides whether a mint is legal;
   *  this only asks. */
  onMint: () => void;
}) {
  const platformGroup = useId();
  const localTest = draft.platform === "mac" ? configuredTest : null;
  return (
    <div className="os-stop-body os-fleet-addstop os-fleet-machine">
      {mintError === "" ? null : (
        <Notice
          tone="error"
          sentence="The connection token could not be created."
          next="Try creating the token again."
          detail={mintError}
        />
      )}

      {localTest ? <Notice sentence={`Local Cockpit test · ${localTest.version}`} next="Use this Mac. Account-only installation and computer use are included." /> : null}
      <Field label="Machine name">
        <Input
          id="fleet-add-name"
          label="Machine name"
          value={draft.name}
          placeholder={draft.platform === "linux" ? "pop-os-desktop" : "studio-mac-mini"}
          onChange={(name) => onDraft({ name })}
          onEnter={onMint}
        />
      </Field>
      <Field label="Operating system">
        <div className="os-fleet-platforms" role="radiogroup" aria-label="Operating system">
          {PLATFORMS.map((option) => (
            <label key={option.value}>
              <input className="os-sr-only" type="radio" name={platformGroup} value={option.value}
                checked={draft.platform === option.value} onChange={() => onDraft({ platform: option.value })} />
              <span>{option.label}</span>
            </label>
          ))}
        </div>
      </Field>

      <div className="fleet-install-options">
        <div><Switch disabled={localTest !== null} checked={draft.userLocal ?? false} onChange={(userLocal) => onDraft({ userLocal })}>Install for my account only</Switch>
          <p>{draft.userLocal ? "No administrator password needed." : "Installation will ask for your administrator password."}</p></div>
        <div><Switch disabled={localTest !== null} checked={draft.computerUse} onChange={(computerUse) => onDraft({ computerUse })}>Computer use</Switch>
          <p>{draft.platform === "mac" ? "Mouse, keyboard and screenshots. Requires Accessibility and Screen Recording access." : "Mouse, keyboard and screenshots require X11. Other tools work on Wayland."}</p></div>
        <div><Switch checked={draft.inference} onChange={(inference) => onDraft({ inference })}>Run local models</Switch>
          <p>Run models on this machine. Downloads require several gigabytes.</p></div>
      </div>

      {connected ? null : (
        <Caption>Reconnect to the cluster to create a connection token.</Caption>
      )}
    </div>
  );
}
