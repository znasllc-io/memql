package email

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// A VALUE SAVED FROM SETTINGS LANDS FOR BOTH ROLES THE SECTION ADMITS, against
// the real engine and a real Postgres.
//
// The unit tests beside this one answer from a recording writer, so they
// cannot say what the engine does with the statement. This runs the real
// handler into the real engine, where both rows it writes are the deployment's:
// v1:platform:globalSecret declares the cluster-owner tier, and a
// v1:platform:globalVariable row is written by a cluster owner or by stamped
// server code. An owner's value is admitted as the owner's, and a developer's
// is admitted only because the handler, having authorized the developer, stamps
// internal origin for that one write. Without the stamp a developer's save is
// refused while the section tells them they may make it.
//
// The developer's OWN call is still refused. The stamp belongs to the handler
// and covers the one statement it composed; it is not something the person who
// triggered it holds.
func TestConfigureSavesAValueForEveryRoleTheSectionAdmits(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ad", 32))
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "email configure", dbtest.DSN(), err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}

	caller := func(role auth.Role, id string) context.Context {
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: id, Role: role})
		return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: id})
	}

	// One sealed slot and one plain one: the two rows the handler writes.
	for _, slotName := range []string{"clientSecret", "senderAddress"} {
		slot, ok := EmailConfigManifest().SlotByName(slotName)
		if !ok || slot.Secret != (slotName == "clientSecret") {
			t.Fatalf("%s is not the slot of the manifest this test was written against: %+v", slotName, slot)
		}
		for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
			t.Run(slotName+"/"+string(role), func(t *testing.T) {
				// The row is written under a name of this run's own, so a real
				// setting's name never lands in the database the suites share.
				suffix := fmt.Sprintf("%s_%d", strings.ToUpper(string(role)), time.Now().UnixNano())
				store := configureTestStore{ConfigWriter: NewConfigWriter(e), envVar: slot.EnvVar, secret: slot.Secret, suffix: suffix}
				lazy := NewLazySender(NewLogSender(slog.Default()), nil, nil, slog.Default())
				i := NewIntegration(lazy, slog.Default()).WithConfigWriter(store)

				ctx := caller(role, "configure-"+strings.ToLower(suffix))
				saved := "saved-by-" + string(role) + "@example.test"
				if _, err := i.handleConfigure(ctx, map[string]any{"slot": slot.Name, "value": saved}, 0); err != nil {
					t.Fatalf("a %s's %s was not saved: %v", role, slot.Name, err)
				}
				var got string
				var err error
				if slot.Secret {
					got, err = e.ResolveSystemSecret(context.Background(), store.name())
				} else {
					got, err = e.ResolveSystemVariable(context.Background(), store.name())
				}
				if err != nil || got != saved {
					t.Fatalf("the %s a %s saved resolved to %d byte(s) and %v; want it back", slot.Name, role, len(got), err)
				}

				if role == auth.RoleOwner {
					return
				}
				// The same statement, sent by the developer rather than by the handler.
				direct, refusal := renderConfigCall("setGlobalVariable", map[string]string{
					"id": "configure-direct-" + strings.ToLower(suffix), "name": "CONFIGURE_DIRECT_" + suffix, "value": "sent directly",
				}), "configuration variable"
				if slot.Secret {
					direct, refusal = renderConfigCall("setGlobalSecret", map[string]string{
						"id": "configure-direct-" + strings.ToLower(suffix), "name": "CONFIGURE_DIRECT_" + suffix,
						"encryptedValue": "bm90IGEgY2lwaGVydGV4dA==", "fingerprint": "...none",
					}), "row-authz"
				}
				_, err = e.Execute(ctx, direct)
				if err == nil || !strings.Contains(err.Error(), refusal) {
					t.Fatalf("a %s's own write answered %v; want the row's own refusal (%s)", role, err, refusal)
				}
			})
		}
	}
}

// configureTestStore sends the handler's statements to the real engine, with
// the row written under a name and id of this run's own.
type configureTestStore struct {
	ConfigWriter
	envVar, suffix string
	secret         bool
}

func (s configureTestStore) name() string { return s.envVar + "_" + s.suffix }

func (s configureTestStore) Execute(ctx context.Context, query string) (any, error) {
	mutation, prefix := "mutation setGlobalVariable(", "var-global-"
	if s.secret {
		mutation, prefix = "mutation setGlobalSecret(", "secret-global-"
	}
	if strings.HasPrefix(query, mutation) {
		id := rowIdFor(prefix, s.envVar)
		query = strings.ReplaceAll(query, `"`+id+`"`, `"`+rowIdFor(prefix, s.name())+`"`)
		query = strings.ReplaceAll(query, `"`+s.envVar+`"`, `"`+s.name()+`"`)
	}
	return s.ConfigWriter.Execute(ctx, query)
}
