// Package storagequota owns durable, cluster-wide physical storage admission.
// It deliberately knows nothing about HTTP, Azure credentials or editable rows.
package storagequota

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/core/gatecontext"
)

const (
	DefaultUserBytes int64 = 10_000_000_000
	ClusterBytes     int64 = 5_000_000_000_000
	BackupBytes      int64 = 1_000_000_000_000
	BlockBytes       int64 = 64 << 20
	// Charge bounded provider metadata as well as payload. SQL journals and local
	// working disk are separate resources, not Azure Blob payload.
	ObjectOverhead int64 = 1024
)

var ErrLimit = errors.New("storage limit reached; stored files, the Bin and unfinished transfers all count")
var ErrRetired = errors.New("this storage destination has been permanently retired")

type Store struct {
	DB          func() *sql.DB
	GateDB      func() *sql.DB
	inventoryMu sync.Mutex
	inventories map[string]*inventoryFlight
}
type Usage struct {
	Used            int64 `json:"used"`
	Reserved        int64 `json:"reserved"`
	Limit           int64 `json:"limit"`
	Available       int64 `json:"available"`
	ClusterUsed     int64 `json:"clusterUsed"`
	ClusterReserved int64 `json:"clusterReserved"`
	ClusterLimit    int64 `json:"clusterLimit"`
	Exempt          bool  `json:"exempt"`
}

// OwnerID gives accounting one key per person. Only identity:user IDs are
// aliases; a node in another concept must never become that person's quota.
func OwnerID(value string) string {
	return strings.TrimPrefix(value, "v1:identity:user:")
}

func (s *Store) Database() (*sql.DB, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("storage accounting is unavailable")
	}
	db := s.DB()
	if db == nil {
		return nil, errors.New("storage accounting is unavailable")
	}
	return db, nil
}

func lock(ctx context.Context, tx *sql.Tx, name string) error {
	sum := sha256.Sum256([]byte("storage-capacity-v1:" + name))
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(sum[:8])))
	return err
}

// Guard serializes effects on ONE object across replicas. Its transaction holds
// no capacity rows. Admission commits separately before the provider is called,
// so a killed process or a lost response cannot release promised capacity.
// Transaction-scoped locks also work behind transaction-pooling proxies.
type Guard struct {
	Store               *Store
	Tx                  *sql.Tx
	Key, Account, Owner string
	borrowed            bool
}

func (s *Store) Guard(ctx context.Context, account, key, owner string) (*Guard, error) {
	owner = OwnerID(owner)
	if account == "" || key == "" || len(key) > 8192 || len(owner) > 512 {
		return nil, errors.New("invalid storage identity")
	}
	if s == nil || s.GateDB == nil || s.GateDB() == nil {
		return nil, errors.New("storage write gate pool is unavailable")
	}
	db := s.GateDB()
	tx := gatecontext.Transaction(ctx, db)
	borrowed := tx != nil
	var err error
	if !borrowed {
		tx, err = db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
	}
	// Bound the server's lock wait below the driver's socket deadline. A lock
	// error aborts the shared transaction too; the outer operation must retry.
	wait := 6 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline)-100*time.Millisecond)
	}
	if wait <= 0 {
		if !borrowed {
			_ = tx.Rollback()
		}
		return nil, context.DeadlineExceeded
	}
	_, err = tx.ExecContext(ctx, "SELECT set_config('lock_timeout', $1, true)", fmt.Sprint(max(int64(1), wait.Milliseconds())))
	if err != nil {
		if !borrowed {
			_ = tx.Rollback()
		}
		return nil, err
	}
	if err = lock(ctx, tx, "object:"+key); err != nil {
		if !borrowed {
			_ = tx.Rollback()
		}
		return nil, err
	}
	return &Guard{Store: s, Tx: tx, Key: key, Account: account, Owner: owner, borrowed: borrowed}, nil
}
func (g *Guard) Close() {
	if !g.borrowed {
		_ = g.Tx.Rollback()
	}
}

