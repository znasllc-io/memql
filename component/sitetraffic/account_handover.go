package sitetraffic

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/accountexport"
)

// account_handover.go -- website statistics in an account export
// (docs/superpowers/specs/2026-11-12-account-export-design.md, section B,
// "Outside the row table").
//
// What travels is what Deployables > Statistics reads: the hourly and daily
// summaries and the ranked values of a day, of the current 24-hour window and
// of a whole month, for the account's sites. What does not:
//
//   - the request log (edge_request) and the per-visitor state
//     (edge_stats_visitor): their visitor hashes are made with a daily secret
//     (edge_visitor_secret) that means nothing on another cluster, and is not
//     carried either;
//   - the rollup's own bookkeeping (edge_stats_progress, edge_stats_final,
//     edge_stats_month_day, edge_stats_settled_hour, edge_stats_value_hour):
//     the target's rollup keeps its own, and a settled state is an open day's
//     working figures, published already into the summaries above.
//
// Each line is the table's row as JSON, column by column, so a column added
// to a table later travels without a change here. A timestamp is written in
// UTC whatever the session's time zone, so two runs give the same bytes.

const siteStatisticsHandOver = "site-statistics"

const siteConcept = "v1:platform:site"

// statisticsTable is one published table: its name, its key in order, and
// its timestamp columns.
type statisticsTable struct {
	name       string
	key        string
	timestamps []string
}

// publishedStatisticsTables are written in this order, the folders' name
// order, so the file's layout does not depend on a map.
var publishedStatisticsTables = []statisticsTable{
	{name: "edge_stats_daily", key: "site_id, day"},
	{name: "edge_stats_daily_top", key: "site_id, day, dimension, value"},
	{name: "edge_stats_hourly", key: "site_id, hour", timestamps: []string{"hour"}},
	{name: "edge_stats_monthly_top", key: "site_id, month, dimension, value"},
	{name: "edge_stats_window_top", key: "site_id, window_end, dimension, value", timestamps: []string{"window_end"}},
}

func init() {
	accountexport.RegisterHandOver(siteStatisticsHandOver, handOverSiteStatistics)
}

func handOverSiteStatistics(ctx context.Context, q accountexport.Querier, in accountexport.HandOverInput, out accountexport.HandOverSink) error {
	sites := in.RowIDs(siteConcept)
	if len(sites) == 0 {
		return nil
	}
	// The ids travel as one JSON array: the snapshot is a plain database/sql
	// transaction, and an array of text is the one list every driver passes.
	ids, err := json.Marshal(sites)
	if err != nil {
		return err
	}
	for _, table := range publishedStatisticsTables {
		if err := handOverStatisticsTable(ctx, q, table, string(ids), out); err != nil {
			return fmt.Errorf("%s: %w", table.name, err)
		}
	}
	return nil
}

// statisticsTableSQL selects a table's rows for the sites, in key order.
//
// No staged-data verdict is needed here: these are tables of their own, not
// rows of a concept, and the account's sites are the rows the file already
// carries, chosen before this runs.
func statisticsTableSQL(table statisticsTable) string {
	line := "to_jsonb(t)"
	for _, col := range table.timestamps {
		line += fmt.Sprintf(` || jsonb_build_object('%[1]s', to_char(t.%[1]s AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))`, col)
	}
	return fmt.Sprintf(`SELECT (%s)::text FROM %s t
 WHERE t.site_id IN (SELECT jsonb_array_elements_text($1::jsonb))
 ORDER BY %s`, line, table.name, table.key)
}

// handOverStatisticsTable writes one table. Its folder is opened at its first
// row, so a table with nothing for these sites adds no empty folder.
func handOverStatisticsTable(ctx context.Context, q accountexport.Querier, table statisticsTable, ids string, out accountexport.HandOverSink) error {
	rows, err := q.QueryContext(ctx, statisticsTableSQL(table), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	var part accountexport.LineSink
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if part == nil {
			part = out.Parted(siteStatisticsHandOver + "/" + table.name)
		}
		if err := part.Write(json.RawMessage(line)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if part == nil {
		return nil
	}
	return part.Close()
}
