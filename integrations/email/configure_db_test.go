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

// A CREDENTIAL SAVED FROM SETTINGS LANDS FOR BOTH ROLES THE SECTION ADMITS,
// against the real engine and a real Postgres.
//
// The unit tests beside this one answer from a recording writer, so they
// cannot say what the engine does with the statement. This runs the real
// handler into the real engine, where v1:platform:globalSecret declares the
// cluster-owner tier: an owner's credential is admitted as the owner's, and a
// developer's is admitted only because the handler, having authorized the
// developer, stamps internal origin for that one write. Without the stamp a
// developer's save is refused by the tier while the section tells them they
// may make it.
//
// The developer's OWN call is still refused. The stamp belongs to the handler
// and covers the one statement it composed; it is not something the person who
// triggered it holds.
func TestConfigureSealsACredentialForEveryRoleTheSectionAdmits(t *testing.T) {
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

	slot, ok := EmailConfigManifest().SlotByName("clientSecret")
	if !ok || !slot.Secret {
		t.Fatalf("clientSecret is not a credential slot of the manifest: %+v", slot)
	}
	caller := func(role auth.Role, id string) context.Context {
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: id, Role: role})
		return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: id})
	}

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		t.Run(string(role), func(t *testing.T) {
			// The row is written under a name of this run's own, so a real
			// credential name never lands in the database the suites share.
			suffix := fmt.Sprintf("%s_%d", strings.ToUpper(string(role)), time.Now().UnixNano())
			store := configureTestStore{ConfigWriter: NewConfigWriter(e), envVar: slot.EnvVar, suffix: suffix}
			lazy := NewLazySender(NewLogSender(slog.Default()), nil, nil, slog.Default())
			i := NewIntegration(lazy, slog.Default()).WithConfigWriter(store)

			ctx := caller(role, "configure-"+strings.ToLower(suffix))
			credential := "credential-saved-by-" + string(role)
			if _, err := i.handleConfigure(ctx, map[string]any{"slot": slot.Name, "value": credential}, 0); err != nil {
				t.Fatalf("a %s's credential was not saved: %v", role, err)
			}
			got, err := e.ResolveSystemSecret(context.Background(), store.name())
			if err != nil || got != credential {
				t.Fatalf("the credential a %s saved resolved to %d byte(s) and %v; want it back", role, len(got), err)
			}

			if role == auth.RoleOwner {
				return
			}
			// The same statement, sent by the developer rather than by the handler.
			_, err = e.Execute(ctx, renderConfigCall("setGlobalSecret", map[string]string{
				"id": "configure-direct-" + strings.ToLower(suffix), "name": "CONFIGURE_DIRECT_" + suffix,
				"encryptedValue": "bm90IGEgY2lwaGVydGV4dA==", "fingerprint": "...none",
			}))
			if err == nil || !strings.Contains(err.Error(), "row-authz") {
				t.Fatalf("a %s's own setGlobalSecret answered %v; want the row tier's refusal", role, err)
			}
		})
	}
}

// configureTestStore sends the handler's statements to the real engine, with
// the credential's row written under a name and id of this run's own.
type configureTestStore struct {
	ConfigWriter
	envVar, suffix string
}

func (s configureTestStore) name() string { return s.envVar + "_" + s.suffix }

func (s configureTestStore) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "mutation setGlobalSecret(") {
		id := rowIdFor("secret-global-", s.envVar)
		query = strings.ReplaceAll(query, `"`+id+`"`, `"`+rowIdFor("secret-global-", s.name())+`"`)
		query = strings.ReplaceAll(query, `"`+s.envVar+`"`, `"`+s.name()+`"`)
	}
	return s.ConfigWriter.Execute(ctx, query)
}