// Initialize imports the provider inventory once, before any managed write.
// The callback must include every retained object and staged block in the account.
// A partial inventory rolls back and leaves writes disabled.
func (s *Store) Initialize(ctx context.Context, account string, inventory func(func(key, owner string, size int64) error) error) error {
	db, err := s.Database()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Another replica can be importing for minutes. Never fill the request
	// pool with waiters for an inventory that must finish before writes open.
	sum := sha256.Sum256([]byte("storage-capacity-v1:capacity"))
	var acquired bool
	if err = tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(sum[:8]))).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return ErrInventoryPending
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO storage_capacity_accounts(account) VALUES($1) ON CONFLICT DO NOTHING`, account); err != nil {
		return err
	}
	var ready bool
	var blocked string
	if err = tx.QueryRowContext(ctx, `SELECT inventoried,blocked_reason FROM storage_capacity_accounts WHERE account=$1`, account).Scan(&ready, &blocked); err != nil {
		return err
	}
	if blocked != "" {
		return errors.New(blocked)
	}
	if !ready {
		type entry struct {
			Key   string `json:"key"`
			Owner string `json:"owner"`
			Size  int64  `json:"size"`
		}
		batch := make([]entry, 0, 512)
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			data, e := json.Marshal(batch)
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, `WITH added AS (
INSERT INTO storage_capacity_objects(object_key,account,owner_id,used_bytes,capacity_bytes)
SELECT x.key,$1,x.owner,x.size+$3,x.size+$3 FROM jsonb_to_recordset($2::jsonb) AS x(key text,owner text,size bigint)
ON CONFLICT(object_key) DO NOTHING RETURNING owner_id,capacity_bytes
), changes AS (
SELECT 'cluster' AS scope,sum(capacity_bytes) AS used FROM added
UNION ALL SELECT 'user:' || owner_id,sum(capacity_bytes) FROM added GROUP BY owner_id
)
INSERT INTO storage_capacity_totals(scope,used_bytes,reserved_bytes)
SELECT scope,used,0 FROM changes WHERE used IS NOT NULL
ON CONFLICT(scope) DO UPDATE SET used_bytes=storage_capacity_totals.used_bytes+EXCLUDED.used_bytes`, account, string(data), ObjectOverhead)
			batch = batch[:0]
			return e
		}
		err = inventory(func(key, owner string, size int64) error {
			if size < 0 || size > ClusterBytes {
				return ErrLimit
			}
			batch = append(batch, entry{Key: key, Owner: OwnerID(owner), Size: size})
			if len(batch) == cap(batch) {
				return flush()
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err = flush(); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE storage_capacity_accounts SET inventoried=true WHERE account=$1`, account); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) transaction(ctx context.Context, f func(*sql.Tx) error) error {
	db, err := s.Database()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx, "capacity"); err != nil {
		return err
	}
	if err = f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Fresh identity reads determine quota exemptions, including borrowed actors.
// Token claims and cached role values cannot grant unlimited personal storage.
// staged-data: MUST-NOT-GATE -- hiding a new role revocation would preserve a
// superseded owner's quota exemption and admit storage after its removal.
func userLimit(ctx context.Context, tx *sql.Tx, owner string) (int64, bool, error) {
	owner = OwnerID(owner)
	if owner == "" {
		return ClusterBytes, true, nil
	} // cluster-owned logs/package assets
	var payload string
	err := tx.QueryRowContext(ctx, `SELECT payload::text FROM "MemoryNodes" WHERE concept='v1:identity:user'
AND (id=$1 OR id='v1:identity:user:' || $1) ORDER BY "createdAt" DESC LIMIT 1`, owner).Scan(&payload)
	if err != nil {
		return 0, false, fmt.Errorf("cannot resolve storage owner: %w", err)
	}
	return limitFromPayload(ctx, tx, owner, []byte(payload))
}

