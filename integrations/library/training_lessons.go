package library

// Manual lesson revisions and decisions are immutable, owned Work observations.
// Library exposes their review; the existing Training reader supplies enabled
// passages. No skill catalog, evaluator, scheduler or automatic miner is added.
import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work/goaleval"
	"github.com/znasllc-io/memql/core/common"
)

type lessonDraft struct {
	Title            string `json:"title"`
	Text             string `json:"text"`
	Scope            string `json:"scope"`
	Applies          string `json:"applies"`
	Exceptions       string `json:"exceptions"`
	Observed         string `json:"observed"`
	Alternative      string `json:"alternative"`
	Principle        string `json:"principle"`
	FactConfidence   string `json:"factConfidence"`
	LessonConfidence string `json:"lessonConfidence"`
	Scenario         string `json:"scenario"`
	Counterexample   string `json:"counterexample"`
}
type trainingLesson struct {
	SchemaVersion int            `json:"schemaVersion"`
	ID            string         `json:"id"`
	Receipt       string         `json:"receipt"`
	Sequence      int            `json:"sequence"`
	Version       int            `json:"version"`
	Digest        string         `json:"digest"`
	RequestID     string         `json:"requestId"`
	RequestDigest string         `json:"requestDigest"`
	Action        string         `json:"action"`
	Draft         lessonDraft    `json:"draft"`
	Source        map[string]any `json:"source"`
	Review        string         `json:"review"`
	Enabled       bool           `json:"enabled"`
	EvaluationIDs []string       `json:"evaluationIds"`
	Reason        string         `json:"reason"`
	At            string         `json:"at"`
}
type lessonEngine interface {
	TrainingLessonSource(context.Context, string, string) (map[string]any, error)
	AppendTrainingLessonEvent(context.Context, map[string]any) error
}

func lessonPerson(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.UserId == "" || ac.Synthetic || ac.Unranked {
		return "", fmt.Errorf("lesson review requires a signed-in person")
	}
	if rc, ok := common.RunFromContext(ctx); ok && rc.RunId != "" {
		return "", fmt.Errorf("a work run cannot propose or approve its own lesson")
	}
	sub, ok := auth.SubjectFromContext(ctx)
	if !ok || !auth.CapableFor(ctx, sub, auth.VerbCreate, auth.ResourceData) {
		return "", fmt.Errorf("you cannot write lessons")
	}
	return memql.BareShortId(ac.UserId), nil
}
func (i *Integration) lessonRows(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	call, err := parser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	res, err := i.engine.Execute(memql.ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}
func lessonFromRow(row map[string]any) (trainingLesson, error) {
	var l trainingLesson
	data, _ := row["data"].(map[string]any)
	raw, err := json.Marshal(data["trainingLesson"])
	if err == nil {
		err = json.Unmarshal(raw, &l)
	}
	if err == nil && (l.SchemaVersion != 1 || l.ID == "" || l.Sequence < 1) {
		err = fmt.Errorf("lesson receipt is incomplete")
	}
	return l, err
}
func (i *Integration) lessonEvents(ctx context.Context, lessonID string) ([]trainingLesson, error) {
	rows, err := i.lessonRows(ctx, "workTrainingLessonEvents", map[string]any{"lessonId": lessonID})
	if err != nil {
		return nil, err
	}
	if len(rows) > 100 {
		return nil, fmt.Errorf("lesson revision limit reached; preserve this history and propose a separately scoped successor")
	}
	out := make([]trainingLesson, 0, len(rows))
	for _, row := range rows {
		l, err := lessonFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Sequence > out[b].Sequence })
	return out, nil
}
func decodeLessonDraft(value any) (lessonDraft, error) {
	var d lessonDraft
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 16000 {
		return d, fmt.Errorf("lesson draft exceeds 16000 bytes")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&d); err != nil {
		return d, err
	}
	for _, v := range []string{d.Title, d.Text, d.Applies, d.Exceptions, d.Observed, d.Alternative, d.Principle, d.FactConfidence, d.LessonConfidence, d.Scenario, d.Counterexample} {
		if strings.TrimSpace(v) == "" {
			return d, fmt.Errorf("complete the lesson, evidence, confidence and both validation scenarios")
		}
	}
	if len(d.Title) > 160 || len(d.Text) > 6000 || d.Scope != "owner" || d.Scenario == d.Counterexample {
		return d, fmt.Errorf("manual lessons require owner scope and distinct application and counterexample scenarios")
	}
	return d, nil
}
func lessonDigest(d lessonDraft) string {
	raw, _ := json.Marshal(d)
	return trainingDigest(string(raw))
}
func lessonPassage(l trainingLesson) string {
	return l.Draft.Text + "\nApplies when: " + l.Draft.Applies + "\nExceptions: " + l.Draft.Exceptions
}

