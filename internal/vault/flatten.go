package vault

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Flattening a folder, all the way or only part of it.
//
// The plain flatten brings every file below a folder up into it. That is the
// right answer for a tree nobody wants and the wrong one for a tree that is only
// too deep: a film library filed by genre, then title, then disc wants its
// discs gone and its titles kept. So a flatten takes a number of levels to
// keep, and which end of the path they are counted from:
//
//   - From the top, the folders nearest the flattened one survive and
//     everything deeper collapses up into them. Keeping one level of
//     Action/Matrix (1999)/Subs/en.srt gives Action/en.srt.
//   - From the leaves, the folders nearest each file survive and are brought up
//     to the flattened one. Keeping one level of the same path gives
//     Subs/en.srt.
//
// Counting from the leaves is the only case where folders can meet: a/CD1 and
// b/CD1 both want to be CD1. Merge decides whether they become one folder or
// stay apart as CD1 and CD1 (2).
//
// Like the rest of the organizer this reads and nothing else (see organize.go).
// It answers where every file would go, and the browser runs that plan over the
// endpoints that already existed — make the folders, move the files, remove what
// the moves emptied — so a run that stalls has moved exactly what it says.
//
// Two properties make it safe to press, and both are shared with DateSort:
//
//   - It is idempotent. A file no deeper than the levels kept is not moved, and
//     every file a flatten moves ends up no deeper than that.
//   - No name is ever landed on. A name a move takes is checked against what is
//     in the destination now and against everything else arriving there, and
//     names are never freed by the moves that vacate them.

// FlattenEnd is which end of a file's path the kept levels are counted from.
type FlattenEnd string

const (
	// FromTop keeps the folders nearest the one being flattened. The default.
	FromTop FlattenEnd = "top"
	// FromLeaf keeps the folders nearest each file.
	FromLeaf FlattenEnd = "leaf"
)

// FlattenOptions is how much of the tree to keep, and how names are settled.
type FlattenOptions struct {
	// Keep is how many levels of folders survive. Zero is the plain flatten:
	// every file lands in the folder itself.
	Keep int

	// From is the end Keep is counted from; empty is FromTop. It makes no
	// difference when Keep is zero.
	From FlattenEnd

	// Merge lets folders of the same name, brought side by side by FromLeaf,
	// become one folder. Off, the later one is numbered: "CD1 (2)". Compared
	// without case, as file names are.
	Merge bool

	// Prefix names each moved file after the folders it loses —
	// "2023 - corfu - IMG_0001.jpg" — instead of numbering collisions.
	Prefix bool
}

// FlattenMove is one file and where the flatten would put it.
type FlattenMove struct {
	ID   string `json:"id"`
	Name string `json:"name"` // what it is called now
	Dir  string `json:"dir"`  // where it is now
	Size int64  `json:"size"`

	// To is the folder it lands in and As what it is called there.
	To string `json:"to"`
	As string `json:"as"`
}

