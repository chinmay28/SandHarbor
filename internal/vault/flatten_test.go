package vault

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"testing"
)

// flatTree turns full file paths into what planFlatten takes: the files, each with
// its path as its ID, and every folder below dir that holds one of them.
// A path ending in a slash is a folder with nothing in it.
func flatTree(dir string, paths ...string) ([]flatFile, []string) {
	var files []flatFile
	seen := map[string]bool{}
	var folders []string
	folder := func(at string) {
		for ; below(at, dir) && !seen[at]; at = CleanDir(path.Dir(at)) {
			seen[at] = true
			folders = append(folders, at)
		}
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "/") {
			folder(CleanDir(p))
			continue
		}
		files = append(files, flatFile{ID: p, Name: path.Base(p), Dir: path.Dir(p), Size: 10})
		folder(path.Dir(p))
	}
	return files, folders
}

// flattened is the plan for a tree, as "from → to" lines in the order it would
// run, which is the whole of what a flatten promises.
func flattened(plan *FlattenPlan) []string {
	out := []string{}
	for _, m := range plan.Moves {
		out = append(out, m.ID+" → "+JoinPath(m.To, m.As))
	}
	return out
}

func same(t *testing.T, what string, got, want []string) {
	t.Helper()
	if got == nil {
		got = []string{}
	}
	if want == nil {
		want = []string{}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s:\n got  %q\n want %q", what, got, want)
	}
}

var films = []string{
	"/m/loose.mkv",
	"/m/action/matrix/matrix.mkv",
	"/m/action/matrix/subs/en.srt",
	"/m/scifi/dune/dune.mkv",
}

