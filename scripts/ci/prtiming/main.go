package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// gh runs the GitHub CLI with arguments, never through a shell.
func gh(args ...string) (string, error) {
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return "", fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// logArgs are the gh arguments that print one job's log. GitHub's job logs
// carry terminal escape sequences, and since gh 2.102 `gh api` refuses such a
// response unless --allow-escape-sequences is passed; an older gh does not
// know the flag, so it is passed only where `gh api --help` offers it.
func logArgs(repo string, jobID int64, help string) []string {
	args := []string{"api"}
	if strings.Contains(help, "--allow-escape-sequences") {
		args = append(args, "--allow-escape-sequences")
	}
	return append(args, fmt.Sprintf("repos/%s/actions/jobs/%d/logs", repo, jobID))
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(s))
	return t
}

// fetchRuns reads the window's green pull-request ci.yml runs, each with its
// jobs and what its plan job printed.
func fetchRuns(repo, since, until string) ([]Run, error) {
	out, err := gh("api", "--paginate",
		fmt.Sprintf("repos/%s/actions/workflows/ci.yml/runs?event=pull_request&status=success&created=%s..%s&per_page=100", repo, since, until),
		"--jq", `.workflow_runs[] | [.id, .created_at] | @tsv`)
	if err != nil {
		return nil, err
	}
	help, _ := gh("api", "--help")
	var runs []Run
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 2 {
			continue
		}
		id, _ := strconv.ParseInt(f[0], 10, 64)
		r := Run{ID: id, CreatedAt: parseTime(f[1])}
		jobs, err := gh("api", "--paginate", fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=100", repo, id),
			"--jq", `.jobs[] | [.id, .name, (.conclusion // ""), (.started_at // ""), (.completed_at // "")] | @tsv`)
		if err != nil {
			return nil, err
		}
		js := bufio.NewScanner(strings.NewReader(jobs))
		for js.Scan() {
			g := strings.Split(js.Text(), "\t")
			if len(g) != 5 {
				continue
			}
			jid, _ := strconv.ParseInt(g[0], 10, 64)
			r.Jobs = append(r.Jobs, Job{ID: jid, Name: g[1], Conclusion: g[2], StartedAt: parseTime(g[3]), CompletedAt: parseTime(g[4])})
		}
		for _, j := range r.Jobs {
			if j.Name == "plan" {
				if log, err := gh(logArgs(repo, j.ID, help)...); err == nil {
					r.Mode, r.Selected = ParsePlanLog(log)
				}
			}
		}
		runs = append(runs, r)
	}
	return runs, sc.Err()
}

func main() {
	repo := flag.String("repo", "znasllc-io/memql", "the repository")
	since := flag.String("since", "", "first day, YYYY-MM-DD")
	until := flag.String("until", "", "last day, YYYY-MM-DD")
	flag.Parse()
	if *since == "" || *until == "" {
		fmt.Fprintln(os.Stderr, "usage: prtiming --since=YYYY-MM-DD --until=YYYY-MM-DD [--repo=owner/name]")
		os.Exit(2)
	}
	runs, err := fetchRuns(*repo, *since, *until)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(Report(runs, *since, *until))
}
