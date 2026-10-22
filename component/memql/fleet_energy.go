package memql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/costestimate"
	"github.com/znasllc-io/memql/core/id"
)

const energyProfileConcept = "v1:worker:energyProfile"

type energySettings struct {
	Address       string   `json:"address"`
	City          string   `json:"city"`
	Region        string   `json:"region"`
	PostalCode    string   `json:"postalCode"`
	Country       string   `json:"country"`
	Timezone      string   `json:"timezone"`
	Verified      bool     `json:"verified"`
	LookupAllowed bool     `json:"lookupAllowed"`
	Provider      string   `json:"provider"`
	Watts         *float64 `json:"watts"`
	EnergySource  string   `json:"energySource"`
}
type utilityCandidate struct {
	Name      string `json:"name"`
	SourceURL string `json:"sourceUrl"`
	Evidence  string `json:"evidence"`
}
type electricityResearch struct {
	Status     string                `json:"status"`
	Candidates []utilityCandidate    `json:"candidates"`
	Tariffs    []costestimate.Tariff `json:"tariffs"`
	Notes      string                `json:"notes"`
	RunID      string                `json:"runId"`
}

func energyProfileID(owner, registration string) string {
	h := sha256.Sum256([]byte(owner + ":" + trimConceptPrefix(registration)))
	return "energy-" + hex.EncodeToString(h[:16])
}
func decodeEnergy(value any, target any) error {
	b, err := json.Marshal(value)
	if err != nil || len(b) > 32768 {
		return fmt.Errorf("energy settings exceed the size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return fmt.Errorf("invalid energy settings")
	}
	return nil
}

// Private settings have their own owner-scoped row; registrations are visible
// to operators and shared-fleet consumers. The address is additionally sealed
// and is never passed to the research goal, candidates, quotes or cost ledger.
func (e *MemQLEngine) energyProfile(ctx context.Context, registration string) (map[string]any, string, error) {
	machine, err := e.modelSetupRow(ctx, registration)
	if err != nil {
		return nil, "", err
	}
	if mapString(machine, "revokedAt") != "" {
		return nil, "", fmt.Errorf("machine was removed")
	}
	pid := energyProfileID(actingUserFromContext(ctx), registration)
	call, _ := parser.RenderCall("workerEnergyProfile", map[string]any{"profileId": pid})
	res, err := e.Execute(ContextWithFreshRead(ctx), call)
	if err != nil {
		return nil, "", err
	}
	rows := MaterializeRows(res)
	if len(rows) == 0 {
		return map[string]any{}, pid, nil
	}
	return rows[0], pid, nil
}
func (e *MemQLEngine) writeEnergy(ctx context.Context, registration, pid string, settings map[string]any, encrypted string, research any, tariff any, revision, locationRevision string) error {
	call, err := parser.RenderCall("storeWorkerEnergyProfile", map[string]any{"profileId": pid, "registrationId": registration, "settings": settings, "encryptedAddress": encrypted, "research": research, "tariff": tariff, "revision": revision, "locationRevision": locationRevision})
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), call)
	return err
}
func energyMap(value any) map[string]any {
	b, _ := json.Marshal(value)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}
