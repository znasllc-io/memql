package memql

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// Generated from the OS's real manifests; navigationContract.test.ts refuses
// stale destinations. Agents and the window chrome consume one contract.
//
//go:embed os_navigation.json
var osNavigationJSON []byte

type navigationSection struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Requires string `json:"requires,omitempty"`
}
type navigationRecord struct {
	Section string   `json:"section"`
	IDField string   `json:"idField"`
	Query   string   `json:"query"`
	Labels  []string `json:"labels"`
}
type navigationApp struct {
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Requires string              `json:"requires"`
	Sections []navigationSection `json:"sections"`
	Records  []navigationRecord  `json:"records"`
}

func navigationApps() []navigationApp {
	var apps []navigationApp
	if err := json.Unmarshal(osNavigationJSON, &apps); err != nil {
		panic("invalid shipped OS navigation catalog: " + err.Error())
	}
	return apps
}
func admittedNavigation(ctx context.Context) []navigationApp {
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok {
		return nil
	}
	apps := []navigationApp{}
	for _, app := range navigationApps() {
		if !auth.CapableFor(ctx, subject, "read", app.Requires) {
			continue
		}
		sections := []navigationSection{}
		for _, section := range app.Sections {
			if section.Requires == "" || auth.CapableFor(ctx, subject, "read", section.Requires) {
				sections = append(sections, section)
			}
		}
		app.Sections = sections
		records := []navigationRecord{}
		for _, record := range app.Records {
			for _, section := range sections {
				if record.Section == section.ID {
					records = append(records, record)
					break
				}
			}
		}
		app.Records = records
		apps = append(apps, app)
	}
	return apps
}
func navigationResult(value any) ([]memorynodes.MemoryNode, error) {
	if result, ok := value.(map[string]any); ok {
		if requested, _ := result["requested"].(bool); requested {
			result["reply"] = "Opening " + fmt.Sprint(result["label"]) + "."
		} else if reason, ok := result["reason"].(string); ok {
			reply := reason
			if choices, ok := result["choices"].([]navigationMatch); ok {
				for _, choice := range choices {
					reply += "\n- " + choice.Label
				}
			}
			result["reply"] = reply
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "navigation", Payload: raw}}, nil
}