func (i *Integration) handleSaveTrainingLesson(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := lessonPerson(ctx)
	if err != nil {
		return nil, err
	}
	engine, ok := i.engine.(lessonEngine)
	if !ok {
		return nil, fmt.Errorf("lesson persistence is unavailable")
	}
	action, requestID := asString(args["action"]), asString(args["requestId"])
	if !reviewRequestID.MatchString(requestID) {
		return nil, fmt.Errorf("lesson action needs a stable request id")
	}
	raw, _ := json.Marshal(args)
	if len(raw) > 24000 {
		return nil, fmt.Errorf("lesson request exceeds its limit")
	}
	requestDigest := trainingDigest(string(raw))
	lessonID := asString(args["lessonId"])
	var source map[string]any
	if action == "propose" {
		read := engine.TrainingLessonSource
		if i.lessonSource != nil {
			read = i.lessonSource
		}
		source, err = read(ctx, asString(args["conversationId"]), asString(args["turnId"]))
		if err != nil {
			return nil, err
		}
		// One candidate per saved feedback message. Rewording cannot silently
		// create a second candidate that evades the first rejection.
		lessonID = "lesson-" + trainingDigest(owner+":"+asString(source["conversationId"])+":"+asString(source["turnId"]))
	}
	if lessonID == "" || len(lessonID) > 100 {
		return nil, fmt.Errorf("lesson identity is required")
	}
	release, err := i.versionGate(ctx, "training:"+owner+":"+lessonID)
	if err != nil {
		return nil, err
	}
	defer release()
	events, err := i.lessonEvents(ctx, lessonID)
	if err != nil {
		return nil, err
	}
	for _, old := range events {
		if old.RequestID == requestID {
			if old.RequestDigest != requestDigest {
				return nil, fmt.Errorf("request id already belongs to different lesson input")
			}
			return lessonResult(old)
		}
	}
	if len(events) >= 100 || (len(events) >= 99 && action != "retire") {
		return nil, fmt.Errorf("lesson revision limit reached; retire this candidate before proposing a successor")
	}
	var next trainingLesson
	if action == "propose" {
		if len(events) > 0 {
			return nil, fmt.Errorf("this feedback already has a lesson; open its existing review")
		}
		runs, err := i.lessonRows(ctx, "workRunForOwner", map[string]any{"runId": asString(source["runId"])})
		if err != nil {
			return nil, err
		}
		if len(runs) != 1 || memql.BareShortId(asString(runs[0]["ownerUserId"])) != owner {
			return nil, fmt.Errorf("lesson source work belongs to another owner")
		}
		next = trainingLesson{SchemaVersion: 1, ID: lessonID, Source: source, Version: 1, Review: "candidate", EvaluationIDs: []string{}}
		next.Draft, err = decodeLessonDraft(args["draft"])
		if err != nil {
			return nil, err
		}
	} else {
		if len(events) == 0 {
			return nil, fmt.Errorf("lesson is unavailable")
		}
		next = events[0]
		if next.Receipt != asString(args["expectedReceipt"]) {
			return nil, fmt.Errorf("lesson changed; read the current revision before deciding")
		}
		switch action {
		case "edit":
			next.Draft, err = decodeLessonDraft(args["draft"])
			if err != nil {
				return nil, err
			}
			next.Version++
			next.Review = "candidate"
			next.Enabled = false
			next.EvaluationIDs = []string{}
		case "review":
			if next.Review != "candidate" {
				return nil, fmt.Errorf("only a candidate can be reviewed")
			}
			next.Review = "reviewed"
		case "reject":
			next.Review = "rejected"
			next.Enabled = false
		case "retire":
			next.Enabled = false
			next.Review = "retired"
		case "enable":
			if next.Review != "reviewed" {
				return nil, fmt.Errorf("review this exact lesson before enabling it")
			}
			ids := stringSliceField(args, "evaluationIds")
			if err = i.verifyLessonEvaluations(ctx, next, ids); err != nil {
				return nil, err
			}
			next.EvaluationIDs = ids
			next.Enabled = true
		default:
			return nil, fmt.Errorf("unknown lesson action")
		}
	}
	next.Sequence++
	next.Digest = lessonDigest(next.Draft)
	next.RequestID = requestID
	next.RequestDigest = requestDigest
	next.Action = action
	next.Reason = asString(args["reason"])
	if action != "propose" && action != "edit" && strings.TrimSpace(next.Reason) == "" {
		return nil, fmt.Errorf("record a reason for this decision")
	}
	next.At = time.Now().UTC().Format(time.RFC3339Nano)
	next.Receipt = "lesson-event-" + trainingDigest(owner+":"+lessonID+":"+requestID)
	err = engine.AppendTrainingLessonEvent(ctx, map[string]any{"observationId": next.Receipt, "runId": asString(next.Source["runId"]), "kind": "decision", "content": "Manual Training lesson: " + next.Action + " (" + next.Draft.Title + ")", "data": map[string]any{"trainingLesson": next}})
	if err != nil {
		return nil, err
	}
	return lessonResult(next)
}

