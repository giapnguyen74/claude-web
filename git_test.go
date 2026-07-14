package main

import "testing"

func TestParseBranchHeader(t *testing.T) {
	cases := []struct {
		in               string
		branch, upstream string
		ahead, behind    int
	}{
		{"main...origin/main [ahead 1, behind 2]", "main", "origin/main", 1, 2},
		{"main...origin/main [ahead 3]", "main", "origin/main", 3, 0},
		{"main...origin/main [behind 5]", "main", "origin/main", 0, 5},
		{"main...origin/main", "main", "origin/main", 0, 0},
		{"main", "main", "", 0, 0},
		{"No commits yet on main", "main", "", 0, 0},
		{"HEAD (no branch)", "HEAD (no branch)", "", 0, 0},
	}
	for _, c := range cases {
		b, u, a, be := parseBranchHeader(c.in)
		if b != c.branch || u != c.upstream || a != c.ahead || be != c.behind {
			t.Errorf("parseBranchHeader(%q) = (%q,%q,%d,%d); want (%q,%q,%d,%d)",
				c.in, b, u, a, be, c.branch, c.upstream, c.ahead, c.behind)
		}
	}
}

func TestParsePorcelainStatus(t *testing.T) {
	// Simulated `git status --porcelain -z --branch` output:
	// - branch header with divergence
	// - staged add (A ), staged+unstaged modify (MM), unstaged modify ( M),
	//   deleted in worktree ( D), untracked (??), rename (R) with orig field,
	//   and an unmerged/conflict entry (UU).
	out := "## main...origin/main [ahead 1, behind 2]\x00" +
		"A  added.txt\x00" +
		"MM both.txt\x00" +
		" M work.txt\x00" +
		" D gone.txt\x00" +
		"?? new.txt\x00" +
		"R  new_name.txt\x00old_name.txt\x00" +
		"UU conflict.txt\x00"

	r := parsePorcelainStatus(out)

	if r.Branch != "main" || r.Upstream != "origin/main" || r.Ahead != 1 || r.Behind != 2 {
		t.Fatalf("branch parse wrong: %+v", r)
	}
	// Staged: added.txt (A ), both.txt (M in index), new_name.txt (R )
	if got := paths(r.Staged); !eq(got, []string{"added.txt", "both.txt", "new_name.txt"}) {
		t.Errorf("staged = %v", got)
	}
	// Unstaged: both.txt (M in worktree), work.txt (M), gone.txt (D)
	if got := paths(r.Unstaged); !eq(got, []string{"both.txt", "work.txt", "gone.txt"}) {
		t.Errorf("unstaged = %v", got)
	}
	if got := paths(r.Untracked); !eq(got, []string{"new.txt"}) {
		t.Errorf("untracked = %v", got)
	}
	if got := paths(r.Conflicts); !eq(got, []string{"conflict.txt"}) {
		t.Errorf("conflicts = %v", got)
	}
}

func paths(entries []gitFileEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