// Navigation resolves a real section and, when requested, a record through
// the SAME authorized named read as the UI. It never creates missing data.
func (e *MemQLEngine) workNavigateBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	appName, sectionName, recordName := stringArg(args, "app"), stringArg(args, "section"), stringArg(args, "record")
	if len(appName) > 100 || len(sectionName) > 100 || len(recordName) > 300 {
		return nil, fmt.Errorf("navigation target is too long")
	}
	apps := admittedNavigation(ctx)
	if appName == "" {
		return navigationResult(map[string]any{"destinations": apps})
	}
	var app *navigationApp
	for index := range apps {
		if strings.EqualFold(apps[index].ID, appName) || strings.EqualFold(apps[index].Name, appName) {
			app = &apps[index]
			break
		}
	}
	if app == nil {
		return navigationResult(map[string]any{"requested": false, "reason": "Choose an available app.", "destinations": apps})
	}
	if sectionName == "" && recordName != "" && len(app.Records) == 1 {
		sectionName = app.Records[0].Section
	}
	if sectionName == "" && len(app.Sections) > 0 {
		sectionName = app.Sections[0].ID
	}
	var section *navigationSection
	for index := range app.Sections {
		if strings.EqualFold(app.Sections[index].ID, sectionName) || strings.EqualFold(app.Sections[index].Name, sectionName) {
			section = &app.Sections[index]
			break
		}
	}
	if section == nil {
		return navigationResult(map[string]any{"requested": false, "reason": "Choose an available section.", "sections": app.Sections})
	}
	arguments := map[string]any{"section": section.ID}
	label := app.Name + " / " + section.Name
	if recordName != "" {
		var contract *navigationRecord
		for index := range app.Records {
			if app.Records[index].Section == section.ID {
				contract = &app.Records[index]
				break
			}
		}
		if contract == nil {
			return navigationResult(map[string]any{"requested": false, "reason": "This section has no registered record destination.", "sections": app.Sections})
		}
		fn, err := e.functions.Get(contract.Query)
		if err != nil || fn.FunctionKind != "query" || !e.workCapabilityAllowed(ctx, fn) {
			return nil, fmt.Errorf("the destination's read is unavailable")
		}
		var rows []map[string]any
		cursor := ""
		// Bounded, cursor-aware discovery; never silently claim a partial scan is
		// the whole collection. Large catalogs need a narrower registered read.
		for page := 0; ; page++ {
			result, err := e.Execute(ContextWithCursor(ctx, cursor), "query "+contract.Query+"()")
			if err != nil {
				return nil, err
			}
			rows = append(rows, MaterializeRows(result.OutputPayload())...)
			next := ""
			if result.Meta != nil {
				next = result.Meta.Cursor
			}
			if next == "" {
				break
			}
			if next == cursor || page >= 19 || len(rows) > 10000 {
				return nil, fmt.Errorf("destination lookup needs a narrower search; no navigation was performed")
			}
			cursor = next
		}
		matches := matchNavigationRecords(rows, contract.Labels, recordName)
		if len(matches) == 0 {
			return navigationResult(map[string]any{"requested": false, "reason": "No matching record is visible to this person. Do not create one for a navigation request."})
		}
		if len(matches) > 1 {
			return navigationResult(map[string]any{"requested": false, "reason": "More than one record matches; ask the person to choose.", "choices": matches})
		}
		arguments[contract.IDField] = matches[0].ID
		label = matches[0].Label
	}
	run, ok := common.RunFromContext(ctx)
	if !ok || run.RunId == "" {
		return nil, fmt.Errorf("navigation requires a work run")
	}
	event := WorkEvent{ID: id.NewShortId(), Kind: "action", Phase: "completed", Name: "Open " + label, App: app.ID, Navigate: true, Arguments: arguments}
	if err := e.RecordWorkProgress(ctx, event); err != nil {
		return nil, err
	}
	return navigationResult(map[string]any{"requested": true, "app": app.ID, "section": section.ID, "label": label, "arguments": arguments, "note": "Destination resolved and requested in the connected OS. This is not a data change or a browser display receipt."})
}

type navigationMatch struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	score float64
}

func navigationWords(value string) []string {
	words := strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, value))
	out := []string{}
	for _, word := range words {
		switch word {
		case "and", "the", "for", "a", "an", "of":
			continue
		}
		out = append(out, word)
	}
	return out
}
func matchNavigationRecords(rows []map[string]any, labels []string, search string) []navigationMatch {
	words := navigationWords(search)
	if len(words) == 0 {
		return nil
	}
	matches := []navigationMatch{}
	for _, row := range rows {
		rowID, _ := row["id"].(string)
		rowID = BareShortId(rowID)
		if rowID == "" {
			continue
		}
		label := ""
		haystack := ""
		for _, key := range labels {
			if value, ok := row[key].(string); ok {
				if label == "" {
					label = value
				}
				haystack += " " + value
			}
		}
		score := 0.0
		if strings.EqualFold(strings.TrimSpace(search), rowID) || strings.EqualFold(strings.TrimSpace(search), label) {
			score = 2
		} else {
			target := navigationWords(haystack)
			hits := 0
			for _, word := range words {
				found := false
				for i, part := range target {
					if word == part || (len(word) > 2 && strings.HasPrefix(part, word)) {
						found = true
						break
					}
					if i+1 < len(target) && len(word) == 2 && word == string([]rune(part)[0])+string([]rune(target[i+1])[0]) {
						found = true
						break
					}
				}
				if found {
					hits++
				}
			}
			score = float64(hits) / float64(len(words))
		}
		if score >= 0.65 {
			matches = append(matches, navigationMatch{ID: rowID, Label: label, score: score})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].ID < matches[j].ID
	})
	if len(matches) > 1 && matches[0].score-matches[1].score >= 0.2 {
		matches = matches[:1]
	}
	if len(matches) > 8 {
		matches = matches[:8]
	}
	return matches
}