// FlattenFolder is a folder other than the flattened one that files move into.
type FlattenFolder struct {
	Path string `json:"path"`

	// Label is the path as it reads from the folder being flattened.
	Label string `json:"label"`

	// Files is how many files move in, and Bytes what they come to.
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`

	// Exists is the client's cue to create it or not. Missing parents come
	// with it: creating a folder creates what is above it.
	Exists bool `json:"exists"`
}

// FlattenPlan is everything a flatten would do, before any of it is done.
type FlattenPlan struct {
	Path   string `json:"path"`
	Keep   int    `json:"keep"`
	From   string `json:"from"`
	Merge  bool   `json:"merge"`
	Prefix bool   `json:"prefix"`

	// Depth is how many folders down the deepest file sits. Keeping that many
	// levels or more moves nothing, so it is the ceiling worth offering.
	Depth int `json:"depth"`

	// Moves is every file that changes folder, shallowest first and
	// alphabetical within a folder, so the file nearest the top keeps its name
	// and the deeper duplicate is the one renamed.
	Moves []FlattenMove `json:"moves"`

	// Folders is where they go, by path. The flattened folder itself is not
	// listed: it is always there and never needs making.
	Folders []FlattenFolder `json:"folders"`

	// Bytes is what the moving files come to. Nothing travels.
	Bytes int64 `json:"bytes"`

	// Kept counts the files left where they are.
	Kept int `json:"kept"`

	// Clashes counts the folders that met another of the same name where they
	// were going — merged into it, or numbered to stay apart. Only FromLeaf
	// can produce one.
	Clashes int `json:"clashes"`

	// Emptied is every folder that would hold nothing at all once the moves
	// are done, deepest first. Never a folder the flatten moves files into.
	Emptied []string `json:"emptied"`
}

// Flatten plans a flatten of a folder. It changes nothing.
func (v *Vault) Flatten(scope Scope, dir string, opts FlattenOptions) (*FlattenPlan, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}

	dir = CleanDir(dir)
	if !m.FolderExists(dir) {
		return nil, fmt.Errorf("no such folder: %s", dir)
	}

	under := m.Descendants(dir)
	files := make([]flatFile, len(under))
	for i, e := range under {
		files[i] = flatFile{ID: e.ID, Name: e.Name, Dir: e.Dir, Size: e.Size}
	}
	var folders []string
	for _, f := range m.AllFolders() {
		if below(f, dir) {
			folders = append(folders, f)
		}
	}
	return planFlatten(dir, files, folders, opts), nil
}

func (o FlattenOptions) validate() error {
	if o.Keep < 0 {
		return fmt.Errorf("levels to keep cannot be negative: %d", o.Keep)
	}
	switch o.From {
	case "", FromTop, FromLeaf:
		return nil
	}
	return fmt.Errorf("unknown end %q: want %q or %q", o.From, FromTop, FromLeaf)
}

// flatFile is as much of a file as a flatten needs to know.
type flatFile struct {
	ID, Name, Dir string
	Size          int64
}

// planFlatten is the plan itself, over plain data: dir is the clean path of the
// folder being flattened, files is every file at or below it, and folders is
// every folder strictly below it. opts must have passed validate.
func planFlatten(dir string, files []flatFile, folders []string, opts FlattenOptions) *FlattenPlan {
	from := opts.From
	if from == "" || opts.Keep == 0 {
		from = FromTop
	}
	plan := &FlattenPlan{
		Path:    dir,
		Keep:    opts.Keep,
		From:    string(from),
		Merge:   opts.Merge,
		Prefix:  opts.Prefix,
		Moves:   []FlattenMove{},
		Folders: []FlattenFolder{},
	}

	files = append([]flatFile(nil), files...)
	sort.SliceStable(files, func(i, j int) bool {
		if a, b := depthUnder(files[i].Dir, dir), depthUnder(files[j].Dir, dir); a != b {
			return a < b
		}
		if files[i].Dir != files[j].Dir {
			return files[i].Dir < files[j].Dir
		}
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})

	exists := map[string]bool{}
	for _, f := range folders {
		exists[f] = true
	}

	// Every name every folder holds now. A file landing beside one that is
	// staying must not be given its name, and one landing where another is
	// leaving must not either: the plan has to hold in whatever order it runs.
	taken := map[string]map[string]bool{}
	claim := func(folder, name string) {
		if taken[folder] == nil {
			taken[folder] = map[string]bool{}
		}
		taken[folder][strings.ToLower(name)] = true
	}
	for _, f := range files {
		claim(f.Dir, f.Name)
		if d := depthUnder(f.Dir, dir); d > plan.Depth {
			plan.Depth = d
		}
	}

	var seats *seating
	if from == FromLeaf {
		seats = newSeating(opts.Merge)
		// The folders that keep a file where it is are spoken for first, under
		// their own names, whatever is brought up beside them. Then the ones
		// that are merely there: free to whichever folder arrives first.
		for _, f := range files {
			if depthUnder(f.Dir, dir) > opts.Keep {
				continue
			}
			for at := f.Dir; below(at, dir); at = CleanDir(path.Dir(at)) {
				seats.reserve(at, at)
			}
		}
		for _, f := range folders {
			if depthUnder(f, dir) <= opts.Keep {
				seats.reserve(f, "")
			}
		}
	}

	lands := make([]string, 0, len(files))
	into := map[string]*FlattenFolder{}

	for _, f := range files {
		segments := segmentsUnder(f.Dir, dir)
		cut := len(segments) - opts.Keep // how many folders this file loses
		if cut <= 0 {
			plan.Kept++
			lands = append(lands, f.Dir)
			continue
		}

		target, lost := dir, segments[opts.Keep:]
		if from == FromTop {
			target = CleanDir(path.Join(dir, path.Join(segments[:opts.Keep]...)))
		} else {
			lost = segments[:cut]
			for i := cut; i < len(segments); i++ {
				source := CleanDir(path.Join(dir, path.Join(segments[:i+1]...)))
				target = seats.seat(target, segments[i], source)
			}
		}

		wanted := f.Name
		if opts.Prefix {
			wanted = strings.Join(lost, " - ") + " - " + f.Name
		}
		as := freeName(wanted, taken[target])
		claim(target, as)

		lands = append(lands, target)
		plan.Bytes += f.Size
		plan.Moves = append(plan.Moves, FlattenMove{
			ID: f.ID, Name: f.Name, Dir: f.Dir, Size: f.Size, To: target, As: as,
		})

		if target == dir {
			continue
		}
		entry := into[target]
		if entry == nil {
			entry = &FlattenFolder{
				Path:   target,
				Label:  strings.Join(segmentsUnder(target, dir), "/"),
				Exists: exists[target],
			}
			into[target] = entry
		}
		entry.Files++
		entry.Bytes += f.Size
	}

	for _, f := range into {
		plan.Folders = append(plan.Folders, *f)
	}
	sort.Slice(plan.Folders, func(i, j int) bool { return plan.Folders[i].Path < plan.Folders[j].Path })

	if seats != nil {
		plan.Clashes = seats.clashes
	}
	plan.Emptied = leftEmpty(folders, dir, lands)
	return plan
}

// seating gives each folder a flatten brings up a place to land, and settles
// what happens when two of the same name want the same place.
//
// A folder is known by where it came from. Merging, every folder wanting a
// path gets that path. Not merging, a path belongs to the first folder to take
// it and the next is numbered — but the same folder asked twice gets the same
// answer, so the files of one folder are never split across two.
type seating struct {
	merge bool

	// owner is who holds each path, keyed without case; empty is a folder that
	// is there but holds nothing that stays, and so is free to the first taker.
	// spelt is the path as written.
	owner map[string]string
	spelt map[string]string

	// placed remembers where a source folder was put under a given parent.
	placed map[[2]string]string

	clashes int
}

func newSeating(merge bool) *seating {
	return &seating{
		merge:  merge,
		owner:  map[string]string{},
		spelt:  map[string]string{},
		placed: map[[2]string]string{},
	}
}

// reserve holds a path that already exists for the folder that is there. The
// first to reserve a path keeps it.
func (s *seating) reserve(p, source string) {
	key := strings.ToLower(p)
	if _, held := s.owner[key]; held {
		return
	}
	s.owner[key], s.spelt[key] = source, p
}

// seat is where the folder that came from source lands under parent, wanting
// to be called name.
func (s *seating) seat(parent, name, source string) string {
	memo := [2]string{parent, source}
	if at, ok := s.placed[memo]; ok {
		return at
	}

	at := s.find(parent, name, source)
	s.placed[memo] = at
	return at
}

func (s *seating) find(parent, name, source string) string {
	clashed := false
	for n := 1; ; n++ {
		wanted := name
		if n > 1 {
			wanted = fmt.Sprintf("%s (%d)", name, n)
		}
		p := CleanDir(path.Join(parent, wanted))
		key := strings.ToLower(p)

		owner, held := s.owner[key]
		switch {
		case !held:
			s.owner[key], s.spelt[key] = source, p
			return p
		case owner == "":
			s.owner[key] = source
			return s.spelt[key]
		case s.merge:
			s.clashes++
			return s.spelt[key]
		}
		if !clashed {
			clashed = true
			s.clashes++
		}
	}
}

// segmentsUnder is the folders between dir and a path at or below it, top
// first: none for dir itself.
func segmentsUnder(p, dir string) []string {
	if !below(p, dir) {
		return nil
	}
	return strings.Split(strings.TrimPrefix(strings.TrimPrefix(p, dir), "/"), "/")
}

// leftEmpty is every one of folders that holds nothing at or below it once
// files sit in the folders lands names, deepest first.
//
// Deepest first is not presentation: each folder is removed on its own and
// non-recursively, so a parent can only go after its children have, and a
// folder whose only contents were empty folders is empty too.
func leftEmpty(folders []string, dir string, lands []string) []string {
	holds := map[string]bool{}
	for _, at := range lands {
		for ; below(at, dir) && !holds[at]; at = CleanDir(path.Dir(at)) {
			holds[at] = true
		}
	}

	out := []string{}
	for _, f := range folders {
		if below(f, dir) && !holds[f] {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := depthUnder(out[i], dir), depthUnder(out[j], dir); a != b {
			return a > b
		}
		return out[i] < out[j]
	})
	return out
}
