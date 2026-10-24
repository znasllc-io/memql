package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The graph half: what this machine is asked to back up, and what it reports
// back about the origin.
//
// ===========================================================================
// ONE TRANSPORT, TWO SURFACES, AND THEY ARE NOT INTERCHANGEABLE
// ===========================================================================
// Reads and writes of graph rows go through POST /memql/query, which forwards
// the caller's Authorization straight into gRPC metadata; BYTES go through the
// Library's own routes in library.go. Both are the bff's HTTP edge and both
// authenticate as the signed-in user, so there is one credential and one
// authorization story -- exactly the browser's.
//
// NOT THE WORKER TOKEN. `mql_wkr_` is pinned to WorkerService by the agent
// node's own interceptor and no HTTP middleware anywhere reads it; presenting
// one here is a 401 with no useful sentence. The cockpit is separately signed
// in, and that is the credential every call in this package carries.
//
// ===========================================================================
// A 200 IS NOT SUCCESS
// ===========================================================================
// The gateway answers HTTP 200 and puts a refusal in the `errors` array, so a
// client that only checked the status code would read "you may not see these
// rows" as "you are watching nothing" -- and a backup that reports nothing to
// do looks exactly like one that is up to date. Every call here reads `errors`
// before it reads `result`.

// GatewayPath is the engine's HTTP query surface. A constant because two
// places have to agree on it and a typo would 404 rather than fail loudly.
const GatewayPath = "/memql/query"

// Graph runs named constructs against one cluster as the signed-in user.
type Graph struct {
	baseURL string
	client  *http.Client
	bearer  func(context.Context) (string, error)
}

// NewGraph builds a client. `bearer` is called per request rather than held,
// so a token that rolls forward mid-sweep is picked up without restarting
// anything.
func NewGraph(baseURL string, client *http.Client, bearer func(context.Context) (string, error)) *Graph {
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	if client.Timeout <= 0 {
		bounded := *client
		bounded.Timeout = time.Minute
		client = &bounded
	}
	return &Graph{baseURL: strings.TrimRight(baseURL, "/"), client: client, bearer: bearer}
}

