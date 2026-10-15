// Package transfer moves backup bytes directly between this computer and
// storage. The cluster owns admission and commits; the computer owns local
// consent, source identity, verified ranges and durable disk checkpoints.
package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	sdk "github.com/znasllc-io/memql/sdk/go/client"
)

type Control interface {
	Call(context.Context, string, map[string]any, any) error
}
type RPC struct {
	Endpoint string
	Bearer   func(context.Context) (string, error)
	mu       sync.Mutex
	conn     *sdk.Connection
	token    string
}

func (r *RPC) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
}
func (r *RPC) Call(ctx context.Context, op string, args map[string]any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	token, err := r.Bearer(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("sign in on this machine to back up files")
	}
	r.mu.Lock()
	if r.conn == nil || r.token != token || r.conn.Status() != sdk.StatusConnected {
		if r.conn != nil {
			r.conn.Close()
			r.conn = nil
		}
		r.conn, err = sdk.Connect(ctx, sdk.ConnectConfig{Endpoint: r.Endpoint, Token: token})
		r.token = token
	}
	conn := r.conn
	r.mu.Unlock()
	if err != nil {
		return err
	}
	data, err := json.Marshal(args)
	if err != nil {
		return err
	}
	name := "libraryBackup" + op
	result, err := sdk.NewQueryClient(conn.Dispatcher()).ExecuteNamed(ctx, name, name+"("+string(data)+")")
	if err != nil {
		return err
	}
	rows := result.Rows()
	if len(rows) != 1 {
		return errors.New("the cluster returned no backup receipt")
	}
	data, err = json.Marshal(rows[0])
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

const BlockBytes int64 = 64 << 20
const MaxBytes int64 = 1_000_000_000_000

type Metadata struct {
	WatchID          string `json:"watchId,omitempty"`
	FolderID         string `json:"folderId,omitempty"`
	FileID           string `json:"fileId,omitempty"`
	ExpectedVersion  int    `json:"expectedVersion,omitempty"`
	VersionNumber    int    `json:"versionNumber,omitempty"`
	SourceTransferID string `json:"sourceTransferId,omitempty"`
	MimeType         string `json:"mimeType,omitempty"`
}
type Job struct {
	CheckpointBlocks int      `json:"checkpointBlocks"`
	ID               string   `json:"id"`
	WorkerID         string   `json:"workerId"`
	Path             string   `json:"path"`
	Kind             string   `json:"kind"`
	Name             string   `json:"name"`
	Size             int64    `json:"size"`
	SHA256           string   `json:"sha256"`
	State            string   `json:"state"`
	Desired          string   `json:"desiredState"`
	Committed        int64    `json:"committedBytes"`
	ETag             string   `json:"etag"`
	Metadata         Metadata `json:"metadata"`
	Failure          string   `json:"failure"`
}
type Grant struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expiresAt"`
}
type DownloadBlock struct {
	Request Grant  `json:"request"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Offset  int64  `json:"offset"`
}

func (r *Runner) Read(ctx context.Context, id string) (Job, error) {
	var j Job
	err := r.Control.Call(ctx, "Read", map[string]any{"id": id}, &j)
	return j, err
}
func (r *Runner) Create(ctx context.Context, j Job) (Job, error) {
	var out Job
	err := r.Control.Call(ctx, "Create", map[string]any{"id": j.ID, "workerId": j.WorkerID, "path": j.Path, "kind": j.Kind, "metadata": j.Metadata}, &out)
	return out, err
}
func (r *Runner) Action(ctx context.Context, id, action string) (Job, error) {
	var j Job
	err := r.Control.Call(ctx, "Control", map[string]any{"id": id, "action": action}, &j)
	return j, err
}
