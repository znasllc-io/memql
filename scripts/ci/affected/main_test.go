package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunRejectsBadParametersWithExitTwo(t *testing.T) {
	cases := map[string][]string{
		"no subcommand":       {},
		"unknown subcommand":  {"shard"},
		"plan without flags":  {"plan"},
		"plan with a stray":   {"plan", "--event=push", "stray"},
		"plan with bad table": {"plan", "--event=push", "--classes=go x", "--timings=t", "--gate-packages=./", "--tag-trees=app"},
		"timings bad lane":    {"timings", "--lane=ui", "--table=t", "log"},
		"timings no logs":     {"timings", "--lane=go", "--table=t"},
		"unknown flag":        {"graph", "--nope"},
	}
	for name, args := range cases {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != exitBadParam {
			t.Errorf("%s: exit %d, want %d (stderr %q)", name, code, exitBadParam, errOut.String())
		}
	}
}

func TestTimingsRewritesTheTableFromLogs(t *testing.T) {
	dir := t.TempDir()
	table := filepath.Join(dir, "timings.tsv")
	if err := os.WriteFile(table, []byte("go\tcmd/kept\t9.00\ngo\tcmd/memqllint\t1.00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log1 := filepath.Join(dir, "a.log")
	log2 := filepath.Join(dir, "b.log")
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(log1, "2026-09-27T08:12:01.0000000Z ok  \tgithub.com/znasllc-io/memql/cmd/memqllint\t440.000s\n")
	write(log2, "ok  \tgithub.com/znasllc-io/memql/cmd/memqllint\t450.000s\nok  \tgithub.com/znasllc-io/memql\t110.000s\n")

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"timings", "--lane=go", "--table=" + table, log1, log2}, &out, &errOut); code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	got, err := readTable(table)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"cmd/kept": 9, "cmd/memqllint": 445, ".": 110}
	if !reflect.DeepEqual(map[string]float64(got["go"]), want) {
		t.Errorf("table = %v, want %v", got["go"], want)
	}

	empty := filepath.Join(dir, "empty.log")
	write(empty, "nothing here\n")
	if code := run(context.Background(), []string{"timings", "--lane=db", "--table=" + table, empty}, &out, &errOut); code != exitRefused {
		t.Errorf("a log with no result line must be refused (exit %d), not rewrite the table; got %d", exitRefused, code)
	}
	after, _ := os.ReadFile(table)
	if !strings.Contains(string(after), "cmd/kept") {
		t.Error("a refused refresh must leave the table untouched")
	}
}

func TestSplitNUL(t *testing.T) {
	got := splitNUL([]byte("a.go\x00dir/b c.md\x00\x00"))
	if want := []string{"a.go", "dir/b c.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("splitNUL = %q, want %q (a path with a space stays one path)", got, want)
	}
}