func TestFlattenPlans(t *testing.T) {
	cases := []struct {
		name    string
		paths   []string
		opts    FlattenOptions
		moves   []string
		emptied []string
		make    []string // folders the plan needs created
		kept    int
		clashes int
	}{
		{
			name:  "keeping nothing brings every file up and empties every folder",
			paths: films,
			moves: []string{
				"/m/action/matrix/matrix.mkv → /m/matrix.mkv",
				"/m/scifi/dune/dune.mkv → /m/dune.mkv",
				"/m/action/matrix/subs/en.srt → /m/en.srt",
			},
			emptied: []string{"/m/action/matrix/subs", "/m/action/matrix", "/m/scifi/dune", "/m/action", "/m/scifi"},
			kept:    1,
		},
		{
			name:    "leaf with nothing kept is the same plain flatten",
			paths:   []string{"/m/a/b/x.txt"},
			opts:    FlattenOptions{From: FromLeaf, Merge: true},
			moves:   []string{"/m/a/b/x.txt → /m/x.txt"},
			emptied: []string{"/m/a/b", "/m/a"},
		},
		{
			name:  "from the top, the nearest folders survive and the rest collapse into them",
			paths: films,
			opts:  FlattenOptions{Keep: 1},
			moves: []string{
				"/m/action/matrix/matrix.mkv → /m/action/matrix.mkv",
				"/m/scifi/dune/dune.mkv → /m/scifi/dune.mkv",
				"/m/action/matrix/subs/en.srt → /m/action/en.srt",
			},
			emptied: []string{"/m/action/matrix/subs", "/m/action/matrix", "/m/scifi/dune"},
			kept:    1,
		},
		{
			name:    "from the top, two levels leaves only what is deeper than two",
			paths:   films,
			opts:    FlattenOptions{Keep: 2},
			moves:   []string{"/m/action/matrix/subs/en.srt → /m/action/matrix/en.srt"},
			emptied: []string{"/m/action/matrix/subs"},
			kept:    3,
		},
		{
			name:  "from the leaves, each file keeps the folder it is in",
			paths: films,
			opts:  FlattenOptions{Keep: 1, From: FromLeaf},
			moves: []string{
				"/m/action/matrix/matrix.mkv → /m/matrix/matrix.mkv",
				"/m/scifi/dune/dune.mkv → /m/dune/dune.mkv",
				"/m/action/matrix/subs/en.srt → /m/subs/en.srt",
			},
			emptied: []string{"/m/action/matrix/subs", "/m/action/matrix", "/m/scifi/dune", "/m/action", "/m/scifi"},
			make:    []string{"/m/dune", "/m/matrix", "/m/subs"},
			kept:    1,
		},
		{
			name:    "from the leaves, two levels come up together",
			paths:   films,
			opts:    FlattenOptions{Keep: 2, From: FromLeaf},
			moves:   []string{"/m/action/matrix/subs/en.srt → /m/matrix/subs/en.srt"},
			emptied: []string{"/m/action/matrix/subs"},
			make:    []string{"/m/matrix/subs"},
			kept:    3,
		},
		{
			name:  "keeping as many levels as there are moves nothing",
			paths: films,
			opts:  FlattenOptions{Keep: 3, From: FromLeaf},
			kept:  4,
		},
		{
			name:  "same-named folders are kept apart unless merging is asked for",
			paths: []string{"/m/a/cd1/one.flac", "/m/b/cd1/one.flac", "/m/b/cd1/two.flac", "/m/c/CD1/three.flac"},
			opts:  FlattenOptions{Keep: 1, From: FromLeaf},
			moves: []string{
				"/m/a/cd1/one.flac → /m/cd1/one.flac",
				"/m/b/cd1/one.flac → /m/cd1 (2)/one.flac",
				"/m/b/cd1/two.flac → /m/cd1 (2)/two.flac",
				"/m/c/CD1/three.flac → /m/CD1 (3)/three.flac",
			},
			emptied: []string{"/m/a/cd1", "/m/b/cd1", "/m/c/CD1", "/m/a", "/m/b", "/m/c"},
			make:    []string{"/m/CD1 (3)", "/m/cd1", "/m/cd1 (2)"},
			clashes: 2,
		},
		{
			name:  "merging puts same-named folders together and numbers the files that collide",
			paths: []string{"/m/a/cd1/one.flac", "/m/b/cd1/one.flac", "/m/b/cd1/two.flac", "/m/c/CD1/three.flac"},
			opts:  FlattenOptions{Keep: 1, From: FromLeaf, Merge: true},
			moves: []string{
				"/m/a/cd1/one.flac → /m/cd1/one.flac",
				"/m/b/cd1/one.flac → /m/cd1/one (2).flac",
				"/m/b/cd1/two.flac → /m/cd1/two.flac",
				"/m/c/CD1/three.flac → /m/cd1/three.flac",
			},
			emptied: []string{"/m/a/cd1", "/m/b/cd1", "/m/c/CD1", "/m/a", "/m/b", "/m/c"},
			make:    []string{"/m/cd1"},
			clashes: 2,
		},
		{
			name:    "a folder that stays keeps its name, and one brought up beside it is numbered",
			paths:   []string{"/m/cd1/here.flac", "/m/a/cd1/up.flac"},
			opts:    FlattenOptions{Keep: 1, From: FromLeaf},
			moves:   []string{"/m/a/cd1/up.flac → /m/cd1 (2)/up.flac"},
			emptied: []string{"/m/a/cd1", "/m/a"},
			make:    []string{"/m/cd1 (2)"},
			kept:    1,
			clashes: 1,
		},
		{
			name:    "merging into a folder that stays never lands on a name it holds",
			paths:   []string{"/m/cd1/one.flac", "/m/a/cd1/one.flac"},
			opts:    FlattenOptions{Keep: 1, From: FromLeaf, Merge: true},
			moves:   []string{"/m/a/cd1/one.flac → /m/cd1/one (2).flac"},
			emptied: []string{"/m/a/cd1", "/m/a"},
			kept:    1,
			clashes: 1,
		},
		{
			name:    "a folder the flatten empties does not push its own child aside",
			paths:   []string{"/m/a/a/x.txt"},
			opts:    FlattenOptions{Keep: 1, From: FromLeaf},
			moves:   []string{"/m/a/a/x.txt → /m/a/x.txt"},
			emptied: []string{"/m/a/a"},
		},
		{
			name:  "unmerged, parents of the same name stay apart and each keeps its own children",
			paths: []string{"/m/p/x/cd1/a.flac", "/m/p/x/cd2/b.flac", "/m/q/x/cd1/c.flac"},
			opts:  FlattenOptions{Keep: 2, From: FromLeaf},
			moves: []string{
				"/m/p/x/cd1/a.flac → /m/x/cd1/a.flac",
				"/m/p/x/cd2/b.flac → /m/x/cd2/b.flac",
				"/m/q/x/cd1/c.flac → /m/x (2)/cd1/c.flac",
			},
			emptied: []string{"/m/p/x/cd1", "/m/p/x/cd2", "/m/q/x/cd1", "/m/p/x", "/m/q/x", "/m/p", "/m/q"},
			make:    []string{"/m/x (2)/cd1", "/m/x/cd1", "/m/x/cd2"},
			clashes: 1,
		},
		{
			name:    "a prefix is the folders a file loses, not the ones it keeps",
			paths:   []string{"/m/2023/corfu/day1/img.jpg"},
			opts:    FlattenOptions{Keep: 1, Prefix: true},
			moves:   []string{"/m/2023/corfu/day1/img.jpg → /m/2023/corfu - day1 - img.jpg"},
			emptied: []string{"/m/2023/corfu/day1", "/m/2023/corfu"},
		},
		{
			name:    "a prefix from the leaves is the folders above the ones kept",
			paths:   []string{"/m/2023/corfu/day1/img.jpg"},
			opts:    FlattenOptions{Keep: 1, From: FromLeaf, Prefix: true},
			moves:   []string{"/m/2023/corfu/day1/img.jpg → /m/day1/2023 - corfu - img.jpg"},
			emptied: []string{"/m/2023/corfu/day1", "/m/2023/corfu", "/m/2023"},
			make:    []string{"/m/day1"},
		},
		{
			name:    "the shallowest file keeps its name and a name already here is never landed on",
			paths:   []string{"/m/one.jpg", "/m/a/one.jpg", "/m/a/b/one.jpg"},
			moves:   []string{"/m/a/one.jpg → /m/one (2).jpg", "/m/a/b/one.jpg → /m/one (3).jpg"},
			emptied: []string{"/m/a/b", "/m/a"},
			kept:    1,
		},
		{
			name:    "a folder already holding nothing is cleared with the rest",
			paths:   []string{"/m/a/x.txt", "/m/hollow/inner/"},
			moves:   []string{"/m/a/x.txt → /m/x.txt"},
			emptied: []string{"/m/hollow/inner", "/m/a", "/m/hollow"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files, folders := flatTree("/m", c.paths...)
			plan := planFlatten("/m", files, folders, c.opts)

			same(t, "moves", flattened(plan), c.moves)
			same(t, "emptied", plan.Emptied, c.emptied)

			var made []string
			for _, f := range plan.Folders {
				if !f.Exists {
					made = append(made, f.Path)
				}
			}
			same(t, "folders to make", made, c.make)

			if plan.Kept != c.kept {
				t.Errorf("%d files kept, want %d", plan.Kept, c.kept)
			}
			if plan.Clashes != c.clashes {
				t.Errorf("%d clashes, want %d", plan.Clashes, c.clashes)
			}
			if want := int64(10 * len(c.moves)); plan.Bytes != want {
				t.Errorf("plan moves %d bytes, want %d", plan.Bytes, want)
			}
		})
	}
}