func safeEnergyObject(value any) any {
	if value == nil {
		return map[string]any{}
	}
	return value
}
func privateEnergyAccess(ctx context.Context) bool {
	// App-session and delegated tool credentials may submit research, but cannot
	// read street addresses or attest that a person verified them.
	if _, ok := auth.DelegationFromContext(ctx); ok {
		return false
	}
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return false
	}
	// Human JWTs deliberately omit class. Forwarded claims carry attribution,
	// not authority; use the resolved post-rotation/cross-node credential stamp.
	if class, _ := claims["class"].(string); class != "" && class != "user" && class != "pat" {
		return false
	}
	grant, err := auth.CaptureExecutionAuthority(ctx)
	if err != nil {
		return false
	}
	class, _ := grant["credentialClass"].(string)
	return class == "user" || class == "pat"
}
func (e *MemQLEngine) evaluateFleetEnergySettings(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	registration := stringArg(args, "registrationId")
	action := stringArg(args, "action")
	if !privateEnergyAccess(ctx) {
		return nil, fmt.Errorf("machine energy settings require the owner's interactive session or personal credential")
	}
	if _, _, err := e.energyProfile(ctx, registration); err != nil {
		return nil, err
	}
	if action != "read" {
		var err error
		ctx, err = e.fenceWriteTarget(ctx, WorkerRegistrationConcept, registration)
		if err != nil {
			return nil, err
		}
	}
	row, pid, err := e.energyProfile(ctx, registration)
	if err != nil {
		return nil, err
	}
	settings, _ := row["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
	}
	if action == "read" {
		address := ""
		if encrypted := mapString(row, "encryptedAddress"); encrypted != "" {
			address, err = secret.Decrypt(encrypted)
			if err != nil {
				return nil, fmt.Errorf("the private address could not be read")
			}
		}
		out := energyMap(settings)
		out["address"] = address
		out["revision"] = mapString(row, "revision")
		out["locationRevision"] = mapString(row, "locationRevision")
		out["research"] = safeEnergyObject(row["research"])
		out["tariff"] = safeEnergyObject(row["tariff"])
		return singleVirtualRow("v1:worker:energySettingsResult", pid, out)
	}
	if mapString(row, "revision") != stringArg(args, "revision") {
		return nil, fmt.Errorf("energy settings changed; reload before saving")
	}
	encrypted := mapString(row, "encryptedAddress")
	locationRevision := mapString(row, "locationRevision")
	research, tariff := safeEnergyObject(row["research"]), safeEnergyObject(row["tariff"])
	switch action {
	case "save":
		var s energySettings
		if err = decodeEnergy(args["secretSettings"], &s); err != nil {
			return nil, err
		}
		s.Address, s.City, s.Region = strings.TrimSpace(s.Address), strings.TrimSpace(s.City), strings.TrimSpace(s.Region)
		s.PostalCode, s.Country = strings.TrimSpace(s.PostalCode), strings.ToUpper(strings.TrimSpace(s.Country))
		s.Provider, s.EnergySource = strings.TrimSpace(s.Provider), strings.TrimSpace(s.EnergySource)
		if len(s.Country) != 2 || s.Country[0] < 'A' || s.Country[0] > 'Z' || s.Country[1] < 'A' || s.Country[1] > 'Z' {
			return nil, fmt.Errorf("provide a two-letter country code")
		}
		if len(s.Address) > 500 || len(s.City) > 120 || len(s.Region) > 120 || len(s.PostalCode) > 32 || len(s.Country) != 2 || len(s.Provider) > 160 || len(s.EnergySource) > 500 {
			return nil, fmt.Errorf("provide bounded address fields and a two-letter country code")
		}
		if _, err = time.LoadLocation(s.Timezone); err != nil {
			return nil, fmt.Errorf("choose the machine location's IANA timezone")
		}
		if s.Verified && (strings.TrimSpace(s.Address) == "" || strings.TrimSpace(s.City) == "" || strings.TrimSpace(s.PostalCode) == "") {
			return nil, fmt.Errorf("address, city and postal code are required to verify the location")
		}
		if s.LookupAllowed && !s.Verified {
			return nil, fmt.Errorf("verify the machine's address before authorizing lookup")
		}
		if s.Watts != nil && (*s.Watts < 0 || *s.Watts > 1000000 || s.EnergySource == "") {
			return nil, fmt.Errorf("estimated watts require a nonnegative value and an explicit source")
		}
		priorAddress := ""
		if encrypted != "" {
			priorAddress, err = secret.Decrypt(encrypted)
			if err != nil {
				return nil, fmt.Errorf("the private address could not be read")
			}
		}
		addressChanged := s.Address != priorAddress
		next := energyMap(s)
		delete(next, "address")
		changed := addressChanged
		for _, k := range []string{"city", "region", "postalCode", "country", "timezone", "provider", "verified", "lookupAllowed"} {
			if fmt.Sprint(settings[k]) != fmt.Sprint(next[k]) {
				changed = true
			}
		}
		if s.Address != "" {
			encrypted, _, err = secret.Encrypt(s.Address)
			if err != nil {
				return nil, fmt.Errorf("the private address could not be sealed")
			}
		} else {
			encrypted = ""
		}
		if changed {
			locationRevision = id.NewShortId()
			research = map[string]any{}
			tariff = map[string]any{}
		}
		settings = next
	case "confirm":
		if settings["verified"] != true {
			return nil, fmt.Errorf("verify this machine's location first")
		}
		var report electricityResearch
		if err = decodeEnergy(research, &report); err != nil {
			return nil, err
		}
		selected := stringArg(args, "tariffName")
		provider := stringArg(args, "provider")
		var matched *costestimate.Tariff
		for _, t := range report.Tariffs {
			if t.Name == selected && t.Provider == provider {
				if matched != nil {
					return nil, fmt.Errorf("tariff selection is ambiguous")
				}
				t := t
				matched = &t
			}
		}
		if matched == nil {
			return nil, fmt.Errorf("select one of the researched tariffs")
		}
		if err = costestimate.ValidateTariff(*matched); err != nil {
			return nil, err
		}
		tariff = matched
	default:
		return nil, fmt.Errorf("unknown energy settings action")
	}
	revision := id.NewShortId()
	if err = e.writeEnergy(ctx, registration, pid, settings, encrypted, research, tariff, revision, locationRevision); err != nil {
		return nil, err
	}
	return singleVirtualRow("v1:worker:energySettingsResult", pid, map[string]any{"revision": revision, "locationRevision": locationRevision})
}

