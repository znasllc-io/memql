package filebackups

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

type engineAdapter struct{ memql.IntegrationEngineAccess }

func (e engineAdapter) Execute(ctx context.Context, q string) (any, error) {
	return e.IntegrationEngineAccess.Execute(ctx, q)
}
func init() {
	memql.RegisterPlugin("fileBackups", func(p memql.PluginContext) (memql.IntegrationProvider, error) {
		engine := engineAdapter{p.Engine}
		s := &Service{DB: func() *sql.DB {
			if p.BunDB == nil || p.BunDB() == nil {
				return nil
			}
			return p.BunDB().DB
		}, GateDB: p.GateDB, Engine: engine, Container: azureblob.ContainerFromEnv()}
		s.Library = server.NewEngineLibraryStore(engine, p.GateDB)
		if s.Container != "" {
			u, err := azureblob.New(context.Background())
			if err == nil {
				s.Provider = u
			}
		}
		return s, nil
	})
}
func (s *Service) IntegrationName() string { return "fileBackups" }

type operation func(context.Context, map[string]any) (any, error)

func reply(value any) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "file-backup-reply", Concept: "integration:fileBackups:reply", Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}
func str(a map[string]any, k string) string { v, _ := a[k].(string); return v }
func integer(a map[string]any, k string) (int64, error) {
	raw, err := json.Marshal(a[k])
	if err != nil {
		return 0, err
	}
	var v int64
	if err = json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("%s must be an integer byte count", k)
	}
	return v, nil
}
func (s *Service) Capabilities() []memql.IntegrationCapability {
	operations := []struct {
		name, description string
		args              map[string]string
		run               operation
	}{
		{"create", "Create a folder-file, archive upload or download request", map[string]string{"id": "string", "workerId": "string", "path": "string", "kind": "string", "metadata": "object"}, func(ctx context.Context, a map[string]any) (any, error) {
			var meta Metadata
			if raw, ok := a["metadata"]; ok {
				data, e := json.Marshal(raw)
				if e != nil {
					return nil, e
				}
				if e = json.Unmarshal(data, &meta); e != nil {
					return nil, e
				}
			}
			return s.Create(ctx, str(a, "id"), str(a, "workerId"), str(a, "path"), str(a, "kind"), meta)
		}},
		{"prepare", "Reserve the complete source before issuing a write grant", map[string]string{"id": "string", "size": "int", "sha256": "string"}, func(ctx context.Context, a map[string]any) (any, error) {
			n, e := integer(a, "size")
			if e != nil {
				return nil, e
			}
			return s.Prepare(ctx, str(a, "id"), n, str(a, "sha256"))
		}},
		{"block", "Authorize one exact bounded block", map[string]string{"id": "string", "ordinal": "int", "md5": "string", "sha256": "string"}, func(ctx context.Context, a map[string]any) (any, error) {
			n, e := integer(a, "ordinal")
			if e != nil || n < 0 || n >= 50000 {
				return nil, errors.New("invalid block ordinal")
			}
			return s.Grant(ctx, str(a, "id"), int(n), str(a, "md5"), str(a, "sha256"))
		}},
		{"checkpoint", "Commit and reconcile an immutable durable prefix", map[string]string{"id": "string", "final": "bool"}, func(ctx context.Context, a map[string]any) (any, error) {
			final, _ := a["final"].(bool)
			return s.Checkpoint(ctx, str(a, "id"), final)
		}},
		{"control", "Pause, resume, cancel, trash, restore or purge a backup", map[string]string{"id": "string", "action": "string"}, func(ctx context.Context, a map[string]any) (any, error) {
			return s.Control(ctx, str(a, "id"), str(a, "action"))
		}},
		{"read", "Read one caller-owned transfer", map[string]string{"id": "string"}, func(ctx context.Context, a map[string]any) (any, error) {
			owner, e := principal(ctx, false)
			if e != nil {
				return nil, e
			}
			t, err := s.read(ctx, str(a, "id"), owner)
			if err == nil && t.Kind == "sync_file" && t.Desired == "running" && t.State != "complete" {
				root, e := s.Library.BackupDirectory(memql.ContextWithFreshRead(ctx), t.Metadata.WatchID, t.WorkerID)
				if e != nil {
					return nil, e
				}
				if root == "" {
					t.Desired = "paused"
				}
			}
			return t, err
		}},
		{"list", "Page the caller's backup transfers", map[string]string{"workerId": "string", "after": "string", "scope": "string"}, func(ctx context.Context, a map[string]any) (any, error) {
			rows, e := s.List(ctx, str(a, "workerId"), str(a, "after"), str(a, "scope"))
			if e != nil {
				return nil, e
			}
			next := ""
			if len(rows) > 100 {
				rows = rows[:100]
				next = rows[99].ID
			}
			return map[string]any{"transfers": rows, "nextCursor": next}, nil
		}},
		{"downloadBlock", "Authorize an exact range pinned to its stored version", map[string]string{"id": "string", "ordinal": "int"}, func(ctx context.Context, a map[string]any) (any, error) {
			n, e := integer(a, "ordinal")
			if e != nil || n < 0 || n >= 50000 {
				return nil, errors.New("invalid block ordinal")
			}
			return s.DownloadGrant(ctx, str(a, "id"), int(n))
		}},
		{"report", "Record verified download progress or a recoverable local error", map[string]string{"id": "string", "state": "string", "message": "string", "offset": "int"}, func(ctx context.Context, a map[string]any) (any, error) {
			n, e := integer(a, "offset")
			if e != nil {
				return nil, e
			}
			return s.Report(ctx, str(a, "id"), str(a, "state"), str(a, "message"), n)
		}},
		{"usage", "Read storage used, reserved and available", map[string]string{"userId": "string"}, s.usage},
		{"setUserLimit", "Set a normal user's storage allowance", map[string]string{"userId": "string", "bytes": "int"}, s.setLimit},
		{"syncDelete", "Move an unchanged synced file to the Bin", map[string]string{"watchId": "string", "workerId": "string", "path": "string", "fileId": "string", "sha256": "string", "version": "int"}, func(ctx context.Context, a map[string]any) (any, error) {
			n, e := integer(a, "version")
			if e != nil || n < 1 || n > 2147483647 {
				return nil, errors.New("invalid file revision")
			}
			e = s.SyncDelete(ctx, str(a, "watchId"), str(a, "workerId"), str(a, "path"), str(a, "fileId"), str(a, "sha256"), int(n))
			return map[string]bool{"archived": e == nil}, e
		}},
	}
	out := make([]memql.IntegrationCapability, 0, len(operations))
	for _, o := range operations {
		out = append(out, memql.IntegrationCapability{Name: o.name, Description: o.description, ArgsSchema: o.args, Handler: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			v, e := o.run(ctx, args)
			if e != nil {
				return nil, e
			}
			return reply(v)
		}})
	}
	return out
}
func manageQuota(ctx context.Context) bool {
	ac, ok := auth.AccessFromContext(ctx)
	return ok && ac != nil && !ac.Synthetic && !ac.RoleStandIn && (ac.Role == auth.RoleOwner || ac.Role == auth.RoleDeveloper)
}
func (s *Service) usage(ctx context.Context, a map[string]any) (any, error) {
	owner, err := principal(ctx, false)
	if err != nil {
		return nil, err
	}
	if s.Provider == nil {
		return nil, errors.New("backup storage is unavailable")
	}
	// Also forces the complete provider inventory before showing any free bytes.
	u, err := s.Provider.StorageUsage(ctx)
	if err != nil {
		return nil, err
	}
	target := str(a, "userId")
	if target == "" || target == owner {
		return u, nil
	}
	if !manageQuota(ctx) {
		return nil, errors.New("only owners and developers can view another storage allowance")
	}
	return azureblob.CapacityStore().Usage(ctx, memql.BareShortId(target))
}
func (s *Service) setLimit(ctx context.Context, a map[string]any) (any, error) {
	owner, err := principal(ctx, true)
	if err != nil {
		return nil, err
	}
	if !manageQuota(ctx) {
		return nil, errors.New("only owners and developers can change storage allowances")
	}
	limit, err := integer(a, "bytes")
	if err != nil {
		return nil, err
	}
	store := azureblob.CapacityStore()
	if store == nil {
		return nil, errors.New("storage accounting is unavailable")
	}
	target := memql.BareShortId(str(a, "userId"))
	if err = store.SetLimit(ctx, owner, target, limit); err != nil {
		return nil, err
	}
	return store.Usage(ctx, target)
}