func usage(ctx context.Context, tx *sql.Tx, owner string) (Usage, error) {
	owner = OwnerID(owner)
	u := Usage{ClusterLimit: ClusterBytes}
	var err error
	u.Limit, u.Exempt, err = userLimit(ctx, tx, owner)
	if err != nil {
		return u, err
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT used_bytes FROM storage_capacity_totals WHERE scope='cluster'),0),
 COALESCE((SELECT reserved_bytes FROM storage_capacity_totals WHERE scope='cluster'),0),
 COALESCE((SELECT used_bytes FROM storage_capacity_totals WHERE scope=$1),0),
 COALESCE((SELECT reserved_bytes FROM storage_capacity_totals WHERE scope=$1),0)`, "user:"+owner).Scan(&u.ClusterUsed, &u.ClusterReserved, &u.Used, &u.Reserved)
	if err != nil {
		return u, err
	}
	u.Available = max(int64(0), min(u.Limit-u.Used-u.Reserved, ClusterBytes-u.ClusterUsed-u.ClusterReserved))
	return u, nil
}
func (s *Store) Usage(ctx context.Context, owner string) (Usage, error) {
	var u Usage
	err := s.transaction(ctx, func(tx *sql.Tx) error { var e error; u, e = usage(ctx, tx, owner); return e })
	return u, err
}

// reserve changes capacity only under the cluster lock. planned is an upfront
// reservation, needed is the largest physical footprint of the next effect.
func (g *Guard) reserve(ctx context.Context, planned, needed int64, change func(*sql.Tx) error) error {
	if planned < 0 || needed < 0 || planned > ClusterBytes || needed > ClusterBytes {
		return ErrLimit
	}
	return g.Store.transaction(ctx, func(tx *sql.Tx) error {
		var ready bool
		if err := tx.QueryRowContext(ctx, `SELECT inventoried FROM storage_capacity_accounts WHERE account=$1`, g.Account).Scan(&ready); err != nil || !ready {
			return errors.New("storage inventory must finish before uploads")
		}
		var owner string
		var capacity, oldPlan int64
		var retired bool
		err := tx.QueryRowContext(ctx, `SELECT owner_id,capacity_bytes,planned_bytes,retired FROM storage_capacity_objects WHERE object_key=$1`, g.Key).Scan(&owner, &capacity, &oldPlan, &retired)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if retired {
			return ErrRetired
		}
		if err == nil && owner != g.Owner {
			return errors.New("storage destination belongs to a different owner")
		}
		target := max(capacity, max(planned, needed)+ObjectOverhead)
		u, e := usage(ctx, tx, g.Owner)
		if e != nil {
			return e
		}
		if delta := target - capacity; delta > 0 && (delta > u.Available) {
			return fmt.Errorf("%w (%d bytes available, %d required)", ErrLimit, u.Available, delta)
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO storage_capacity_objects(object_key,account,owner_id,capacity_bytes,planned_bytes)
VALUES($1,$2,$3,$4,$5) ON CONFLICT(object_key) DO UPDATE SET capacity_bytes=$4,planned_bytes=$5,updated_at=clock_timestamp()`, g.Key, g.Account, g.Owner, target, max(planned, oldPlan))
		if e != nil {
			return e
		}
		if e = totals(ctx, tx, g.Owner, 0, target-capacity); e != nil {
			return e
		}
		if change != nil {
			return change(tx)
		}
		return nil
	})
}
func (g *Guard) Plan(ctx context.Context, size int64) error { return g.reserve(ctx, size, 0, nil) }
func (g *Guard) Put(ctx context.Context, size int64) error {
	var used int64
	db, err := g.Store.Database()
	if err != nil {
		return err
	}
	err = db.QueryRowContext(ctx, `SELECT used_bytes FROM storage_capacity_objects WHERE object_key=$1`, g.Key).Scan(&used)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return g.reserve(ctx, 0, max(int64(0), used-ObjectOverhead)+size, nil)
}
func (g *Guard) Stage(ctx context.Context, id string, size int64) error {
	if id == "" || len(id) > 128 || size <= 0 {
		return errors.New("invalid storage block")
	}
	var used, staged int64
	db, err := g.Store.Database()
	if err != nil {
		return err
	}
	err = db.QueryRowContext(ctx, `SELECT COALESCE((SELECT greatest(used_bytes-$2,0) FROM storage_capacity_objects WHERE object_key=$1),0),
COALESCE((SELECT sum(size_bytes) FROM storage_capacity_blocks WHERE object_key=$1 AND NOT committed AND block_id<>$3),0)`, g.Key, ObjectOverhead, id).Scan(&used, &staged)
	if err != nil {
		return err
	}
	return g.reserve(ctx, 0, used+staged+size, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO storage_capacity_blocks(object_key,block_id,size_bytes,committed) VALUES($1,$2,$3,false)
ON CONFLICT(object_key,block_id,committed) DO UPDATE SET size_bytes=$3`, g.Key, id, size)
		return e
	})
}

// Settled is called only after the provider confirmed the exact effect and no
// old writer can change it. An unknown outcome deliberately keeps capacity.
func (g *Guard) Settled(ctx context.Context, size int64, final bool) error {
	if size < 0 || size > ClusterBytes {
		return ErrLimit
	}
	return g.Store.transaction(ctx, func(tx *sql.Tx) error { return g.settled(ctx, tx, size, final) })
}
func (g *Guard) settled(ctx context.Context, tx *sql.Tx, size int64, final bool) error {
	var capacity, plan, oldUsed int64
	if err := tx.QueryRowContext(ctx, `SELECT capacity_bytes,planned_bytes,used_bytes FROM storage_capacity_objects WHERE object_key=$1`, g.Key).Scan(&capacity, &plan, &oldUsed); err != nil {
		return err
	}
	if size+ObjectOverhead > capacity {
		return errors.New("provider receipt exceeds reserved storage")
	}
	if final {
		plan = 0
	}
	_, err := tx.ExecContext(ctx, `UPDATE storage_capacity_objects SET used_bytes=$2,capacity_bytes=$3,planned_bytes=$4,updated_at=clock_timestamp() WHERE object_key=$1`, g.Key, size+ObjectOverhead, max(size, plan)+ObjectOverhead, plan)
	if err != nil {
		return err
	}
	return totals(ctx, tx, g.Owner, size+ObjectOverhead-oldUsed, max(size, plan)-size-(capacity-oldUsed))
}

// Retired retains a charged zero-byte tombstone. The caller must first fence
// every issued write grant and confirm provider cleanup. Never delete this row.
func (g *Guard) Retired(ctx context.Context) error {
	return g.Store.transaction(ctx, func(tx *sql.Tx) error {
		var used, capacity int64
		if e := tx.QueryRowContext(ctx, `SELECT used_bytes,capacity_bytes FROM storage_capacity_objects WHERE object_key=$1`, g.Key).Scan(&used, &capacity); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `DELETE FROM storage_capacity_blocks WHERE object_key=$1`, g.Key); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, `UPDATE storage_capacity_objects SET retired=true,used_bytes=$2,capacity_bytes=$2,planned_bytes=0 WHERE object_key=$1`, g.Key, ObjectOverhead)
		if e != nil {
			return e
		}
		return totals(ctx, tx, g.Owner, ObjectOverhead-used, -(capacity - used))
	})
}

// Counters change in the same capacity transaction as the object. Admission
// is constant-time even with millions of files; a replica never scans every
// retained object to issue one 64 MiB block grant.
func totals(ctx context.Context, tx *sql.Tx, owner string, used, reserved int64) error {
	for _, scope := range []string{"cluster", "user:" + owner} {
		if _, e := tx.ExecContext(ctx, `INSERT INTO storage_capacity_totals(scope) VALUES($1) ON CONFLICT DO NOTHING`, scope); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `UPDATE storage_capacity_totals SET used_bytes=used_bytes+$2,reserved_bytes=reserved_bytes+$3 WHERE scope=$1`, scope, used, reserved); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) BlockAccount(ctx context.Context, account, reason string) error {
	db, e := s.Database()
	if e != nil {
		return e
	}
	_, e = db.ExecContext(ctx, `INSERT INTO storage_capacity_accounts(account,blocked_reason) VALUES($1,$2) ON CONFLICT(account) DO UPDATE SET blocked_reason=$2`, account, reason)
	return e
}
