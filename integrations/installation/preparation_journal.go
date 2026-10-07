package installation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

type preparationJournal struct{ db func() *sql.DB }

type preparationCapture struct {
	Started       bool                  `json:"started"`
	Receipt       *sourceCaptureReceipt `json:"receipt"`
	SourceDigest  string                `json:"sourceDigest"`
	ReceiptDigest string                `json:"receiptDigest"`
	Acknowledged  bool                  `json:"acknowledged"`
}

type preparationRecord struct {
	ID             string
	Scope          preparationScope
	SlotEpoch      int64
	State          string
	PromotedPlanID string
	Captures       map[string]preparationCapture
}

func (r preparationRecord) String() string {
	return fmt.Sprintf("installation preparation=%s state=%s", r.ID, r.State)
}
func (r preparationRecord) GoString() string { return r.String() }

func preparationActor(ctx context.Context) (string, error) {
	actor, err := installationActor(ctx)
	if err != nil || auth.OriginFromContext(ctx) != auth.OriginInternal {
		return "", errors.New("installation preparation requires an admitted native operator")
	}
	return actor, nil
}

func (j *preparationJournal) database() (*sql.DB, error) {
	if j == nil {
		return nil, errors.New("installation preparation journal is unavailable")
	}
	return (&revisionJournal{db: j.db}).database()
}

func lockPreparationHead(ctx context.Context, tx *sql.Tx, installation string) (string, string, int64, error) {
	var plan, preparation sql.NullString
	var epoch int64
	err := tx.QueryRowContext(ctx, `SELECT active_plan_id,active_preparation_id,slot_epoch FROM installation_revision_heads WHERE installation_id=$1 FOR UPDATE`, installation).Scan(&plan, &preparation, &epoch)
	return plan.String, preparation.String, epoch, err
}

