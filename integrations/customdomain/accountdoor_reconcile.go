package customdomain

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/frontdoor"
	"github.com/znasllc-io/memql/core/id"
)

// accountdoor_reconcile.go -- the sweep that walks an account's reserved front
// door from a verified reservation to three hosts that answer (epic
// memql#5168, design D).
//
// The state machine, and the one arrow that is not the custom-domain one:
//
//	                     a verified account with a reserved name and no door
//	                                        |
//	                                        v
//	  pending_dns --all three CNAMEs here--> issuing --cert Ready--> live
//	       ^                                    |                     |
//	       +---------- verifying <--------------+                     |
//	                                                                  |
//	  <any> --the reservation went away (D9)--> removing --unbind--> removed
//	                                                ^                 (terminal)
//	                                                |
//	                              THIS is the only arrow out of `live`
//
// # ONE STATE PER PASS
//
// The custom-domain discipline, and the reason is the same: the row is the
// state machine. Writing `issuing` and then acting on it in the same pass
// means the dispatch is driven by a local variable rather than by a row every
// replica can see, and two replicas running the sweep would both act on a door
// having each written it. Letting the next pass pick it up makes the row the
// only coordination point there is.
//
// # POINTING ONLY, NO SECOND OWNERSHIP CHECK
//
// Record A's domain walk already proved the parent domain with a TXT token,
// and `memqlReservedAt` is stamped only when that succeeded AND the name
// passed the guardrails. `memql.acme.com` is under `acme.com`, so asking an
// operator to prove the same thing twice would buy nothing and cost a second
// record to publish.

// DoorReconciler walks account front doors.
type DoorReconciler struct {
	doors    *DoorStore
	accounts *DoorAccountReader
	resolver Resolver

	provisioner DoorProvisioner
	logger      *slog.Logger

	// apiPaths is the generated HTTP path list the api. host routes. Held on
	// the reconciler rather than read per pass so a test can prove the
	// provisioner is handed the same slice component/frontdoor holds.
	apiPaths []string

	// edgeHost is the hostname all three of a door's names CNAME to. ONE
	// target for three names, because one ingress controller terminates all
	// three and routes by Host.
	edgeHost   string
	acmeIssuer string
	namespace  string

	ingressClass     string
	edgeService      string
	edgePort         int
	bffHTTPService   string
	bffHTTPPort      int
	bffGRPCService   string
	bffGRPCPort      int
	agentGRPCService string
	agentGRPCPort    int
	identityService  string
	identityPort     int

	// now is injectable so a test can assert the exact timestamps written.
	now func() time.Time
	// newID is injectable for the same reason.
	newID func() string
}

// DoorConfig is the reconciler's environment-derived half. Every field is a
// VALUE -- the same flow shape runs on k3d and on AKS and only these differ,
// which is what environment parity means here (custom domains D7).
type DoorConfig struct {
	EdgeHost     string
	ACMEIssuer   string
	Namespace    string
	IngressClass string

	EdgeService      string
	EdgePort         int
	BFFHTTPService   string
	BFFHTTPPort      int
	BFFGRPCService   string
	BFFGRPCPort      int
	AgentGRPCService string
	AgentGRPCPort    int
	IdentityService  string
	IdentityPort     int
}

