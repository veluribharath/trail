package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Commit is a commit made while (or shortly after) a session ran.
type Commit struct {
	SHA     string    `json:"sha"`
	Short   string    `json:"short"`
	Author  string    `json:"author"`
	When    time.Time `json:"when"`
	Subject string    `json:"subject"`
	Files   []string  `json:"files"`
	Overlap []string  `json:"overlap"` // files the session's transcript says it edited
}

type CommitsResult struct {
	Available bool     `json:"available"`
	Reason    string   `json:"reason,omitempty"`
	Toplevel  string   `json:"toplevel,omitempty"`
	Since     string   `json:"since,omitempty"`
	Until     string   `json:"until,omitempty"`
	Commits   []Commit `json:"commits"`
}

const (
	commitLead  = 10 * time.Minute // commits a little before the first event can belong to it
	commitTrail = 2 * time.Hour    // people often commit after the agent finishes
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	out, err := cmd.Output()
	return string(out), err
}

// SessionCommits lists commits in the session's repository authored inside its
// time window, marking those that touch files the session edited.
func SessionCommits(ctx context.Context, t *Transcript) CommitsResult {
	s := t.Session
	res := CommitsResult{Commits: []Commit{}}
	if _, err := exec.LookPath("git"); err != nil {
		res.Reason = "git is not installed on this machine."
		return res
	}
	if fi, err := os.Stat(s.Cwd); err != nil || !fi.IsDir() {
		res.Reason = "The session's folder isn't on this machine, so its commits can't be read."
		return res
	}
	top, err := git(ctx, s.Cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		res.Reason = "The session's folder isn't a git repository."
		return res
	}
	res.Toplevel = strings.TrimSpace(top)
	res.Available = true

	since := s.Started.Add(-commitLead)
	until := s.Ended.Add(commitTrail)
	res.Since, res.Until = since.Format(time.RFC3339), until.Format(time.RFC3339)

	// Files the transcript edited, relative to the repo top level.
	edited := map[string]bool{}
	for _, c := range t.Changes {
		for _, p := range []string{c.Path, c.MoveTo} {
			if p == "" {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(s.Cwd, p)
			}
			if r, err := filepath.Rel(res.Toplevel, p); err == nil && !strings.HasPrefix(r, "..") {
				edited[filepath.ToSlash(r)] = true
			}
		}
	}

	out, err := git(ctx, s.Cwd, "log", "--all", "--no-merges", "--date=iso-strict",
		"--since="+res.Since, "--until="+res.Until,
		"--format=%x1e%H%x1f%h%x1f%an%x1f%aI%x1f%s", "--name-only")
	if err != nil {
		res.Reason = "git log failed: " + firstLine(errText(err))
		return res
	}
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		lines := strings.Split(rec, "\n")
		f := strings.Split(lines[0], "\x1f")
		if len(f) < 5 {
			continue
		}
		c := Commit{SHA: f[0], Short: f[1], Author: f[2], When: parseTime(f[3]), Subject: f[4], Files: []string{}, Overlap: []string{}}
		for _, l := range lines[1:] {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			c.Files = append(c.Files, l)
			if edited[l] {
				c.Overlap = append(c.Overlap, l)
			}
		}
		res.Commits = append(res.Commits, c)
	}
	return res
}

// CommitPatch returns `git show` output for one commit in the session's repo.
func CommitPatch(ctx context.Context, cwd, sha string) (string, error) {
	if !shaRe.MatchString(sha) {
		return "", errors.New("not a commit id")
	}
	out, err := git(ctx, cwd, "show", "--no-color", "--stat", "--patch", "--format=fuller", sha)
	if err != nil {
		return "", errors.New(firstLine(errText(err)))
	}
	return clip(out, 2<<20), nil
}

func errText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return string(ee.Stderr)
	}
	return err.Error()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