func (j *preparationJournal) reserve(ctx context.Context, scope preparationScope) (preparationRecord, error) {
	actor, err := preparationActor(ctx)
	if err != nil {
		return preparationRecord{}, err
	}
	if actor != scope.RequestedBy {
		return preparationRecord{}, errors.New("preparation belongs to another requester")
	}
	body, key, err := scope.canonical()
	if err != nil {
		return preparationRecord{}, err
	}
	db, err := j.database()
	if err != nil {
		return preparationRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return preparationRecord{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO installation_revision_heads(installation_id) VALUES($1) ON CONFLICT DO NOTHING`, scope.InstallationID); err != nil {
		return preparationRecord{}, err
	}
	plan, active, epoch, err := lockPreparationHead(ctx, tx, scope.InstallationID)
	if err != nil {
		return preparationRecord{}, err
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT preparation_id FROM installation_preparations WHERE installation_id=$1 AND request_id=$2`, scope.InstallationID, scope.RequestID).Scan(&existing)
	if err == nil {
		if existing != key {
			return preparationRecord{}, errors.New("preparation request identity was reused with changed inputs")
		}
		r, err := readPreparation(ctx, tx, scope.InstallationID, key)
		if err != nil {
			return preparationRecord{}, err
		}
		if r.State != "preparing" || active != key || epoch != r.SlotEpoch || plan != "" {
			return preparationRecord{}, errChanged
		}
		if err = tx.Commit(); err != nil {
			return preparationRecord{}, err
		}
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return preparationRecord{}, err
	}
	if active != "" || plan != "" {
		return preparationRecord{}, errBusy
	}
	captures, _ := json.Marshal(map[string]preparationCapture{"candidate": {}, "rollback": {}})
	_, err = tx.ExecContext(ctx, `INSERT INTO installation_preparations(preparation_id,installation_id,request_id,requested_by,source_run_id,slot_epoch,scope,captures,state) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,'preparing')`, key, scope.InstallationID, scope.RequestID, actor, preparationSourceRun(scope.InstallationID, scope.RequestID, actor), epoch+1, string(body), string(captures))
	if err != nil {
		return preparationRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_preparation_id=$2,slot_epoch=$3 WHERE installation_id=$1`, scope.InstallationID, key, epoch+1)
	if err != nil {
		return preparationRecord{}, err
	}
	r, err := readPreparation(ctx, tx, scope.InstallationID, key)
	if err != nil {
		return preparationRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return preparationRecord{}, err
	}
	return r, nil
}

func readPreparation(ctx context.Context, tx *sql.Tx, installation, key string) (preparationRecord, error) {
	r := preparationRecord{ID: key}
	var scope, captures []byte
	var request, actor, run string
	var promoted sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT scope,captures,slot_epoch,state,request_id,requested_by,source_run_id,promoted_plan_id FROM installation_preparations WHERE installation_id=$1 AND preparation_id=$2 FOR UPDATE`, installation, key).Scan(&scope, &captures, &r.SlotEpoch, &r.State, &request, &actor, &run, &promoted)
	if err != nil {
		return preparationRecord{}, err
	}
	var digest string
	r.Scope, digest, err = decodePreparationScope(scope)
	r.PromotedPlanID = promoted.String
	if err != nil || digest != key || r.Scope.InstallationID != installation || r.Scope.RequestID != request || r.Scope.RequestedBy != actor || run != preparationSourceRun(installation, request, actor) || r.SlotEpoch < 1 || (r.State != "preparing" && r.State != "cancelled" && r.State != "promoted") {
		return preparationRecord{}, errors.New("installation preparation identity is inconsistent")
	}
	if (r.State == "promoted") != promoted.Valid || (promoted.Valid && !internalDigest.MatchString(promoted.String)) {
		return preparationRecord{}, errors.New("installation preparation promotion marker is inconsistent")
	}
	if len(captures) > 64<<10 {
		return preparationRecord{}, errors.New("preparation capture records exceed their bound")
	}
	d := json.NewDecoder(bytes.NewReader(captures))
	d.DisallowUnknownFields()
	if d.Decode(&r.Captures) != nil || len(r.Captures) != 2 {
		return preparationRecord{}, errors.New("preparation capture records are malformed")
	}
	encoded, _ := json.Marshal(r.Captures)
	a, ea := canonicalJSON(captures)
	b, eb := canonicalJSON(encoded)
	if ea != nil || eb != nil || !bytes.Equal(a, b) {
		return preparationRecord{}, errors.New("preparation capture records are noncanonical")
	}
	for role, spec := range r.Scope.Captures {
		c, exists := r.Captures[role]
		if !exists || (!c.Started && (c.Receipt != nil || c.SourceDigest != "" || c.ReceiptDigest != "" || c.Acknowledged)) || (r.State == "cancelled" && c.Started) {
			return preparationRecord{}, errors.New("preparation capture markers are inconsistent")
		}
		capture, err := newSourceCapture(spec)
		if err != nil {
			return preparationRecord{}, err
		}
		if c.Receipt != nil {
			if _, err = capture.reference(*c.Receipt); err != nil {
				return preparationRecord{}, err
			}
		}
		if (c.SourceDigest != "" || c.ReceiptDigest != "") && (c.Receipt == nil || !internalDigest.MatchString(c.SourceDigest) || !internalDigest.MatchString(c.ReceiptDigest)) {
			return preparationRecord{}, errors.New("preparation source proof is incomplete")
		}
		if c.Acknowledged && c.SourceDigest == "" {
			return preparationRecord{}, errors.New("unverified source Job was acknowledged")
		}
		if r.State == "promoted" && !c.Acknowledged {
			return preparationRecord{}, errors.New("promoted preparation has unfinished source evidence or cleanup")
		}
	}
	return r, nil
}

// The shared installation head is always locked first. It fences final Argo
// attempts as well as preparation, including late callbacks on another node.
func (j *preparationJournal) withRecord(ctx context.Context, installation, key, workflow string, update func(*preparationRecord) error) (preparationRecord, error) {
	actor, err := preparationActor(ctx)
	if err != nil {
		return preparationRecord{}, err
	}
	if !identifier.MatchString(installation) || !internalDigest.MatchString(key) {
		return preparationRecord{}, errors.New("exact preparation identities required")
	}
	db, err := j.database()
	if err != nil {
		return preparationRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return preparationRecord{}, err
	}
	defer tx.Rollback()
	plan, active, epoch, err := lockPreparationHead(ctx, tx, installation)
	if err != nil {
		return preparationRecord{}, err
	}
	r, err := readPreparation(ctx, tx, installation, key)
	if err != nil {
		return preparationRecord{}, err
	}
	if r.State == "preparing" && (plan != "" || active != key || epoch != r.SlotEpoch) {
		return preparationRecord{}, errChanged
	}
	if r.State == "promoted" {
		if _, err = promotedRevision(ctx, tx, r); err != nil {
			return preparationRecord{}, err
		}
	}
	if update != nil {
		if r.State != "preparing" || actor != r.Scope.RequestedBy || workflow != r.Scope.WorkflowDigest {
			return preparationRecord{}, errors.New("preparation effect authority or workflow changed")
		}
		if err = update(&r); err != nil {
			return preparationRecord{}, err
		}
		body, err := json.Marshal(r.Captures)
		if err != nil {
			return preparationRecord{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_preparations SET captures=$2::jsonb,state=$3,updated_at=clock_timestamp() WHERE preparation_id=$1`, key, string(body), r.State)
		if err != nil {
			return preparationRecord{}, err
		}
		if r.State == "cancelled" {
			_, err = tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_preparation_id=NULL WHERE installation_id=$1 AND active_preparation_id=$2 AND slot_epoch=$3`, installation, key, epoch)
			if err != nil {
				return preparationRecord{}, err
			}
		}
		r, err = readPreparation(ctx, tx, installation, key)
		if err != nil {
			return preparationRecord{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return preparationRecord{}, err
	}
	return r, nil
}

func (j *preparationJournal) get(ctx context.Context, installation, key string) (preparationRecord, error) {
	return j.withRecord(ctx, installation, key, "", nil)
}

// Recover the original native scope before resolving new configuration or a
// new capture start time. Request identities cannot be recycled, including
// after cancellation. A concurrent first reservation is reconciled by reserve.
func (j *preparationJournal) getByRequest(ctx context.Context, installation, request string) (preparationRecord, error) {
	if _, err := preparationActor(ctx); err != nil {
		return preparationRecord{}, err
	}
	if !identifier.MatchString(installation) || !identifier.MatchString(request) {
		return preparationRecord{}, errors.New("exact installation and preparation request identities required")
	}
	db, err := j.database()
	if err != nil {
		return preparationRecord{}, err
	}
	var key string
	if err := db.QueryRowContext(ctx, `SELECT preparation_id FROM installation_preparations WHERE installation_id=$1 AND request_id=$2`, installation, request).Scan(&key); err != nil {
		return preparationRecord{}, err
	}
	// The lookup is only an address. get rereads the complete native scope and
	// shared head under their locks; this query cannot confer effect authority.
	r, err := j.get(ctx, installation, key)
	if err != nil {
		return preparationRecord{}, err
	}
	if r.Scope.RequestID != request {
		return preparationRecord{}, errChanged
	}
	return r, nil
}

// Started records dispatch intent, not proof that the receiver created a Job.
// A crash after this commit but before dispatch stays uncertain on recovery
// unless the receiver has a durable queued attempt or outcome. Only fenced
// retirement may release that slot; this marker never permits fresh replay.
func (j *preparationJournal) beginCapture(ctx context.Context, installation, key, workflow, role string) (preparationRecord, bool, error) {
	recoverOnly := true
	r, err := j.withRecord(ctx, installation, key, workflow, func(r *preparationRecord) error {
		c, exists := r.Captures[role]
		if !exists {
			return errors.New("source role is outside this preparation")
		}
		recoverOnly = c.Started
		c.Started = true
		r.Captures[role] = c
		return nil
	})
	if err != nil {
		return preparationRecord{}, true, err
	}
	return r, recoverOnly, nil
}

func (j *preparationJournal) recordReceipt(ctx context.Context, installation, key, workflow, role string, receipt sourceCaptureReceipt) (preparationRecord, error) {
	return j.withRecord(ctx, installation, key, workflow, func(r *preparationRecord) error {
		c, exists := r.Captures[role]
		if !exists || !c.Started {
			return errors.New("source capture has no committed dispatch intent")
		}
		capture, err := newSourceCapture(r.Scope.Captures[role])
		if err != nil {
			return err
		}
		if _, err = capture.reference(receipt); err != nil {
			return err
		}
		if c.Receipt != nil && *c.Receipt != receipt {
			return errors.New("source capture already recorded a different receipt")
		}
		c.Receipt = &receipt
		r.Captures[role] = c
		return nil
	})
}

func (j *preparationJournal) cancelBeforeCapture(ctx context.Context, installation, key, workflow string) (preparationRecord, error) {
	return j.withRecord(ctx, installation, key, workflow, func(r *preparationRecord) error {
		for _, capture := range r.Captures {
			if capture.Started {
				return errors.New("source effects may have started; reconcile owned cleanup before releasing preparation")
			}
		}
		r.State = "cancelled"
		return nil
	})
}