// NewDoorReconciler builds the production reconciler.
func NewDoorReconciler(doors *DoorStore, accounts *DoorAccountReader, cfg DoorConfig, provisioner DoorProvisioner, logger *slog.Logger) *DoorReconciler {
	return &DoorReconciler{
		doors:       doors,
		accounts:    accounts,
		resolver:    NewSystemResolver(),
		provisioner: provisioner,
		logger:      logger,
		// FROM component/frontdoor, NOT a list of its own. cmd/frontdoorpaths
		// writes that slice from the same collect() that fills the cluster's
		// own api Ingress, so a door routes what the cluster routes by
		// construction rather than by anybody remembering to update two lists.
		apiPaths:         frontdoor.BFFHTTPPaths,
		edgeHost:         cfg.EdgeHost,
		acmeIssuer:       cfg.ACMEIssuer,
		namespace:        cfg.Namespace,
		ingressClass:     cfg.IngressClass,
		edgeService:      cfg.EdgeService,
		edgePort:         cfg.EdgePort,
		bffHTTPService:   cfg.BFFHTTPService,
		bffHTTPPort:      cfg.BFFHTTPPort,
		bffGRPCService:   cfg.BFFGRPCService,
		bffGRPCPort:      cfg.BFFGRPCPort,
		agentGRPCService: cfg.AgentGRPCService,
		agentGRPCPort:    cfg.AgentGRPCPort,
		identityService:  cfg.IdentityService,
		identityPort:     cfg.IdentityPort,
		now:              func() time.Time { return time.Now().UTC() },
		newID:            id.NewShortId,
	}
}

// DoorPassResult is what one sweep did, for the automation's step result and
// the log line.
type DoorPassResult struct {
	Opened   int `json:"opened"`
	Checked  int `json:"checked"`
	Verified int `json:"verified"`
	Issued   int `json:"issued"`
	Removed  int `json:"removed"`
	Demoted  int `json:"demoted"`
	Failed   int `json:"failed"`
}

// Run performs one reconciliation pass: open doors for reservations that have
// none, then advance every door that is not settled.
//
// A per-row failure never aborts the pass, Run's reasoning one concept over:
// one client whose DNS provider is timing out must not stop another client's
// certificate from being noticed Ready.
func (r *DoorReconciler) Run(ctx context.Context) (DoorPassResult, error) {
	var out DoorPassResult
	if r == nil || r.doors == nil {
		return out, fmt.Errorf("customdomain: door reconciler has no store")
	}

	selected := map[string]Door{}
	var reservationErr error
	_, err := workflowhost.Run(ctx, "accountDoorReconcileWorkflow", nil, workflowhost.Options{Logger: r.logger, Operations: map[string]workflowhost.Operation{
		"customDomainSyncReservations": func(ctx context.Context, _ map[string]any) (any, error) {
			reservationErr = r.open(ctx, &out)
			if reservationErr != nil {
				out.Failed++
				r.warn("could not read account reservations", "error", reservationErr)
			}
			return nil, reservationErr
		},
		"customDomainReadDoors": func(ctx context.Context, _ map[string]any) (any, error) {
			doors, err := r.doors.ToReconcile(ctx)
			if err != nil {
				return nil, err
			}
			facts := []any{}
			for _, d := range doors {
				selected[d.ID] = d
				facts = append(facts, map[string]any{"id": d.ID, "status": d.Status})
			}
			return facts, nil
		},
		"customDomainReconcileDoor": func(ctx context.Context, a map[string]any) (any, error) {
			id, _ := a["doorId"].(string)
			d, ok := selected[id]
			if !ok {
				return nil, fmt.Errorf("door outside reconciliation snapshot")
			}
			if !NonTerminal(d.Status) {
				return nil, nil
			}
			out.Checked++
			err := r.step(ctx, d, &out)
			if err != nil {
				out.Failed++
				r.warn("account front door reconciliation step failed", "reservedName", d.ReservedName, "error", err)
			}
			return nil, err
		},
		"customDomainRequireReservations": func(context.Context, map[string]any) (any, error) {
			if reservationErr != nil {
				return nil, fmt.Errorf("customdomain: account front door pass could not read reservations, so no door was opened or torn down: %w", reservationErr)
			}
			return nil, nil
		},
	}})
	return out, err
}

