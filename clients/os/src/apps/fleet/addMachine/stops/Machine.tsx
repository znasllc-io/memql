import type { LocalCockpitInstall } from "../localInstall";
import { Caption, Switch, Field, Input, Notice, type ChoiceOption } from "../../../../kit";
import { InfoDetail } from "../../../../kit/InfoDetail";
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
  const localTest = draft.platform === "mac" ? configuredTest : null;
  return (
    <div className="os-stop-body os-fleet-addstop">
      {mintError === "" ? null : (
        <Notice
          tone="error"
          sentence="The connection token could not be created."
          next="Try creating the token again."
          detail={mintError}
        />
      )}

      {localTest ? <Notice sentence={`Local Cockpit test · ${localTest.version}`} next="Installs the tested macOS app for your account with computer use enabled. Run this command on this Mac; the download is served locally." /> : null}
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
      {/* THE CHOICE ROW, not the choice stack. The stack is for options that
          each need a sentence; "macOS" and "Linux" explain themselves, and as
          two full-width cards they were the heaviest thing in a form whose
          first question is the name above them. */}
      <Field label="Operating system">
        <div className="os-choice-row" role="radiogroup" aria-label="Operating system">
          {PLATFORMS.map((option) => (
            <button key={option.value} type="button" role="radio" className="os-choice" aria-checked={draft.platform === option.value}
              onClick={() => onDraft({ platform: option.value })}>
              {option.label}
            </button>
          ))}
        </div>
      </Field>

      <div className="fleet-install-options">
        <div><Switch disabled={localTest !== null} checked={draft.userLocal ?? false} onChange={(userLocal) => onDraft({ userLocal })}>Install for my account only</Switch>
          <p>No password needed. Otherwise, installation uses your account password.</p>
          <InfoDetail title="Installation location"><p>Account-only installs in ~/.memql/bin. System installation places a protected command in /usr/local/bin. Both connect this computer to your cluster.</p></InfoDetail></div>
        <div><Switch disabled={localTest !== null} checked={draft.computerUse} onChange={(computerUse) => onDraft({ computerUse })}>Computer use</Switch>
          <p>{draft.platform === "mac" ? "Mouse, keyboard and screenshots. Requires Accessibility and Screen Recording permission." : "Mouse, keyboard and screenshots require an X11 desktop. Other tools also work on Wayland."}</p></div>
        <div><Switch checked={draft.inference} onChange={(inference) => onDraft({ inference })}>Run local models</Switch>
          <p>Downloads models after installation. Allow several gigabytes of disk space.</p>
          <InfoDetail title="Local model setup"><p>A second command checks your hardware and asks you to approve the runtime setup before downloading recommended models. Downloads may take time; progress and readiness stay visible here.</p></InfoDetail></div>
      </div>

      {connected ? null : (
        <Caption>Reconnect to the cluster to create a connection token.</Caption>
      )}
    </div>
  );
}