// Research can propose evidence through any authorized inference door. It cannot
// confirm the owner location or select a tariff. A stale location token refuses
// late research instead of applying a previous city's utility after a move.
func (e *MemQLEngine) evaluateFleetRecordElectricityResearch(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	registration := stringArg(args, "registrationId")
	if _, _, err := e.energyProfile(ctx, registration); err != nil {
		return nil, err
	}
	ctx, err := e.fenceWriteTarget(ctx, WorkerRegistrationConcept, registration)
	if err != nil {
		return nil, err
	}
	row, pid, err := e.energyProfile(ctx, registration)
	if err != nil {
		return nil, err
	}
	settings, _ := row["settings"].(map[string]any)
	if settings["verified"] != true || settings["lookupAllowed"] != true || mapString(row, "locationRevision") != stringArg(args, "locationRevision") {
		return nil, fmt.Errorf("location lookup is not authorized or its revision changed")
	}
	var report electricityResearch
	if err = decodeEnergy(args["research"], &report); err != nil {
		return nil, err
	}
	if report.Status != "found" && report.Status != "ambiguous" && report.Status != "unavailable" {
		return nil, fmt.Errorf("research must report found, ambiguous or unavailable")
	}
	if len(report.Candidates) > 8 || len(report.Tariffs) > 12 || len(report.Notes) > 2000 {
		return nil, fmt.Errorf("research exceeds bounded candidate limits")
	}
	for _, c := range report.Candidates {
		source, parseErr := url.Parse(c.SourceURL)
		if len(c.Name) > 160 || len(c.Evidence) > 2000 || c.Name == "" || c.Evidence == "" || parseErr != nil || source.Scheme != "https" || source.Hostname() == "" || source.User != nil {
			return nil, fmt.Errorf("utility candidates require name, source and applicability evidence")
		}
	}
	for _, t := range report.Tariffs {
		if err = costestimate.ValidateTariff(t); err != nil {
			return nil, err
		}
		if t.Timezone != mapString(settings, "timezone") {
			return nil, fmt.Errorf("tariff timezone differs from the verified machine location")
		}
	}
	revision := id.NewShortId()
	if err = e.writeEnergy(ctx, registration, pid, settings, mapString(row, "encryptedAddress"), report, safeEnergyObject(row["tariff"]), revision, mapString(row, "locationRevision")); err != nil {
		return nil, err
	}
	return singleVirtualRow("v1:worker:energyResearchResult", pid, map[string]any{"status": report.Status, "revision": revision, "confirmation": "owner-required"})
}

// EstimateLocalElectricity is called only by the trusted ledger writer with its
// authenticated call owner. Borrowing the owner actor permits the normal owned
// read; it grants no access to another machine's profile or precise address.
func (e *MemQLEngine) EstimateLocalElectricity(ctx context.Context, registration, caller string, seconds float64) map[string]any {
	ctx = auth.ContextWithUserActor(ctx, caller)
	row, _, err := e.energyProfile(ctx, registration)
	if err != nil {
		return map[string]any{"kind": "electricity", "status": "unavailable", "reason": "No readable owner-confirmed electricity profile for this machine."}
	}
	settings, _ := row["settings"].(map[string]any)
	var s energySettings
	_ = decodeEnergy(settings, &s)
	var tariff costestimate.Tariff
	_ = decodeEnergy(row["tariff"], &tariff)
	var active *costestimate.Tariff
	if tariff.Provider != "" && s.Verified {
		active = &tariff
	}
	result := costestimate.Electricity(active, costestimate.Energy{Method: "estimated-watts", Watts: s.Watts, Seconds: seconds, Source: s.EnergySource, Allocation: "Whole-machine power estimate over the routed attempt wall duration, including dispatch/wait time; overlapping calls must not be summed as measured machine energy."}, time.Now())
	return energyMap(result)
}