// open creates a door for every held reservation that has none, and asks for
// the teardown of every live door whose reservation has gone away (design D9).
//
// BOTH DIRECTIONS ARE DRIVEN FROM THE ACCOUNT SIDE, and that is why `live` is
// excluded from the sweep's own work list. Walking live doors on every tick to
// ask whether their account still wants them would make the steady state of a
// healthy cluster the most expensive one; walking RESERVATIONS instead costs a
// read whose size is the number of accounts, which is the number an operator
// typed.
func (r *DoorReconciler) open(ctx context.Context, out *DoorPassResult) error {
	if r.accounts == nil {
		return nil
	}
	reservations, unheld, err := r.accounts.HeldReservations(ctx)
	if err != nil {
		return err
	}
	held := make(map[string]string, len(reservations))
	for _, res := range reservations {
		held[res.AccountID] = res.ReservedName
		// A HELD NAME CARRIES NO REASON. Clearing it here is what makes the
		// field mean "why it is not held" rather than "why it was not held
		// once"; without it a name that was refused and then fixed would keep
		// explaining a state it is no longer in.
		if res.Reason != "" {
			if err := r.accounts.RecordReservationReason(ctx, res.AccountID, ""); err != nil {
				r.warn("could not clear a reservation reason", "account", res.AccountID, "error", err)
			}
		}
	}

	// THE UNHELD ONES GET A REASON, which is the ask the Accounts rail made:
	// an absent memqlReservedAt meant two different things and the rail
	// inferred which from the ownership stop beside it.
	//
	// ONE VALUE, and that is the whole vocabulary a ROW ever needs. The other
	// four codes are REFUSALS -- the guard rejects the write, so no row is
	// left to carry them, and the operator sees them as an error on the form
	// at the moment they type the name, which is both sooner and more
	// actionable than a row state. A name that is recorded but not held is
	// recorded because its domain is not verified yet.
	for _, res := range unheld {
		if res.Reason == ReasonOwnershipUnproven {
			continue
		}
		if err := r.accounts.RecordReservationReason(ctx, res.AccountID, ReasonOwnershipUnproven); err != nil {
			r.warn("could not record a reservation reason", "account", res.AccountID, "error", err)
		}
	}

	existing, err := r.accounts.LiveAndPendingDoors(ctx)
	if err != nil {
		return err
	}

	// The live re-check rides the rows this read already returned, so it costs
	// no extra query -- and it runs BEFORE the reservation comparison, so a
	// door demoted this pass is still compared against its reservation on the
	// next one rather than being skipped.
	r.recheckLive(ctx, existing, out)

	now := r.now()

	for _, d := range existing {
		want, stillHeld := held[d.AccountID]

		// ONE DOOR AT A TIME PER ACCOUNT, and this line is what enforces it.
		// Every object a door applies is named after the ACCOUNT, not the door
		// -- so opening a second door for an account that still has one would
		// apply the same five object names, overwrite the first door's
		// Ingresses and certificate, and then have them deleted underneath it
		// by the first door's unbind. The account is dropped from the work
		// list here, before any branch below decides what to do with the door
		// it already has; the next pass opens the replacement once this one
		// has reached `removed`.
		delete(held, d.AccountID)

		switch {
		case !stillHeld:
			// THE RESERVATION IS GONE. Changing an account's domain clears
			// memqlDomain, the ownership token and the verification (record
			// A's resetAccountDomainOnChange), and archiving an account takes
			// it out of the walk. Either way the cluster is serving three
			// names whose ownership proof has been discarded, which is the
			// state this arrow exists to end.
			if d.Status == StatusRemoving {
				continue
			}
			if err := r.doors.RequestRemoval(ctx, d.ID, ReasonReservationGone,
				"the account no longer holds the reserved name "+d.ReservedName, now); err != nil {
				r.warn("could not request front door removal", "reservedName", d.ReservedName, "error", err)
				continue
			}
			r.info("account front door reservation withdrawn", "reservedName", d.ReservedName, "account", d.AccountID)
		case want != d.ReservedName:
			// The reservation MOVED. Same conclusion for the same reason:
			// proof of one name is not proof of another. The next pass opens a
			// door for the new name once this one is gone.
			if d.Status == StatusRemoving {
				continue
			}
			if err := r.doors.RequestRemoval(ctx, d.ID, ReasonReservationGone,
				fmt.Sprintf("the reserved name changed from %s to %s", d.ReservedName, want), now); err != nil {
				r.warn("could not request front door removal", "reservedName", d.ReservedName, "error", err)
				continue
			}
			r.info("account front door reservation changed", "from", d.ReservedName, "to", want, "account", d.AccountID)
		default:
			// A door already exists for this reservation and matches it. The
			// step loop owns it from here.
		}
	}

	for accountID, name := range held {
		door := Door{ID: r.newID(), AccountID: accountID, ReservedName: name}
		if err := r.doors.Create(ctx, door); err != nil {
			r.warn("could not open an account front door", "reservedName", name, "error", err)
			continue
		}
		out.Opened++
		r.info("account front door opened", "reservedName", name, "account", accountID)
	}
	return nil
}

