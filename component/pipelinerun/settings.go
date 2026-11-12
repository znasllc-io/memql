package pipelinerun

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// settings.go -- saving the two installation-wide switches of Pipelines >
// Settings: the AI switches (pipelines.AISettingsVariable) and the weekly
// security scans switch (pipelines.SecuritySettingsVariable).
//
// # Why this is a capability and not the page calling setGlobalVariable
//
// The switches are v1:platform:globalVariable rows, and a variable's row is
// written by a cluster owner or by server code that has authorized its caller
// and stamps internal origin for the one write (component/memql
// platform_variable_write_guard.go): the resolver reads a row by name ahead of
// the environment, so its writers decide what a name means on every node. The
// Pipelines app is an owner's AND a developer's, so a developer's switch has to
// be written the second way -- which is this file. Three things follow from
// writing it here and not in the browser:
//
//  1. THE ROW IS NOT A PARAMETER. The caller sends switches. The variable's
//     name, the row's id, its description and the JSON the engine parses are
//     this package's, so the stamp below can reach these two rows and no
//     other, and a save cannot produce a value the reader would not accept.
//  2. THE ROW ID IS THE ONE ALREADY THERE. The resolver looks a row up by NAME,
//     so a second id would add a second row carrying the same name and not
//     update the first. The save writes the active row's own id, and the
//     seeder's derivation (`var-global-<slug>`, scripts/secrets and the default
//     injector) when there is none yet.
//  3. TWO ACTIVE ROWS ARE REFUSED, NOT GUESSED AT. The security reader refuses
//     more than one active row as ambiguous consent
//     (pipelineSystemVariableReader), so a save that updated one of them would
//     report success while the other still decided.
//
// # Who may save
//
// A holder of `execute app:pipelines/settings`, seeded on owner and developer
// (dsl/rbac/seeds.memql). The builtins declare it and the engine enforces it on
// the call; saveSetting asks again, once, because a stamp is earned by the
// order of its own function: the write is downstream of the check in this
// file, whatever calls it.
//
// # What a save does not do
//
// It changes no run. The engine reads each switch when a feature would use it
// (aiSettings, securitySettings), so a save applies to the next review, failure
// or scan and never to one already started. And it writes the WHOLE value: the
// two AI switches are one row, so a save states both, as the page always did.
//
// Regression tests: settings_test.go (who may save, the one read and the one
// stamped write, the row's name and id, a missing switch, an ambiguous row)
// and settings_db_test.go (against the real engine: an owner's and a
// developer's saves land and read back through the engine's own readers, and a
// developer's own setGlobalVariable is refused).

// settingsPart is the capability a settings save requires.
const settingsPart = "app:pipelines/settings"

const (
	aiSettingsDescription       = "Pipeline AI features: independent pull-request review and failed-run diagnosis. Off unless turned on in Pipelines settings."
	securitySettingsDescription = "Scheduled pipeline security scans and remediation goals. Off unless enabled in Pipelines settings."
)

// SettingRow is one installation-wide setting as its variable row carries it.
type SettingRow struct {
	// ID is the row's bare id.
	ID string
	// Name is the variable's name, which is what the resolver looks up.
	Name string
	// Value is the setting's whole value, as the engine's parser reads it.
	Value string
	// Description is the row's own account of what the variable is.
	Description string
}

// SettingsStore is the storage of the installation-wide settings. The
// production Store implements it (store_dsl.go); it is a separate port because
// only the two settings capabilities use it.
type SettingsStore interface {
	// ActiveSettingRows is every ACTIVE row carrying name, read under the
	// caller's own actor and fresh: the answer decides which row a save writes.
	ActiveSettingRows(ctx context.Context, name string) ([]SettingRow, error)
	// SaveSetting writes row under the caller's own actor with internal origin
	// stamped for the one write. The caller must already be authorized.
	SaveSetting(ctx context.Context, row SettingRow) error
}

// settingsAuthorized admits a caller who holds the settings part.
func settingsAuthorized(ctx context.Context) error {
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok || !auth.CapableFor(ctx, subject, auth.VerbExecute, settingsPart) {
		return fmt.Errorf("%s: saving a pipeline setting needs the settings part of the Pipelines app (execute on %s)",
			memql.CodeCapabilityNotHeld, settingsPart)
	}
	return nil
}

// switchArg reads one required boolean switch. A switch that was not sent, or
// is not a boolean, is refused: reading it as false would turn a feature off
// without anybody having chosen that.
func switchArg(args map[string]any, key string) (bool, error) {
	switch v := args[key].(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("pipelines: %s is required and must be true or false", key)
}

// seededVariableID is the seeder's row id for a global variable.
func seededVariableID(name string) string {
	return "var-global-" + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "_", "-"))
}

// saveSetting writes one setting's whole value to its own row. The caller is
// authorized here, before anything is read.
func (i *Integration) saveSetting(ctx context.Context, name, description, value string) error {
	if err := settingsAuthorized(ctx); err != nil {
		return err
	}
	store, ok := i.snapshot().Store.(SettingsStore)
	if !ok || store == nil {
		return errNoStore
	}
	rows, err := store.ActiveSettingRows(ctx, name)
	if err != nil {
		return err
	}
	if len(rows) > 1 {
		return fmt.Errorf("pipelines: more than one active row carries the setting %s, so a save would change one and leave the other deciding; keep one row and save again", name)
	}
	id := seededVariableID(name)
	if len(rows) == 1 && strings.TrimSpace(rows[0].ID) != "" {
		id = bareID(rows[0].ID)
	}
	return store.SaveSetting(ctx, SettingRow{ID: id, Name: name, Value: value, Description: description})
}

// SaveAISettings saves both pipeline AI switches. The caller must hold the
// settings part.
func (i *Integration) SaveAISettings(ctx context.Context, settings pipelines.AISettings) error {
	return i.saveSetting(ctx, pipelines.AISettingsVariable, aiSettingsDescription, settings.Encode())
}

// SaveSecuritySettings saves the weekly security scans switch. The caller must
// hold the settings part.
func (i *Integration) SaveSecuritySettings(ctx context.Context, settings pipelines.SecuritySettings) error {
	return i.saveSetting(ctx, pipelines.SecuritySettingsVariable, securitySettingsDescription, settings.Encode())
}

func (i *Integration) handleAISettingsSave(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = handlerContext(ctx)
	review, err := switchArg(args, "review")
	if err != nil {
		return nil, err
	}
	diagnosis, err := switchArg(args, "diagnosis")
	if err != nil {
		return nil, err
	}
	settings := pipelines.AISettings{Review: review, Diagnosis: diagnosis}
	if err := i.SaveAISettings(ctx, settings); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"setting": "ai", "review": settings.Review, "diagnosis": settings.Diagnosis}), nil
}

func (i *Integration) handleSecuritySettingsSave(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = handlerContext(ctx)
	enabled, err := switchArg(args, "enabled")
	if err != nil {
		return nil, err
	}
	settings := pipelines.SecuritySettings{Enabled: enabled}
	if err := i.SaveSecuritySettings(ctx, settings); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"setting": "security", "enabled": settings.Enabled}), nil
}
