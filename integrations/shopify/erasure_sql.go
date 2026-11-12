package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// erasure_sql.go -- the statements of a customer's erasure: the session they
// run on, the scope they find (erasureScopeOf), and the scrub they make of it
// (scrubRows, takeOut). Moved here from compliance.go, which keeps the jobs,
// the runner and the purge. What each field of a row is left as is the
// policy's (erasure_fields.go); this file is where the policy meets the table.

// erasedRow is one row a customer's erasure reaches.
type erasedRow struct {
	// ID is the row's id as the table holds it; GID the origin's.
	ID, GID string
}

// erasureScope is what one customer's erasure covers in one store: the rows
// it scrubs, which are the rows a data request exports, and the rows outside
// them that name the customer in an entry that is taken out.
type erasureScope struct {
	// Rows are the rows in scope, by concept, in the order they were found.
	Rows map[string][]erasedRow
	// Customers are the gids of the customer rows in scope: the erased
	// customer's, whether or not the mirror holds its row, and those of the
	// earlier records merged into it.
	Customers []string
	// Alone are deleted customer rows whose last order is in scope, by
	// concept. Each is scrubbed whole and BY ITSELF (ruling 98): it is not a
	// seed, nothing is reached from it, and it is not in an export. The link
	// cannot tell a record merged into the customer from another deleted
	// person whose stale pointer names an order that is hers now.
	Alone map[string][]erasedRow
	// Earlier are versions of rows OUTSIDE the scope that hold one of
	// Customers in a link field, by concept: the row was hers once and is
	// not now. Each is scrubbed by itself, and nothing else of its row.
	Earlier map[string][]rowVersion
	// TakenFrom are the ids of rows OUTSIDE the scope that name one of
	// Customers in a field an entry is taken out of (erasureTakeOutsOf), by
	// concept. The entry goes; nothing else in such a row is about her.
	TakenFrom map[string][]string
}

// rowVersion names one version of one row: the row's id and the version's
// time, as the table spells it.
type rowVersion struct {
	ID string `json:"id"`
	At string `json:"at"`
}

// count is how many rows the scope holds.
func (s *erasureScope) count() int {
	n := 0
	for _, rows := range s.Rows {
		n += len(rows)
	}
	return n
}

// erasureBatch is how many gids or row ids one statement of the scope or the
// scrub names. They travel as one JSON array parameter, of half a megabyte or
// so at this size; a smaller batch is another read of the concept's rows.
const erasureBatch = 10000

// privacyStatementLimit is how long the server lets one statement of a
// privacy job run, and privacyStatementGrace how much later the client gives
// up on it. The limit is the server's, so a statement that overruns is rolled
// back whole and says so, rather than left running behind a closed socket.
//
// Ten minutes is long beside what a statement here does on a store of a few
// million rows (a read of one concept's rows of one store, a rewrite of rows
// named by id) and short beside the hour between two runs.
const (
	privacyStatementLimit = 10 * time.Minute
	privacyStatementGrace = 15 * time.Second
)

// privacySession is the ONE database session a privacy job's statements run
// on: the engine's maintenance session (memql.PluginContext's
// OpenMaintenanceSession), which has no server statement_timeout of its own
// and no client read deadline beyond the caller's context.
//
// NOT THE REQUEST POOL. Its connections carry an eight-second statement limit
// and a ten-second read deadline, and a statement here reads every row of a
// concept for a store: on a large store the scope would be cancelled part-way
// at every run, and the erasure would never be made. There is no falling back
// to the pool either, for the same reason the engine's other long passes have
// none: a pass that half ran is worse than one that says it cannot run.
type privacySession struct {
	conn *sql.Conn
}

// WithMaintenanceSession attaches the opener of the engine's maintenance
// session, which the privacy jobs run their statements on (privacySession).
func (c *Connector) WithMaintenanceSession(open func(context.Context) (*sql.Conn, func(), error)) *Connector {
	c.maintain = open
	return c
}

// openPrivacySession opens the session for one run of the privacy jobs, or
// one call of an erasure, and bounds every statement on it. The caller closes
// it with the func it answers.
func (c *Connector) openPrivacySession(ctx context.Context, what string) (*privacySession, func(), error) {
	if c.maintain == nil {
		return nil, nil, fmt.Errorf("shopify: %s needs the engine's maintenance session, and this connector was given none; it does not run on the request pool instead, whose statement limit would end it part-way", what)
	}
	conn, done, err := c.maintain(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("shopify: %s could not open the maintenance session: %w", what, err)
	}
	if conn == nil || done == nil {
		return nil, nil, fmt.Errorf("shopify: %s was given no maintenance session", what)
	}
	sctx, cancel := context.WithTimeout(ctx, privacyStatementGrace)
	defer cancel()
	if _, err := conn.ExecContext(sctx, "SET statement_timeout = "+strconv.FormatInt(privacyStatementLimit.Milliseconds(), 10)); err != nil {
		done()
		return nil, nil, fmt.Errorf("shopify: %s could not bound its statements: %w", what, err)
	}
	return &privacySession{conn: conn}, done, nil
}