// LiveRecheckInterval is how often a LIVE door's three names are looked up
// again, and DriftDemotionThreshold is how many consecutive failures demote it.
//
// # WHY A LIVE DOOR IS RE-CHECKED AT ALL
//
// Because going live is not a permanent fact about DNS. A door whose `app.`
// host is repointed after it goes live keeps serving -- and keeps its callback
// registered as an OAuth redirect URI for the OS client. A review found what
// that composes into: repoint `app.<reservedName>` at a server you control,
// send somebody a sign-in link at this cluster naming that callback, and the
// authorization code is delivered to your server through a consent page
// showing this platform's own name and logo. The redirect allowlist is the
// control that is supposed to make that impossible, and a stale one is not a
// control.
//
// # WHY A COUNTER AND NOT AN IMMEDIATE DEMOTION
//
// Demoting on one failed lookup would take a client's whole front door down
// for a resolver hiccup, and the blast radius of a false positive here is
// every one of their people. Three consecutive failures at fifteen minutes is
// forty-five minutes of SUSTAINED drift -- long enough that a transient
// failure cannot reach it, short enough that the exposure window is bounded
// and small.
const (
	LiveRecheckInterval    = 15 * time.Minute
	DriftDemotionThreshold = 3
)

// recheckLive re-verifies the doors that are already serving.
//
// It runs over the rows `open()` already read, so it costs no extra query --
// only the DNS lookups, and only for doors whose last check is older than the
// interval.
func (r *DoorReconciler) recheckLive(ctx context.Context, doors []Door, out *DoorPassResult) {
	now := r.now()
	for _, d := range doors {
		if d.Status != StatusLive || !r.dueForRecheck(d, now) {
			continue
		}

		checks := make(map[string]HostCheck, len(frontdoor.AccountRoles()))
		allOK := true
		for _, h := range frontdoor.AccountHosts(d.ReservedName) {
			res := CheckPointing(ctx, r.resolver, h.Name, r.edgeHost)
			checks[string(h.Role)] = HostCheck{OK: res.OK, Reason: res.Reason, Detail: res.Detail}
			if !res.OK {
				allOK = false
			}
		}

		if allOK {
			// PASSING RESETS THE COUNT rather than leaving it. Drift means
			// CONSECUTIVE failures; a door that fails twice, recovers, and
			// fails once more has not drifted for forty-five minutes.
			if err := r.doors.RecordDrift(ctx, d.ID, checks, 0, "", "", now); err != nil {
				r.warn("could not record a passing re-check", "reservedName", d.ReservedName, "error", err)
			}
			continue
		}

		failures := d.DriftFailures + 1
		if failures < DriftDemotionThreshold {
			if err := r.doors.RecordDrift(ctx, d.ID, checks, failures, ReasonNotPointing, pointingDetail(checks), now); err != nil {
				r.warn("could not record a failing re-check", "reservedName", d.ReservedName, "error", err)
			}
			continue
		}

		// SUSTAINED DRIFT. Back to `verifying`, which stops the edge resolving
		// the app. host AND drops the door out of the identity node's live set
		// -- so its callback stops being a registered redirect URI in the same
		// write. It re-verifies on its own if the records come back; the
		// certificate is still there, so recovery is fast.
		if err := r.doors.RecordCheck(ctx, d.ID, StatusVerifying, checks, ReasonNotPointing, pointingDetail(checks), now); err != nil {
			r.warn("could not demote a drifted door", "reservedName", d.ReservedName, "error", err)
			continue
		}
		out.Demoted++
		r.info("account front door demoted: its names stopped pointing here",
			"reservedName", d.ReservedName, "account", d.AccountID, "consecutiveFailures", failures)
	}
}