// The properties every plan has to have, whatever was asked for: nothing lands
// on anything, nothing moves that is already shallow enough, a folder files
// move into is never one to remove, and a second run finds nothing to do.
func TestFlattenPlansAreSafeAndIdempotent(t *testing.T) {
	paths := []string{
		"/m/one.jpg", "/m/a/one.jpg", "/m/a/b/one.jpg", "/m/a/b/c/one.jpg",
		"/m/b/one.jpg", "/m/b/b/one.jpg", "/m/c/b/c/one.jpg", "/m/c/B/c/two.jpg",
		"/m/b (2)/one.jpg", "/m/d/b (2)/one.jpg", "/m/empty/",
	}

	for _, from := range []FlattenEnd{FromTop, FromLeaf} {
		for _, merge := range []bool{false, true} {
			for _, prefix := range []bool{false, true} {
				for keep := 0; keep <= 4; keep++ {
					opts := FlattenOptions{Keep: keep, From: from, Merge: merge, Prefix: prefix}
					t.Run(fmt.Sprintf("%s keep=%d merge=%t prefix=%t", from, keep, merge, prefix), func(t *testing.T) {
						files, folders := flatTree("/m", paths...)
						plan := planFlatten("/m", files, folders, opts)

						after, landed := apply(files, plan)
						if len(after) != len(files) {
							t.Fatalf("%d files after the flatten, want %d: something was landed on", len(after), len(files))
						}
						for _, f := range landed {
							if d := depthUnder(f.Dir, "/m"); d > keep {
								t.Errorf("%s ends %d folders down, deeper than the %d kept", f.ID, d, keep)
							}
						}
						for _, m := range plan.Moves {
							if depthUnder(m.Dir, "/m") <= keep {
								t.Errorf("%s moved though it was no deeper than the levels kept", m.ID)
							}
						}

						holding := map[string]bool{}
						for _, f := range landed {
							holding[f.Dir] = true
						}
						for _, gone := range plan.Emptied {
							if holding[gone] {
								t.Errorf("%s is to be removed and holds a file", gone)
							}
						}

						_, still := flatTree("/m")
						for _, f := range landed {
							_, more := flatTree("/m", JoinPath(f.Dir, f.Name))
							still = append(still, more...)
						}
						if again := planFlatten("/m", landed, still, opts); len(again.Moves) != 0 {
							t.Errorf("a second run moves %d files: %v", len(again.Moves), flattened(again))
						}
					})
				}
			}
		}
	}
}