// alive reports whether the session still answers: the server ends one that
// has been idle past its limit, and a connection it ended does not say so
// until it is used.
func (s *privacySession) alive(ctx context.Context) bool {
	sctx, cancel := context.WithTimeout(ctx, privacyStatementGrace)
	defer cancel()
	var one int
	return s.conn.QueryRowContext(sctx, "SELECT 1").Scan(&one) == nil
}

// rows runs a read whose every column is text, or NULL read as "".
func (s *privacySession) rows(ctx context.Context, query string, args ...any) ([][]string, error) {
	sctx, cancel := context.WithTimeout(ctx, privacyStatementLimit+privacyStatementGrace)
	defer cancel()
	return queryRows(sctx, s.conn, query, args...)
}

// exec runs a write and answers how many rows it changed.
func (s *privacySession) exec(ctx context.Context, query string, args ...any) (int64, error) {
	sctx, cancel := context.WithTimeout(ctx, privacyStatementLimit+privacyStatementGrace)
	defer cancel()
	res, err := s.conn.ExecContext(sctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// mirrorRowKey is a mirror row's id as the table holds it. It is made of the
// row's concept, its store and its gid, so a row is read by it without
// looking through a concept's rows -- and a row of another store that carries
// the same gid has another id.
func mirrorRowKey(concept, storeID, gid string) string {
	return generated.ConceptID(concept) + ":" + MirrorRowID(storeID, gid)
}

// A customer's gid and an order's, as the origin spells them: the type and a
// number, and nothing else. A privacy request is refused at the door for an
// id of any other form (parseComplianceJob), and the scope refuses one too:
// its statements name rows by id and match the gid as text.
var (
	customerGIDForm = regexp.MustCompile(`^gid://shopify/Customer/[0-9]+$`)
	orderGIDForm    = regexp.MustCompile(`^gid://shopify/Order/[0-9]+$`)
)

// erasureScopeOf finds what the erasure of one customer covers in one store
// (rulings 78 and 91). It is the ONE function the erasure and the data request
// both use, so an export and an erasure cannot disagree about what MemQL
// holds on a person.
//
// THE SCOPE starts at the customer's own row and the orders the request names
// (orders_to_redact, orders_requested), each read by its row id, and grows to
// a fixed point by the links of erasureLinksOf, and by no other. That list is
// THE enumeration: the golden file is rendered from the same one, so what it
// says an erasure reaches is what these statements look for. A link is one of
// three things:
//
//   - a row that hangs from an in-scope row by the model's parent link: an
//     order's lines, transactions, refunds, fulfilments and returns, the
//     customer's own payment methods;
//   - a row that holds an in-scope row's gid as the WHOLE value of a gid field
//     that is a link: every row "of" the customer, every row of an order that
//     does not hang from it. A gid field on erasureNotALink is not followed,
//     and one of them (a customer's last order) is followed only from a row
//     whose newest version is deleted: a customer merged into the erased one;
//   - a row that holds an in-scope customer's gid as an id inside one of the
//     object fields of erasureCustomerInside: the object says who the row is
//     for.
//
// NOTHING IS MATCHED BY VALUE. With every gid field a link and the customer's
// gid matched anywhere inside any object, a customer whose row once named the
// erased customer's order as his last order was scrubbed with her and
// exported to her, and so was an order of a third customer that carried her
// gid as the value of a custom attribute. One person's data handed to another
// is the worst thing this code can do.
//
// A ROW THAT ONLY REFERS TO HER IS NOT IN SCOPE. A row outside the scope that
// names her in a field of erasureTakeOutsOf (a merchant's metaobject, a list
// of references that can hold a customer) is answered in TakenFrom: the entry
// that names her is taken out and the row is left.
//
// A gid is matched as a whole value, never as the beginning of a longer one:
// the scrub this replaces matched a substring of the row's text, so erasing
// customer 123 scrubbed every row of customer 1234.
//
// A ROW IS DECIDED ON WHAT IT IS NOW (ruling 94). It is in scope, with
// everything that hangs from it, when its NEWEST version holds the link. An
// order that was hers and has since been moved to another person is that
// person's: with the scope decided on any version, her erasure scrubbed the
// other person's current order and its transaction, and her export, which
// answers each row at its newest version, handed it to her.
//
// AN OLDER VERSION THAT HOLDS HER IS SCRUBBED BY ITSELF. MemoryNodes is keyed
// (id, createdAt), and the versions from when such a row named her are what a
// point-in-time read answers. A version that holds an in-scope customer's gid
// in a link field, on a row whose newest version does not, is answered in
// Earlier: that version is scrubbed, and not the row's newest version, and
// not its dependents.
//
// It reads the table and not the engine, and every row of the store, a
// tombstoned one included: the paged read the export used answered the first
// hundred rows of a concept.
//
// staged-data: MUST-NOT-GATE -- a staged row left out of the scope KEEPS ITS
// PII and is missing from a data request's export. The obligation is that the
// person's data is found wherever it sits, and a visibility gate would leave
// out the one copy nobody can see.
func erasureScopeOf(ctx context.Context, s *privacySession, storeID, customerGID string, orderGIDs []string) (*erasureScope, error) {
	if storeID == "" {
		return nil, fmt.Errorf("shopify: an erasure's scope needs the store's id")
	}
	if !customerGIDForm.MatchString(customerGID) {
		return nil, fmt.Errorf("shopify: the customer's id is not a customer's gid")
	}
	for _, gid := range orderGIDs {
		if !orderGIDForm.MatchString(gid) {
			return nil, fmt.Errorf("shopify: an order's id is not an order's gid")
		}
	}
	scope := &erasureScope{Rows: map[string][]erasedRow{}, Alone: map[string][]erasedRow{}, Earlier: map[string][]rowVersion{}, TakenFrom: map[string][]string{}}
	known := map[string]bool{}
	add := func(concept, id, gid string) bool {
		if id == "" || known[id] {
			return false
		}
		known[id] = true
		scope.Rows[concept] = append(scope.Rows[concept], erasedRow{ID: id, GID: gid})
		return true
	}

	// The frontier: the gids whose holders and children are still to be
	// found, by the concept of the row each is the gid of. It starts with the
	// customer's gid and the orders the request names, whether or not the
	// mirror holds a row of either.
	seen := map[string]bool{}
	alone := map[string]bool{}
	frontier := map[string][]string{}
	reach := func(concept, gid string) {
		if gid == "" || seen[gid] {
			return
		}
		seen[gid] = true
		frontier[concept] = append(frontier[concept], gid)
		if concept == erasedConcept {
			scope.Customers = append(scope.Customers, gid)
		}
	}
	reach(erasedConcept, customerGID)

	// The customer's own row, by its id: every version of it is hers.
	rows, err := s.rows(ctx,
		`SELECT id, max(payload->>'gid') FROM "MemoryNodes"
		  WHERE concept = $1 AND id = $2 AND payload->>'storeId' = $3
		  GROUP BY id`,
		generated.ConceptID(erasedConcept), mirrorRowKey(erasedConcept, storeID, customerGID), storeID)
	if err != nil {
		return nil, fmt.Errorf("shopify: read the %s row a privacy request names: %w", erasedConcept, err)
	}
	for _, row := range rows {
		add(erasedConcept, row[0], row[1])
	}

	// The orders the request names, each by its id, AS THEY ARE NOW (ruling
	// 98). A request is compiled when its orders were the customer's, and a
	// job keeps the orders of every request it was asked; one that has since
	// been moved to another person is that person's order. So a named order
	// is a seed when its newest version names her or names no customer. One
	// that names another customer is not: nothing is reached from it, it is
	// not exported, and its versions from when it named her are found below
	// with those of every other row that moved.
	anothers := map[string]bool{}
	ids := make([]string, 0, len(orderGIDs))
	for _, gid := range orderGIDs {
		ids = append(ids, mirrorRowKey(requestedConcept, storeID, gid))
	}
	for _, batch := range batches(ids) {
		rows, err := s.rows(ctx,
			`SELECT m.id, m.payload->>'gid', COALESCE(NULLIF(m.payload->>'customerGid', ''), m.payload->'purchasingEntity'->>'id', '')
			   FROM "MemoryNodes" m
			  WHERE m.concept = $1 AND m.id IN (SELECT jsonb_array_elements_text($2::jsonb))
			    AND m.payload->>'storeId' = $3 AND NOT `+aNewerVersion,
			generated.ConceptID(requestedConcept), jsonArray(batch), storeID)
		if err != nil {
			return nil, fmt.Errorf("shopify: read the %s rows a privacy request names: %w", requestedConcept, err)
		}
		for _, row := range rows {
			if names := row[2]; names != "" && names != customerGID {
				anothers[row[1]] = true
				continue
			}
			add(requestedConcept, row[0], row[1])
		}
	}
	// A named order the mirror holds no row of is reached from all the same:
	// what hangs from it names nobody else.
	for _, gid := range orderGIDs {
		if !anothers[gid] {
			reach(requestedConcept, gid)
		}
	}

	// The fixed point. A concept's rows are read in a round only for the
	// links that name a concept the round reached, and are asked only the
	// gids of those concepts: a line item's gid is asked of the concepts that
	// can name a line item and not of all of them.
	for len(frontier) > 0 {
		round := frontier
		frontier = map[string][]string{}
		for _, concept := range generated.ApplyOrder {
			spec := generated.Types[concept]
			var always, whenDeleted []string
			var names []string
			for _, link := range erasureLinksOf(spec) {
				if len(round[link.Names]) == 0 {
					continue
				}
				names = append(names, link.Names)
				condition := linkCondition(link)
				if link.FromADeletedRow {
					whenDeleted = append(whenDeleted, condition)
				} else {
					always = append(always, condition)
				}
			}
			if len(always) == 0 && len(whenDeleted) == 0 {
				continue
			}
			for _, batch := range reachedBatches(round, names) {
				if len(always) > 0 {
					rows, err := s.rows(ctx, linkStatement(always, nil), generated.ConceptID(concept), storeID, batch)
					if err != nil {
						return nil, fmt.Errorf("shopify: follow an erasure's links into the %s rows: %w", concept, err)
					}
					for _, row := range rows {
						if add(concept, row[0], row[1]) {
							reach(concept, row[1])
						}
					}
				}
				// A row a deleted-row link finds is scrubbed by itself: it
				// joins neither the scope nor what the next round asks of.
				if len(whenDeleted) > 0 {
					rows, err := s.rows(ctx, linkStatement(nil, whenDeleted), generated.ConceptID(concept), storeID, batch)
					if err != nil {
						return nil, fmt.Errorf("shopify: follow an erasure's links into the deleted %s rows: %w", concept, err)
					}
					for _, row := range rows {
						if !known[row[0]] && !alone[row[0]] {
							alone[row[0]] = true
							scope.Alone[concept] = append(scope.Alone[concept], erasedRow{ID: row[0], GID: row[1]})
						}
					}
				}
			}
		}
	}
	// A row found alone that a later round brought into scope is in scope.
	for concept, rows := range scope.Alone {
		scope.Alone[concept] = slices.DeleteFunc(rows, func(row erasedRow) bool { return known[row.ID] })
	}

	// The versions from when a row that is not hers now was hers: the links
	// that name a customer, asked of the customers in scope.
	hers := reachedBatches(map[string][]string{erasedConcept: scope.Customers}, []string{erasedConcept})
	for _, concept := range generated.ApplyOrder {
		var links []string
		for _, link := range erasureLinksOf(generated.Types[concept]) {
			if link.Names == erasedConcept {
				links = append(links, linkCondition(link))
			}
		}
		if len(links) == 0 {
			continue
		}
		for _, batch := range hers {
			rows, err := s.rows(ctx, earlierStatement(links), generated.ConceptID(concept), storeID, batch)
			if err != nil {
				return nil, fmt.Errorf("shopify: look for the versions of %s rows that were the customer's: %w", concept, err)
			}
			for _, row := range rows {
				if !known[row[0]] && !alone[row[0]] {
					scope.Earlier[concept] = append(scope.Earlier[concept], rowVersion{ID: row[0], At: row[1]})
				}
			}
		}
	}

	// The rows outside the scope that only refer to her.
	for _, concept := range generated.ApplyOrder {
		fields := erasureTakeOutsOf(generated.Types[concept])
		if len(fields) == 0 {
			continue
		}
		for _, customer := range scope.Customers {
			rows, err := s.rows(ctx, takeOutFind(fields),
				generated.ConceptID(concept), storeID, `%"`+likeEscape(customer)+`"%`, jsonObject(map[string]any{"g": customer}))
			if err != nil {
				return nil, fmt.Errorf("shopify: look for the customer in the %s rows that can refer to her: %w", concept, err)
			}
			for _, row := range rows {
				if !known[row[0]] && !alone[row[0]] && !slices.Contains(scope.TakenFrom[concept], row[0]) {
					scope.TakenFrom[concept] = append(scope.TakenFrom[concept], row[0])
				}
			}
		}
	}
	return scope, nil
}

// linkCondition is one link as a condition on a row version m, against the
// gids a round reached (the reached relation of linkStatement: c the concept
// a gid is of, g the gid).
func linkCondition(link erasureLink) string {
	of := "(SELECT g FROM reached WHERE c = " + sqlText(link.Names) + ")"
	switch link.Kind {
	case linkParent:
		return "m.payload->>'parentGid' IN " + of
	case linkObject:
		// An id inside the object, at any depth, and nothing else in it: a
		// message that happens to spell a gid is not who the row is for.
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM jsonb_path_query(m.payload->%s, '$.**.id') AS inside(id) WHERE inside.id #>> '{}' IN %s)`, sqlText(link.Field), of)
	}
	return fmt.Sprintf("m.payload->>%s IN %s", sqlText(link.Field), of)
}

// reachedRelation is what a round reached, as a relation: c the concept a gid
// is of, g the gid ($3, a JSON array of {c, g}).
const reachedRelation = `WITH reached AS (SELECT r.c, r.g FROM jsonb_to_recordset($3::jsonb) AS r(c text, g text))`

// aNewerVersion is true of a version m of a row of the concept $1 when the
// row has a version after it: m is not what the row is now.
//
// staged-data: MUST-NOT-GATE -- with a staged newer version not seen here, an
// older version would read as what the row is now, and a row that is no
// longer the customer's would be scrubbed whole and exported as hers.
const aNewerVersion = `EXISTS (SELECT 1 FROM "MemoryNodes" n WHERE n.concept = $1 AND n.id = m.id AND n."createdAt" > m."createdAt")`

// linkStatement reads the rows of one concept ($1) of one store ($2) that a
// link brings into scope, given what a round reached ($3). A row is found
// when its NEWEST version holds a link (ruling 94); a link that holds from a
// deleted row only also needs that version to be a tombstone.
//
// staged-data: MUST-NOT-GATE -- a staged row a link leads to and this read
// did not see KEEPS ITS PII, and is missing from the person's export.
func linkStatement(always, whenDeleted []string) string {
	links := append([]string{}, always...)
	for _, link := range whenDeleted {
		links = append(links, "("+link+" AND m.payload->>'deleted' = 'true')")
	}
	return reachedRelation + `
		SELECT m.id, m.payload->>'gid' FROM "MemoryNodes" m
		 WHERE m.concept = $1 AND m.payload->>'storeId' = $2 AND (` + anyOf(links) + `)
		   AND NOT ` + aNewerVersion
}

// earlierStatement reads the versions of rows of one concept ($1) of one
// store ($2) that hold a link to a customer the scope holds ($3) and are NOT
// their row's newest version: the id and the time of each. The caller leaves
// out those of rows in scope, whose every version is scrubbed anyway.
//
// staged-data: MUST-NOT-GATE -- a staged version from when a row was the
// customer's, not seen here, keeps her name and address after her erasure.
func earlierStatement(links []string) string {
	return reachedRelation + `
		SELECT m.id, m."createdAt"::text FROM "MemoryNodes" m
		 WHERE m.concept = $1 AND m.payload->>'storeId' = $2 AND (` + anyOf(links) + `)
		   AND ` + aNewerVersion
}

// reachedBatches is what a round reached of the named concepts, as the JSON
// arrays linkStatement takes, a statement's worth each.
func reachedBatches(round map[string][]string, names []string) []string {
	type reached struct {
		C string `json:"c"`
		G string `json:"g"`
	}
	var all []reached
	for _, name := range sortedSetOf(names) {
		for _, gid := range round[name] {
			all = append(all, reached{C: name, G: gid})
		}
	}
	var out []string
	for len(all) > 0 {
		n := min(len(all), erasureBatch)
		raw, _ := json.Marshal(all[:n])
		out = append(out, string(raw))
		all = all[n:]
	}
	return out
}

func sortedSetOf(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return slices.Compact(out)
}

// namesHer is the condition under which a JSON value names the customer $4
// ({"g": gid}): her gid is a whole value somewhere inside it. A take-out
// reaches nobody else's row -- the entry that names her goes and the rest of
// the row stays -- so, unlike a link, it looks at every value of the entry.
func namesHer(value string) string {
	return "jsonb_path_exists(" + value + ", '$.** ? (@ == $g)', $4::jsonb)"
}

// takeOutFind reads the rows of one concept ($1) of one store ($2) that name
// the customer in one of the given fields. Only versions whose text holds her
// gid as a whole JSON string ($3) are looked at.
//
// staged-data: MUST-NOT-GATE -- a staged row that refers to the customer and
// was not seen here keeps her gid after her erasure.
func takeOutFind(fields []string) string {
	var conditions []string
	for _, field := range fields {
		conditions = append(conditions, namesHer("m.payload->"+sqlText(field)))
	}
	return `SELECT m.id FROM "MemoryNodes" m
		 WHERE m.concept = $1 AND m.payload->>'storeId' = $2 AND m.payload::text LIKE $3
		   AND (` + anyOf(conditions) + `)
		 GROUP BY m.id`
}

// queryer is what a read runs on: a session, or a test's handle.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryRows runs a read whose every column is text, or NULL read as "".
func queryRows(ctx context.Context, db queryer, query string, args ...any) ([][]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		into := make([]any, len(cols))
		for i := range cells {
			into[i] = &cells[i]
		}
		if err := rows.Scan(into...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, cell := range cells {
			row[i] = cell.String
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// batches cuts a list into statements' worth.
func batches(items []string) [][]string {
	var out [][]string
	for len(items) > erasureBatch {
		out = append(out, items[:erasureBatch])
		items = items[erasureBatch:]
	}
	if len(items) > 0 {
		out = append(out, items)
	}
	return out
}

// jsonArray is a list of strings as the one text parameter a statement takes
// it in. A gid holds colons and slashes, which an array literal would need
// quoted; JSON has no such trouble.
func jsonArray(items []string) string {
	if items == nil {
		items = []string{}
	}
	raw, _ := json.Marshal(items)
	return string(raw)
}

func jsonObject(v map[string]any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// sqlText is a field's name as a SQL string literal. The names come from the
// generated model, never from a request, and are quoted all the same.
func sqlText(name string) string { return "'" + strings.ReplaceAll(name, "'", "''") + "'" }

// anyOf joins conditions with OR, false for none.
func anyOf(conditions []string) string {
	if len(conditions) == 0 {
		return "false"
	}
	return strings.Join(conditions, " OR ")
}

// likeEscape makes a value match itself in a LIKE pattern.
func likeEscape(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// ExportCustomerData collects every mirror row a customer's erasure would
// scrub, for the store's owner to answer a data request with: the rows of
// erasureScopeOf, each at its newest version.
func (c *Connector) ExportCustomerData(ctx context.Context, store Store, customerGID string, orderGIDs []string) (map[string]any, error) {
	if customerGID == "" {
		return nil, fmt.Errorf("shopify: data request carried no customer id")
	}
	s, done, err := c.openPrivacySession(ctx, "customers/data_request")
	if err != nil {
		return nil, err
	}
	defer done()
	return c.exportCustomerData(ctx, s, store, customerGID, orderGIDs)
}

// exportCustomerData is ExportCustomerData on a session that is open.
//
// staged-data: MUST-NOT-GATE -- a data request answers with what MemQL HOLDS
// on the person, and a staged row left out of the export is held all the same.
func (c *Connector) exportCustomerData(ctx context.Context, s *privacySession, store Store, customerGID string, orderGIDs []string) (map[string]any, error) {
	scope, err := erasureScopeOf(ctx, s, store.ID, customerGID, orderGIDs)
	if err != nil {
		return nil, err
	}
	collected := map[string]any{}
	for concept, rows := range scope.Rows {
		var exported []map[string]any
		for _, batch := range batches(rowIDs(rows)) {
			found, err := s.rows(ctx,
				`SELECT DISTINCT ON (id) payload::text FROM "MemoryNodes"
				  WHERE concept = $1 AND id IN (SELECT jsonb_array_elements_text($2::jsonb))
				  ORDER BY id, "createdAt" DESC`,
				generated.ConceptID(concept), jsonArray(batch))
			if err != nil {
				return nil, fmt.Errorf("shopify: export %s: %w", concept, err)
			}
			for _, row := range found {
				payload, err := decodeJSONObject(row[0])
				if err != nil {
					return nil, fmt.Errorf("shopify: export %s: a stored row is not JSON", concept)
				}
				exported = append(exported, payload)
			}
		}
		if len(exported) > 0 {
			collected[concept] = exported
		}
	}
	return map[string]any{
		"store":       store.Domain,
		"customerGid": customerGID,
		"exportedAt":  c.now().UTC().Format(time.RFC3339),
		"apiVersion":  generated.APIVersion,
		"rows":        collected,
	}, nil
}

func rowIDs(rows []erasedRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

// ErasureResult is what one pass of a customer's erasure did.
type ErasureResult struct {
	// Rows is how many rows the erasure's scope held.
	Rows int
	// Versions is how many row versions the pass changed. A second pass over
	// rows nothing has written since changes none.
	Versions int64
	// Earlier is how many versions of rows that are no longer the customer's
	// held her in a link field and were scrubbed by themselves (ruling 94).
	// They are counted in Versions when the pass changed them.
	Earlier int64
	// Alone is how many deleted customer records whose last order is in
	// scope were scrubbed by themselves (ruling 98).
	Alone int64
	// Unlisted is how many versions of rows OUTSIDE the scope had an entry
	// that names the customer taken out of them: a field of a merchant's
	// metaobject, her gid in a list of references.
	Unlisted int64
}

// RedactCustomer erases one customer from a store's mirror: it finds the scope
// (erasureScopeOf) and rewrites every version of every row in it by the policy
// of erasure_fields.go, keeping the opaque gids and the facts of the sale.
//
// WHAT SURVIVES IS THE POINT. An order's total, its line quantities and its
// dates stay: the merchant's books are not the customer's personal data, and
// destroying them would be a different compliance failure. What goes is
// everything that is not shown to be about the sale.
//
// It runs by the STORE'S ID and needs no store row: the mirror's rows can
// outlive the row that configured them. It is safe to run twice, and every
// erasure is (ErasureSecondPassAfter): a version the pass leaves as it should
// be is not written, so what a second pass changes is what was written
// between the two.
func (c *Connector) RedactCustomer(ctx context.Context, storeID, customerGID string, orderGIDs []string) (ErasureResult, error) {
	if customerGID == "" {
		return ErasureResult{}, fmt.Errorf("shopify: redact carried no customer id")
	}
	s, done, err := c.openPrivacySession(ctx, "customers/redact")
	if err != nil {
		return ErasureResult{}, err
	}
	defer done()
	res, _, err := redactCustomer(ctx, s, storeID, customerGID, orderGIDs)
	return res, err
}

// redactCustomer is RedactCustomer on a session that is open, and answers the
// scope it scrubbed as well.
func redactCustomer(ctx context.Context, s *privacySession, storeID, customerGID string, orderGIDs []string) (ErasureResult, *erasureScope, error) {
	scope, err := erasureScopeOf(ctx, s, storeID, customerGID, orderGIDs)
	if err != nil {
		return ErasureResult{}, nil, err
	}
	res := ErasureResult{Rows: scope.count()}
	for _, concept := range generated.ApplyOrder {
		spec := generated.Types[concept]
		if spec == nil {
			continue
		}
		// Every customer row scrubbed here is a person's own: hers, and a
		// deleted record scrubbed by itself (Alone). It keeps nothing.
		plan := erasurePlanFor(spec, concept == erasedConcept)
		for _, batch := range batches(rowIDs(scope.Rows[concept])) {
			n, err := scrubRows(ctx, s, generated.ConceptID(concept), plan, batch)
			if err != nil {
				return res, scope, fmt.Errorf("shopify: redact %s: %w", concept, err)
			}
			res.Versions += n
		}
		for _, batch := range batches(rowIDs(scope.Alone[concept])) {
			n, err := scrubRows(ctx, s, generated.ConceptID(concept), plan, batch)
			if err != nil {
				return res, scope, fmt.Errorf("shopify: redact the deleted %s rows whose last order is the customer's: %w", concept, err)
			}
			res.Versions += n
			res.Alone += int64(len(batch))
		}
		for earlier := scope.Earlier[concept]; len(earlier) > 0; {
			n := min(len(earlier), erasureBatch)
			changed, err := scrubVersions(ctx, s, generated.ConceptID(concept), plan, earlier[:n])
			if err != nil {
				return res, scope, fmt.Errorf("shopify: redact the versions of %s rows that were the customer's: %w", concept, err)
			}
			res.Versions += changed
			res.Earlier += int64(n)
			earlier = earlier[n:]
		}
		fields, freeForm := erasureTakeOutsOf(spec), erasureFreeFormTakeOutsOf(spec)
		for _, batch := range batches(scope.TakenFrom[concept]) {
			for _, customer := range scope.Customers {
				n, err := takeOut(ctx, s, generated.ConceptID(concept), fields, freeForm, customer, batch)
				if err != nil {
					return res, scope, fmt.Errorf("shopify: take the customer out of the %s rows that refer to her: %w", concept, err)
				}
				res.Unlisted += n
			}
		}
	}
	return res, scope, nil
}

// scrubRows rewrites every VERSION of the given rows of one concept by a plan,
// and answers how many versions it changed.
//
// A key the plan keeps is left; a string it marks becomes the marker; a list
// it empties becomes []; and EVERY OTHER KEY IS REMOVED, a key the model does
// not know included. That last is default deny in the one place it has to
// hold: a payload is rebuilt from what may stay, not edited by a list of what
// must go.
//
// The values are ones the concept's schema accepts (erasureScrubFate), which
// is what lets the mirror write the row again. Whatever a field holds now is
// rewritten, a value an earlier erasure left included: that one put the
// string marker in objects, lists and booleans, and a row it touched is
// writable again after this.
//
// Every version, and that is the requirement rather than thoroughness for its
// own sake: MemoryNodes is keyed (id, createdAt), so scrubbing the newest
// version alone leaves the customer's name in the row's history, where a
// point-in-time read still returns it. A version already as the plan leaves it
// is not written.
//
// staged-data: MUST-NOT-GATE -- a staged row skipped here KEEPS ITS PII.
func scrubRows(ctx context.Context, s *privacySession, concept string, plan erasurePlan, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return scrub(ctx, s, concept, plan, `m.id IN (SELECT jsonb_array_elements_text($2::jsonb))`, jsonArray(ids))
}

// scrubVersions rewrites the given versions, and no other version of their
// rows, by a plan: the versions from when a row that is not the customer's
// now was hers (erasureScope.Earlier).
func scrubVersions(ctx context.Context, s *privacySession, concept string, plan erasurePlan, versions []rowVersion) (int64, error) {
	if len(versions) == 0 {
		return 0, nil
	}
	raw, _ := json.Marshal(versions)
	return scrub(ctx, s, concept, plan,
		`(m.id, m."createdAt") IN (SELECT v.id, v.at::timestamptz FROM jsonb_to_recordset($2::jsonb) AS v(id text, at text))`, string(raw))
}

// scrub is the one rewrite both make: which names the versions, by $2.
//
// staged-data: MUST-NOT-GATE -- a staged version skipped here KEEPS ITS PII.
func scrub(ctx context.Context, s *privacySession, concept string, plan erasurePlan, which, named string) (int64, error) {
	return s.exec(ctx,
		`WITH scrubbed AS (
		   SELECT m.id, m."createdAt",
		          COALESCE((SELECT jsonb_object_agg(e.key, CASE
		                      WHEN e.key IN (SELECT jsonb_array_elements_text($4::jsonb)) THEN to_jsonb($7::text)
		                      WHEN e.key IN (SELECT jsonb_array_elements_text($6::jsonb)) AND COALESCE(m.payload->>$8, '') = '' THEN to_jsonb($7::text)
		                      WHEN e.key IN (SELECT jsonb_array_elements_text($5::jsonb)) THEN '[]'::jsonb
		                      ELSE e.value END)
		                      FROM jsonb_each(m.payload) e
		                     WHERE e.key IN (SELECT jsonb_array_elements_text($3::jsonb))
		                        OR e.key IN (SELECT jsonb_array_elements_text($4::jsonb))
		                        OR e.key IN (SELECT jsonb_array_elements_text($5::jsonb))
		                        OR e.key IN (SELECT jsonb_array_elements_text($6::jsonb))), '{}'::jsonb) AS payload
		     FROM "MemoryNodes" m
		    WHERE m.concept = $1 AND `+which+`
		 )
		 UPDATE "MemoryNodes" m SET payload = s.payload
		   FROM scrubbed s
		  WHERE m.concept = $1 AND m.id = s.id AND m."createdAt" = s."createdAt"
		    AND m.payload IS DISTINCT FROM s.payload`,
		concept, named, jsonArray(plan.Keep), jsonArray(plan.Mark), jsonArray(plan.Empty),
		jsonArray(plan.MarkOnCustomItem), RedactionMarker, plan.CustomItemReference)
}

// takeOut takes the entries that name one customer out of the given fields of
// the given rows, in every version that holds one, and changes nothing else
// in them. A field that is a list loses the entries that name her -- an
// object of the list, or her gid in a list of references -- and a field that
// is one object and names her loses its key, which the concept's schema reads
// as a field that was never set. A FREE-FORM field (freeForm: one JSON value
// by key, a metaobject's values) loses the keys that name her, has her gid
// taken out of a key that lists several, and keeps its other keys.
//
// staged-data: MUST-NOT-GATE -- a staged row skipped here keeps the entry
// that names the customer.
func takeOut(ctx context.Context, s *privacySession, concept string, fields, freeForm []string, customerGID string, ids []string) (int64, error) {
	if len(ids) == 0 || len(fields) == 0 {
		return 0, nil
	}
	// The entries of a list that do not name her, in their order.
	kept := func(list string) string {
		return `COALESCE((SELECT jsonb_agg(entry.value ORDER BY entry.at)
		                    FROM jsonb_array_elements(` + list + `) WITH ORDINALITY AS entry(value, at)
		                   WHERE NOT ` + namesHer("entry.value") + `), '[]'::jsonb)`
	}
	return s.exec(ctx,
		`WITH taken AS (
		   SELECT m.id, m."createdAt",
		          COALESCE((SELECT jsonb_object_agg(e.key, CASE
		                      WHEN e.key IN (SELECT jsonb_array_elements_text($5::jsonb)) AND jsonb_typeof(e.value) = 'object'
		                        THEN COALESCE((SELECT jsonb_object_agg(v.key, CASE WHEN jsonb_typeof(v.value) = 'array' THEN `+kept("v.value")+` ELSE v.value END)
		                                         FROM jsonb_each(e.value) v
		                                        WHERE jsonb_typeof(v.value) = 'array' OR NOT `+namesHer("v.value")+`), '{}'::jsonb)
		                      WHEN e.key IN (SELECT jsonb_array_elements_text($3::jsonb)) AND jsonb_typeof(e.value) = 'array'
		                        THEN `+kept("e.value")+`
		                      ELSE e.value END)
		                      FROM jsonb_each(m.payload) e
		                     WHERE NOT (e.key IN (SELECT jsonb_array_elements_text($3::jsonb))
		                                AND NOT e.key IN (SELECT jsonb_array_elements_text($5::jsonb))
		                                AND jsonb_typeof(e.value) = 'object' AND `+namesHer("e.value")+`)
		                       AND NOT (e.key IN (SELECT jsonb_array_elements_text($5::jsonb))
		                                AND jsonb_typeof(e.value) = 'string' AND `+namesHer("e.value")+`)), '{}'::jsonb) AS payload
		     FROM "MemoryNodes" m
		    WHERE m.concept = $1 AND m.id IN (SELECT jsonb_array_elements_text($2::jsonb))
		 )
		 UPDATE "MemoryNodes" m SET payload = t.payload
		   FROM taken t
		  WHERE m.concept = $1 AND m.id = t.id AND m."createdAt" = t."createdAt"
		    AND m.payload IS DISTINCT FROM t.payload`,
		concept, jsonArray(ids), jsonArray(fields), jsonObject(map[string]any{"g": customerGID}), jsonArray(freeForm))
}