// dueForRecheck is true when a live door has not been looked at within the
// interval. A door with NO recorded check is due immediately -- absent is
// "never looked", not "looked recently".
func (r *DoorReconciler) dueForRecheck(d Door, now time.Time) bool {
	if d.LastCheckedAt == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339, d.LastCheckedAt)
	if err != nil {
		// An unparseable timestamp is not a licence to skip the check: the
		// whole point is that a live door must not go unexamined forever.
		return true
	}
	return now.Sub(at) >= LiveRecheckInterval
}

func (r *DoorReconciler) step(ctx context.Context, d Door, out *DoorPassResult) error {
	switch d.Status {
	case StatusPendingDNS, StatusVerifying:
		return r.verify(ctx, d, out)
	case StatusIssuing:
		return r.provision(ctx, d, out)
	case StatusRemoving:
		return r.unprovision(ctx, d, out)
	default:
		return nil
	}
}

// verify runs CheckPointing against all three hosts and promotes only when all
// three pass.
//
// EVERY HOST IS CHECKED ON EVERY PASS, even after the first miss. Stopping at
// the first failure would be one fewer DNS lookup and would make the rail say
// "app. is not pointing here" while api. and id. were equally wrong -- so an
// operator would fix one record, wait two minutes, and be told about the next
// one. Three lookups is what it costs to give them the whole list at once.
func (r *DoorReconciler) verify(ctx context.Context, d Door, out *DoorPassResult) error {
	now := r.now()
	checks := make(map[string]HostCheck, len(frontdoor.AccountRoles()))
	allOK := true

	for _, h := range frontdoor.AccountHosts(d.ReservedName) {
		res := CheckPointing(ctx, r.resolver, h.Name, r.edgeHost)
		checks[string(h.Role)] = HostCheck{OK: res.OK, Reason: res.Reason, Detail: res.Detail}
		if !res.OK {
			allOK = false
		}
	}

	if !allOK {
		return r.doors.RecordCheck(ctx, d.ID, StatusVerifying, checks,
			ReasonNotPointing, pointingDetail(checks), now)
	}

	// ALL THREE PASSED. This is the ONLY line in the tree that reaches
	// `issuing` for a door.
	if err := r.doors.MarkVerified(ctx, d.ID, checks, now); err != nil {
		return err
	}
	out.Verified++
	r.info("account front door verified", "reservedName", d.ReservedName, "account", d.AccountID)
	return nil
}

// provision applies the five objects, idempotently, and promotes to `live`
// only when the one certificate is Ready -- which it cannot be unless all
// three hosts solved HTTP-01 (design D8).
func (r *DoorReconciler) provision(ctx context.Context, d Door, out *DoorPassResult) error {
	now := r.now()
	res, err := r.provisioner.BindDoor(ctx, r.request(d), r.apiPaths)
	if err != nil {
		// The substrate could not RUN -- an unregistered script id, a missing
		// backend, no envelope on stdout. An engine-side fault rather than a
		// statement about this door, so it is recorded as issuance_failed with
		// the reason.
		return r.doors.RecordIssuanceFailure(ctx, d.ID, ReasonIssuanceFailed, err.Error(), now)
	}
	if res.Reason != "" {
		return r.doors.RecordIssuanceFailure(ctx, d.ID, res.Reason, res.Detail, now)
	}
	if !res.CertificateReady {
		// APPLIED, NOT YET READY -- the ordinary state for the first minutes of
		// a three-name order, and deliberately not recorded as a failure.
		// `issuing` with no failureReason is exactly "we asked and are
		// waiting"; a reason here would make a normal wait look like a problem.
		return r.doors.RecordIssuingProgress(ctx, d.ID, res.Note, now)
	}
	if err := r.doors.MarkLive(ctx, d.ID, now); err != nil {
		return err
	}
	out.Issued++
	r.info("account front door live", "reservedName", d.ReservedName, "account", d.AccountID)
	return nil
}