// apply runs a plan over files, returning where everything is by full path and
// the files themselves as they stand afterwards.
func apply(files []flatFile, plan *FlattenPlan) (map[string]bool, []flatFile) {
	moved := map[string]FlattenMove{}
	for _, m := range plan.Moves {
		moved[m.ID] = m
	}
	at := map[string]bool{}
	var out []flatFile
	for _, f := range files {
		if m, ok := moved[f.ID]; ok {
			f.Dir, f.Name = m.To, m.As
		}
		at[strings.ToLower(JoinPath(f.Dir, f.Name))] = true
		out = append(out, f)
	}
	return at, out
}

func TestFlattenRefusesWhatItCannotMean(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if _, err := v.Flatten(MainScope, "/", FlattenOptions{Keep: -1}); err == nil {
		t.Error("a negative number of levels was accepted")
	}
	if _, err := v.Flatten(MainScope, "/", FlattenOptions{From: "middle"}); err == nil {
		t.Error("an end that is neither top nor leaf was accepted")
	}
	if _, err := v.Flatten(MainScope, "/nowhere", FlattenOptions{}); err == nil {
		t.Error("a folder that is not there was flattened")
	}
}

// The vault's own answer: read from the index, including a folder nothing was
// ever put in, and changing nothing.
func TestFlattenReadsTheIndexAndChangesNothing(t *testing.T) {
	v := tree(t, "/m/a/cd1/one.flac", "/m/b/cd1/one.flac")
	if err := v.Mkdir(MainScope, "/m/cd1"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	plan, err := v.Flatten(MainScope, "/m", FlattenOptions{Keep: 1, From: FromLeaf, Merge: true})
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	var to []string
	for _, m := range plan.Moves {
		to = append(to, JoinPath(m.To, m.As))
	}
	sort.Strings(to)
	same(t, "landings", to, []string{"/m/cd1/one (2).flac", "/m/cd1/one.flac"})

	if len(plan.Folders) != 1 || !plan.Folders[0].Exists || plan.Folders[0].Label != "cd1" {
		t.Errorf("folders are %+v, want the one cd1 that is already there", plan.Folders)
	}
	if plan.Depth != 2 {
		t.Errorf("deepest file is %d down, want 2", plan.Depth)
	}

	s, err := v.Survey(MainScope, "/m")
	if err != nil {
		t.Fatalf("Survey: %v", err)
	}
	for _, f := range s.Files {
		if f.Depth != 2 {
			t.Errorf("%s moved while it was only being planned", f.Name)
		}
	}
}