func (i *Integration) verifyLessonEvaluations(ctx context.Context, l trainingLesson, ids []string) error {
	if len(ids) != 2 || ids[0] == ids[1] {
		return fmt.Errorf("enable requires two distinct saved evaluations: application and counterexample")
	}
	seen := map[string]bool{}
	for _, ref := range ids {
		rows, err := i.lessonRows(ctx, "workTrainingEvidenceById", map[string]any{"observationId": ref})
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return fmt.Errorf("evaluation evidence is unavailable")
		}
		data, _ := rows[0]["data"].(map[string]any)
		if data["oracleAuthority"] != "external-harness" {
			return fmt.Errorf("use a result published through Training's evaluator")
		}
		raw, _ := json.Marshal(data["goalEvaluation"])
		r, err := goaleval.Decode[goaleval.Result](raw)
		if err != nil {
			return err
		}
		sr, _ := json.Marshal(data["scenario"])
		s, err := goaleval.Decode[goaleval.Scenario](sr)
		if err != nil {
			return err
		}
		if err = r.Validate(s); err != nil {
			return err
		}
		if r.Outcome != "autonomous" && r.Outcome != "assisted" {
			return fmt.Errorf("evaluation did not establish success")
		}
		bound := false
		for _, b := range r.Bindings {
			if b.ID == l.ID && b.Digest == l.Digest && b.Version == strconv.Itoa(l.Version) {
				bound = true
			}
		}
		if !bound {
			return fmt.Errorf("evaluation did not test this exact lesson revision")
		}
		seen[r.ScenarioID] = true
	}
	if !seen[l.Draft.Scenario] || !seen[l.Draft.Counterexample] {
		return fmt.Errorf("evaluations do not cover the declared application and counterexample")
	}
	return nil
}

func (i *Integration) handleIndexTrainingLesson(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := lessonPerson(ctx)
	if err != nil {
		return nil, err
	}
	release, err := i.versionGate(ctx, "training:"+owner+":"+asString(args["lessonId"]))
	if err != nil {
		return nil, err
	}
	defer release()
	events, err := i.lessonEvents(ctx, asString(args["lessonId"]))
	if err != nil {
		return nil, err
	}
	if len(events) == 0 || events[0].Receipt != asString(args["expectedReceipt"]) || !events[0].Enabled || events[0].Review != "reviewed" {
		return nil, fmt.Errorf("only the current enabled lesson can be indexed")
	}
	if i.training == nil {
		return nil, fmt.Errorf("Training index is unavailable")
	}
	l := events[0]
	// This is one index-write effect. Choice to enable/index stays with the caller.
	for _, cap := range i.training.embed.Capabilities() {
		if cap.Name == "store" {
			_, err = cap.Handler(ctx, map[string]any{"nodeId": "v1:work:observation:" + l.Receipt, "concept": "v1:work:observation", "vectorField": "content", "text": lessonPassage(l)}, 0)
			if err != nil {
				return nil, err
			}
			return reviewResult(map[string]any{"indexed": true, "receipt": l.Receipt})
		}
	}
	return nil, fmt.Errorf("Training index writer is unavailable")
}

func lessonResult(l trainingLesson) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return reviewResult(value)
}

// A candidate can be supplied only when the person-operated evaluator explicitly
// pins it in this goal's input. This does not change normal retrieval eligibility.
func (i *Integration) evaluationLesson(ctx context.Context, run common.RunContext, value any) (map[string]any, error) {
	binding, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid evaluation lesson binding")
	}
	events, err := i.lessonEvents(ctx, asString(binding["lessonId"]))
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("evaluation lesson is unavailable")
	}
	l := events[0]
	if l.Receipt != asString(binding["receipt"]) || (l.Review != "candidate" && l.Review != "reviewed") {
		return nil, fmt.Errorf("evaluation lesson revision changed or was withdrawn")
	}
	scenario := asString(binding["scenarioId"])
	if (scenario != l.Draft.Scenario && scenario != l.Draft.Counterexample) || asString(binding["fixtureVersion"]) == "" {
		return nil, fmt.Errorf("evaluation must name a declared lesson scenario and fixture version")
	}
	rows, err := i.lessonRows(ctx, "workRunForOwner", map[string]any{"runId": run.RunId})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("evaluation work is unavailable")
	}
	routing, _ := rows[0]["routing"].(map[string]any)
	if routing["noPaidProviders"] != true || !strings.HasPrefix(asString(routing["source"]), "fleet:") {
		return nil, fmt.Errorf("lesson evaluations require the existing fleet-only evaluation route")
	}
	return map[string]any{"chunkId": l.Receipt, "source": "manual-coaching-evaluation", "sourceRef": "lesson:" + l.ID, "lessonId": l.ID, "lessonVersion": l.Version, "lessonDigest": l.Digest, "citation": l.Draft.Title, "text": lessonPassage(l)}, nil
}