// unprovision removes the five objects and closes the walk.
//
// THE THREE HOSTS HAVE ALREADY STOPPED RESOLVING by the time this runs:
// `liveAccountFrontDoorByReservedName` filters `status=="live"`, so serving
// stopped at the write that set `removing`. This is the cleanup behind that
// decision, and its failure is loud but not urgent.
func (r *DoorReconciler) unprovision(ctx context.Context, d Door, out *DoorPassResult) error {
	now := r.now()
	res, err := r.provisioner.UnbindDoor(ctx, r.request(d))
	if err != nil {
		return r.doors.RecordRemovalFailure(ctx, d.ID, ReasonIssuanceFailed, err.Error(), now)
	}
	if res.Reason != "" {
		return r.doors.RecordRemovalFailure(ctx, d.ID, res.Reason, res.Detail, now)
	}
	// APPLIED IS THE PROMOTION GATE, and reading only `err` and `Reason` was
	// the defect: an envelope reporting applied:false with no reason closed the
	// walk while the objects were still serving, and the row is the only thing
	// anyone reads afterwards. Design D step 5 says `removing` -> `removed`
	// when the envelope SAYS APPLIED.
	if !res.Applied {
		return r.doors.RecordRemovalFailure(ctx, d.ID, ReasonIssuanceFailed,
			"the unbind reported no failure and did not report the objects as removed", now)
	}
	if err := r.doors.MarkRemoved(ctx, d.ID, now); err != nil {
		return err
	}
	out.Removed++
	r.info("account front door removed", "reservedName", d.ReservedName, "account", d.AccountID)
	return nil
}

// request composes the substrate-independent description of one door's
// objects.
func (r *DoorReconciler) request(d Door) DoorBindRequest {
	return DoorBindRequest{
		AccountID:        d.AccountID,
		DoorID:           d.ID,
		ReservedName:     d.ReservedName,
		Namespace:        r.namespace,
		Issuer:           r.acmeIssuer,
		IngressClass:     r.ingressClass,
		EdgeService:      r.edgeService,
		EdgePort:         r.edgePort,
		BFFHTTPService:   r.bffHTTPService,
		BFFHTTPPort:      r.bffHTTPPort,
		BFFGRPCService:   r.bffGRPCService,
		BFFGRPCPort:      r.bffGRPCPort,
		AgentGRPCService: r.agentGRPCService,
		AgentGRPCPort:    r.agentGRPCPort,
		IdentityService:  r.identityService,
		IdentityPort:     r.identityPort,
	}
}

// pointingDetail composes the sentence beside the typed reason: which of the
// three hosts is still wrong, and what was seen there.
//
// THE ROLES ARE WALKED IN frontdoor.AccountRoles ORDER rather than by ranging
// the map, so the sentence is stable between passes. A detail string that
// reordered itself every two minutes would fingerprint as a change and make
// the rail flicker for a door that had not moved.
func pointingDetail(checks map[string]HostCheck) string {
	var out string
	for _, role := range frontdoor.AccountRoles() {
		c, ok := checks[string(role)]
		if !ok || c.OK {
			continue
		}
		if out != "" {
			out += "; "
		}
		out += string(role) + ": " + c.Detail
	}
	return out
}

// ReasonReservationGone is the typed reason a door is torn down: the account
// no longer holds the name it was serving (design D9).
//
// A REASON OF ITS OWN rather than one of the DNS ones, because it is not a
// statement about DNS at all and the repair is somewhere else entirely -- an
// operator looks at the account's domain, not at their zone file.
const ReasonReservationGone = "reservation_withdrawn"

func (r *DoorReconciler) info(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Info(msg, append([]any{"component", "accountFrontDoor"}, args...)...)
	}
}

func (r *DoorReconciler) warn(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Warn(msg, append([]any{"component", "accountFrontDoor"}, args...)...)
	}
}