type gatewayResponse struct {
	Result struct {
		Data []map[string]any `json:"data"`
	} `json:"result"`
	Errors []struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"errors"`
}

// call sends one MemQL construct call and returns its rows.
func (g *Graph) call(ctx context.Context, call string) ([]map[string]any, error) {
	if g == nil {
		return nil, fmt.Errorf("backup: no graph client")
	}
	token, err := g.bearer(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"query": call})
	if err != nil {
		return nil, fmt.Errorf("backup: encode %s: %w", constructOf(call), err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+GatewayPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backup: %s: %w", constructOf(call), err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("backup: %s: read response: %w", constructOf(call), err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The one status worth naming, because the repair is specific and a
		// person can do it: this cluster's credential is not one the Library
		// surface accepts. A PAT is the common case -- PATs verify only on
		// the identity node, so the bff rejects one outright.
		return nil, fmt.Errorf("backup: %s: the cluster rejected this machine's credential (401). Run `memql login` here; a PAT alone cannot reach the Library surface", constructOf(call))
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("backup: %s: HTTP %d: %s", constructOf(call), resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var decoded gatewayResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("backup: %s: decode response: %w", constructOf(call), err)
	}
	if len(decoded.Errors) > 0 {
		// A REFUSAL ARRIVES AS A 200. Surfaced verbatim: the sentence names
		// the construct or the row, which is the part a paraphrase loses.
		return nil, fmt.Errorf("backup: %s refused: %s", constructOf(call), decoded.Errors[0].Message)
	}
	return decoded.Result.Data, nil
}

// constructOf pulls the construct name out of a call for a diagnostic, so an
// error says which read or write failed rather than quoting the whole string
// (which can carry a path somebody would rather not see in a log).
func constructOf(call string) string {
	fields := strings.Fields(call)
	if len(fields) < 2 {
		return "call"
	}
	name := fields[1]
	if idx := strings.IndexByte(name, '('); idx > 0 {
		name = name[:idx]
	}
	return name
}

// Watch is one arrangement this machine is asked to keep.
type Watch struct {
	Mode            string
	IntervalMinutes int
	LastSweepAt     time.Time
	ID              string
	WorkerID        string
	LocalPath       string
	FolderID        string
	Status          string
	ExcludeGlobs    []string
	IncludeHidden   bool
}

// Active reports whether the sweeper should be doing anything for this watch.
// A paused watch is READ and kept -- its ledger stays, so resuming does not
// re-push the folder -- but nothing is scanned or sent.
func (w Watch) Active() bool { return w.Status != "paused" }

// Watches returns the arrangements for one machine, or -- with a BLANK
// workerID -- every one the caller has.
//
// THE ARGUMENT IS OMITTED WHEN BLANK, and that is the whole difference between
// the two questions. The query guards it with `when(args.workerId)`, and that
// guard drops an ABSENT argument, not an empty string: `workerId: ""` is a
// present value that compares against every row and matches none. Rendering
// the argument unconditionally therefore turned "show me everything" into
// "show me the rows belonging to the machine whose id is the empty string" --
// which answers nothing, forever, and reads exactly like a person having set
// no backups up.
func (g *Graph) Watches(ctx context.Context, workerID string) ([]Watch, error) {
	call := "query libraryWatchedFolders()"
	if strings.TrimSpace(workerID) != "" {
		call = "query libraryWatchedFolders(workerId: " + langparser.QuoteString(workerID) + ")"
	}
	rows, err := g.call(ctx, call)
	if err != nil {
		return nil, err
	}
	out := make([]Watch, 0, len(rows))
	for _, row := range rows {
		w := Watch{
			Mode:            rowString(row, "mode"),
			ID:              rowString(row, "id"),
			WorkerID:        rowString(row, "workerId"),
			LocalPath:       rowString(row, "localPath"),
			FolderID:        rowString(row, "folderId"),
			Status:          rowString(row, "status"),
			ExcludeGlobs:    rowStrings(row, "excludeGlobs"),
			IncludeHidden:   rowBool(row, "includeHidden"),
			IntervalMinutes: backupInterval(row["intervalMinutes"]),
		}
		w.LastSweepAt, _ = time.Parse(time.RFC3339Nano, rowString(row, "lastSweepAt"))
		// A row with no id or no path is one this build cannot act on. Skipped
		// rather than errored: one malformed row must not stop the other
		// watches on this machine from being swept.
		if w.ID == "" || w.LocalPath == "" {
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

// SweepReport is what one pass found at the origin.
type SweepReport struct {
	WatchID     string
	OriginState string
	FilesSeen   int
	BytesSeen   int64
	Error       string
}

// ReportSweep records what this machine saw.
//
// `lastSweepAt` is NOT sent, and cannot be: the mutation stamps it server-side
// so a caller cannot claim to have looked at a time it did not. That is the
// whole reason the field is trustworthy enough to render.
func (g *Graph) ReportSweep(ctx context.Context, report SweepReport) error {
	call := "mutation reportLibraryWatchedFolderSweep(" +
		"watchId: " + langparser.QuoteString(report.WatchID) +
		", originState: " + langparser.QuoteString(report.OriginState) +
		", filesSeen: " + strconv.Itoa(report.FilesSeen) +
		", bytesSeen: " + strconv.FormatInt(report.BytesSeen, 10) +
		", lastSweepError: " + langparser.QuoteString(report.Error) +
		")"
	_, err := g.call(ctx, call)
	return err
}

// SetLinkState reports how one file's copy stands against its origin.
//
// The engine writes `synced` itself on any push naming a (machine, path), so
// this is the lane for the two states only something looking at the origin can
// give -- plus the periodic re-stamp that keeps a `synced` from silently
// meaning "as of three weeks ago".
func (g *Graph) SetLinkState(ctx context.Context, fileID, state string) error {
	call := "mutation setLibraryFileLinkState(fileId: " + langparser.QuoteString(fileID) +
		", linkState: " + langparser.QuoteString(state) + ")"
	_, err := g.call(ctx, call)
	return err
}

// FileIDAt resolves the live Library file at one (machine, path), or "" when
// there is none.
//
// Used only where the ledger has no id -- a first sweep after the cache was
// lost. The query excludes archived rows, so an empty answer means "start a
// fresh file", which is the coherent reading of somebody having binned it.
func (g *Graph) FileIDAt(ctx context.Context, workerID, path string) (string, error) {
	call := "query libraryFileByUploadedFrom(workerId: " + langparser.QuoteString(workerID) +
		", path: " + langparser.QuoteString(path) + ")"
	rows, err := g.call(ctx, call)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rowString(rows[0], "id"), nil
}

// ---------------------------------------------------------------------------
// row readers
// ---------------------------------------------------------------------------
//
// A row arrives either flat (a shaped query's result) or wrapped in `payload`.
// Both are read the same way, for the reason the OS client flattens first: a
// value that resolved on one path and not the other would produce a sweeper
// that worked against one release and silently did nothing against another.

func rowField(row map[string]any, key string) any {
	if v, ok := row[key]; ok {
		return v
	}
	if payload, ok := row["payload"].(map[string]any); ok {
		return payload[key]
	}
	return nil
}

func rowString(row map[string]any, key string) string {
	if s, ok := rowField(row, key).(string); ok {
		return s
	}
	return ""
}

func rowBool(row map[string]any, key string) bool {
	if b, ok := rowField(row, key).(bool); ok {
		return b
	}
	return false
}

func rowStrings(row map[string]any, key string) []string {
	raw, ok := rowField(row, key).([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		if s, ok := entry.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// FileAt reads the remote baseline when a local ledger has been lost or predates
// version preconditions. It never assumes an existing remote edit is disposable.
func (g *Graph) FileAt(ctx context.Context, workerID, path string) (map[string]any, error) {
	rows, err := g.call(ctx, "query libraryFileByUploadedFrom(workerId: "+langparser.QuoteString(workerID)+", path: "+langparser.QuoteString(path)+")")
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func (g *Graph) Relink(ctx context.Context, fileID, workerID, previousPath, path string) error {
	_, err := g.call(ctx, "builtin relinkLibraryFileOrigin(fileId: "+langparser.QuoteString(fileID)+", workerId: "+langparser.QuoteString(workerID)+", previousPath: "+langparser.QuoteString(previousPath)+", path: "+langparser.QuoteString(path)+")")
	return err
}

// Invalid legacy values use the former cadence. The DSL bounds new values.
func backupInterval(v any) int {
	value, err := strconv.Atoi(fmt.Sprint(v))
	if err != nil || value < 5 || value > 525600 {
		return 5
	}
	return value
}

func (w Watch) Due(now time.Time) bool {
	minutes := w.IntervalMinutes
	if minutes < 5 || minutes > 525600 {
		minutes = 5
	}
	// A future server stamp must not suspend a backup indefinitely after clock correction.
	return w.LastSweepAt.IsZero() || w.LastSweepAt.After(now) || !now.Before(w.LastSweepAt.Add(time.Duration(minutes)*time.Minute))
}
