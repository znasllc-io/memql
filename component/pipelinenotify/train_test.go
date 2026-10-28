package pipelinenotify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const trainMentionID = "123456789012345678"

func trainMessage(event TrainEvent) TrainMessage {
	return TrainMessage{
		Event:          event,
		Repository:     "znasllc-io/memql",
		Branch:         "train/2026-10-28-1",
		Version:        "0.31.5",
		PullRequestURL: "https://github.com/znasllc-io/memql/pull/6400",
		RunPageURL:     "https://os.example.test/?pipelineRun=abc",
		OSOrigin:       "https://os.example.test",
		OnBoard:        []TrainItem{{Number: 6390, Title: "Fix the badge", URL: "https://github.com/znasllc-io/memql/pull/6390"}},
		MentionUserID:  trainMentionID,
		At:             time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC),
	}
}

func TestTrainAwaitingApprovalMessage(t *testing.T) {
	got, err := TrainDiscordMessage(trainMessage(TrainAwaitingApproval))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"username":"MemQL Pipelines","content":"<@123456789012345678> ACTION NEEDED: approve train/2026-10-28-1",` +
		`"allowed_mentions":{"parse":[],"users":["123456789012345678"]},"embeds":[{` +
		`"title":"znasllc-io/memql · ACTION NEEDED: approve train/2026-10-28-1",` +
		`"url":"https://github.com/znasllc-io/memql/pull/6400",` +
		`"description":"The full run passed. Approve the train pull request on GitHub to land it.",` +
		`"color":15105570,"fields":[` +
		`{"name":"Train","value":"[train/2026-10-28-1](https://github.com/znasllc-io/memql/pull/6400)"},` +
		`{"name":"Version","value":"0.31.5"},` +
		`{"name":"Run","value":"[Open the run](https://os.example.test/?pipelineRun=abc)"},` +
		`{"name":"What you need to do","value":"Review the train pull request on GitHub and approve it. It lands within a minute of your approval."},` +
		`{"name":"On board","value":"[#6390](https://github.com/znasllc-io/memql/pull/6390) Fix the badge"},` +
		`{"name":"MemQL OS","value":"[Open](https://os.example.test/)"}],` +
		`"timestamp":"2026-10-28T17:00:00Z"}]}`
	if string(got) != want {
		t.Errorf("message:\n%s\nwant:\n%s", got, want)
	}
}

func TestTrainDepartedMessage(t *testing.T) {
	m := trainMessage(TrainDeparted)
	m.LeftOff = []TrainItem{{Number: 6391, Reason: "is a draft"}}
	m.RunPageURL = ""
	got, err := TrainDiscordMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"username":"MemQL Pipelines","allowed_mentions":{"parse":[]},"embeds":[{` +
		`"title":"znasllc-io/memql · Train train/2026-10-28-1 left with 1 pull request",` +
		`"url":"https://github.com/znasllc-io/memql/pull/6400",` +
		`"description":"1 pull request on board, 1 left off. The full run starts now.",` +
		`"color":3447003,"fields":[` +
		`{"name":"Train","value":"[train/2026-10-28-1](https://github.com/znasllc-io/memql/pull/6400)"},` +
		`{"name":"Version","value":"0.31.5"},` +
		`{"name":"On board","value":"[#6390](https://github.com/znasllc-io/memql/pull/6390) Fix the badge"},` +
		`{"name":"Left off","value":"#6391 is a draft"},` +
		`{"name":"MemQL OS","value":"[Open](https://os.example.test/)"}],` +
		`"timestamp":"2026-10-28T17:00:00Z"}]}`
	if string(got) != want {
		t.Errorf("message:\n%s\nwant:\n%s", got, want)
	}
}

// decodedTrain is the wire shape the remaining events are checked through.
type decodedTrain struct {
	Username        string `json:"username"`
	Content         string `json:"content"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
		Users []string `json:"users"`
	} `json:"allowed_mentions"`
	Embeds []struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Description string `json:"description"`
		Color       int    `json:"color"`
		Timestamp   string `json:"timestamp"`
		Fields      []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"fields"`
	} `json:"embeds"`
}

func decodeTrain(t *testing.T, m TrainMessage) decodedTrain {
	t.Helper()
	raw, err := TrainDiscordMessage(m)
	if err != nil {
		t.Fatalf("TrainDiscordMessage(%s): %v", m.Event, err)
	}
	var d decodedTrain
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if d.AllowedMentions.Parse == nil || len(d.AllowedMentions.Parse) != 0 {
		t.Errorf("%s: allowed_mentions.parse must be present and empty", m.Event)
	}
	return d
}

func trainFieldNames(d decodedTrain) string {
	var names []string
	for _, f := range d.Embeds[0].Fields {
		names = append(names, f.Name)
	}
	return strings.Join(names, ",")
}

func TestTrainEventMessages(t *testing.T) {
	cases := []struct {
		event   TrainEvent
		edit    func(*TrainMessage)
		title   string
		action  bool
		color   int
		hasName string
	}{
		{TrainDropped, func(m *TrainMessage) {
			m.Dropped = &TrainItem{Number: 6390, Title: "Fix the badge", URL: "https://github.com/znasllc-io/memql/pull/6390", Reason: "tests.go-unit#2 failed in its packages"}
		}, "znasllc-io/memql · Dropped #6390 from train/2026-10-28-1", false, 15158332, "Dropped"},
		{TrainHeld, func(m *TrainMessage) {
			m.Reason, m.Failed = "main fails tests.go-unit#2 too", []string{"tests.go-unit#2"}
		}, "znasllc-io/memql · ACTION NEEDED: train/2026-10-28-1 is held", true, 15105570, "Failed"},
		{TrainReminder, nil, "znasllc-io/memql · ACTION NEEDED: train/2026-10-28-1 is still waiting for your approval", true, 15105570, "What you need to do"},
		{TrainLanded, func(m *TrainMessage) { m.Reason = "Landed on main as 9f8e7d6." }, "znasllc-io/memql · PASSED: train/2026-10-28-1 landed on main", false, 3066993, "On board"},
		{TrainAdmissionHeld, nil, "znasllc-io/memql · ACTION NEEDED: the cost guard is holding train/2026-10-28-1's run", true, 15105570, "What you need to do"},
	}
	for _, c := range cases {
		t.Run(string(c.event), func(t *testing.T) {
			m := trainMessage(c.event)
			if c.edit != nil {
				c.edit(&m)
			}
			d := decodeTrain(t, m)
			if d.Embeds[0].Title != c.title {
				t.Errorf("title %q, want %q", d.Embeds[0].Title, c.title)
			}
			if d.Embeds[0].Color != c.color {
				t.Errorf("color %d, want %d", d.Embeds[0].Color, c.color)
			}
			if c.action {
				if !strings.HasPrefix(d.Content, "<@"+trainMentionID+"> ACTION NEEDED: ") || len(d.AllowedMentions.Users) != 1 || d.AllowedMentions.Users[0] != trainMentionID {
					t.Errorf("an action message must mention exactly the owner: content %q users %v", d.Content, d.AllowedMentions.Users)
				}
			} else if d.Content != "" || len(d.AllowedMentions.Users) != 0 {
				t.Errorf("a status message mentions no one: content %q users %v", d.Content, d.AllowedMentions.Users)
			}
			if !strings.Contains(trainFieldNames(d), c.hasName) {
				t.Errorf("fields %s lack %q", trainFieldNames(d), c.hasName)
			}
			if d.Embeds[0].Description == "" {
				t.Errorf("no description")
			}
		})
	}
}

func TestTrainMessageNeverMentionsFromText(t *testing.T) {
	m := trainMessage(TrainAwaitingApproval)
	m.OnBoard = []TrainItem{{Number: 1, Title: "@everyone <@1> **x** [click](https://evil.example)"}}
	d := decodeTrain(t, m)
	var onBoard string
	for _, f := range d.Embeds[0].Fields {
		if f.Name == "On board" {
			onBoard = f.Value
		}
	}
	if strings.Contains(onBoard, "<@1>") || strings.Contains(onBoard, "**x**") || strings.Contains(onBoard, "[click](") {
		t.Errorf("a pull request title reached the message unescaped: %q", onBoard)
	}
	if strings.Join(d.AllowedMentions.Users, ",") != trainMentionID {
		t.Errorf("users %v, want only the configured owner", d.AllowedMentions.Users)
	}
	noMention := trainMessage(TrainAwaitingApproval)
	noMention.MentionUserID = ""
	if d := decodeTrain(t, noMention); d.Content != "" || len(d.AllowedMentions.Users) != 0 {
		t.Errorf("no configured owner: content %q users %v; want neither", d.Content, d.AllowedMentions.Users)
	}
}

func TestTrainMessageRefusesABadMentionID(t *testing.T) {
	m := trainMessage(TrainAwaitingApproval)
	m.MentionUserID = "abc"
	if _, err := TrainDiscordMessage(m); err == nil {
		t.Error("a mention id that is not a Discord user id was accepted")
	}
	m = trainMessage("bogus")
	if _, err := TrainDiscordMessage(m); err == nil {
		t.Error("an unknown train event was accepted")
	}
}
