package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"
)

// The flatten plan, decoded into what the browser reads off it.
type flattenPlanBody struct {
	Path    string `json:"path"`
	Keep    int    `json:"keep"`
	From    string `json:"from"`
	Depth   int    `json:"depth"`
	Folders []struct {
		Path   string `json:"path"`
		Label  string `json:"label"`
		Exists bool   `json:"exists"`
	} `json:"folders"`
	Moves []struct {
		ID string `json:"id"`
		To string `json:"to"`
		As string `json:"as"`
	} `json:"moves"`
	Kept    int      `json:"kept"`
	Clashes int      `json:"clashes"`
	Emptied []string `json:"emptied"`
}

func (c *testClient) flattenPlan(t *testing.T, dir, query string) flattenPlanBody {
	t.Helper()

	w := c.do(http.MethodGet, "/api/folders/flatten?path="+url.QueryEscape(dir)+query, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("flatten plan %s: %d %s", dir, w.Code, w.Body.String())
	}
	var out flattenPlanBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("flatten plan %s: %v", dir, err)
	}
	return out
}

// runFlatten does what the browser does with a plan: make the folders that are
// not there, move the files in one batch, remove what was emptied one folder at
// a time and never recursively.
func (c *testClient) runFlatten(t *testing.T, plan flattenPlanBody) {
	t.Helper()

	var paths []string
	for _, f := range plan.Folders {
		if !f.Exists {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) > 0 {
		if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"paths": paths}); w.Code != http.StatusCreated {
			t.Fatalf("create %v: %d %v", paths, w.Code, body)
		}
	}

	moves := []map[string]any{}
	for _, m := range plan.Moves {
		moves = append(moves, map[string]any{"id": m.ID, "dir": m.To, "name": m.As})
	}
	if got := c.moveBatch(t, moves); got.Moved != len(moves) || len(got.Refused)+len(got.Missing) > 0 {
		t.Fatalf("batch moved %d of %d: %+v", got.Moved, len(moves), got)
	}

	for _, dir := range plan.Emptied {
		if w, body := c.json(http.MethodDelete, "/api/folders?path="+url.QueryEscape(dir), nil); w.Code != http.StatusOK {
			t.Fatalf("remove %s: %d %v", dir, w.Code, body)
		}
	}
}

// where everything under a folder is, as sorted paths relative to nothing.
func (c *testClient) under(t *testing.T, dir string) []string {
	t.Helper()

	out := []string{}
	s := c.survey(t, dir)
	for _, f := range s.Files {
		out = append(out, f.Dir+"/"+f.Name)
	}
	for _, f := range s.Folders {
		out = append(out, f.Path+"/")
	}
	sort.Strings(out)
	return out
}

var discs = []string{
	"/music/abba/cd1/one.flac",
	"/music/abba/cd2/one.flac",
	"/music/queen/cd1/one.flac",
	"/music/queen/cd1/two.flac",
}

func TestFlattenPlansRunOverTheOrdinaryEndpoints(t *testing.T) {
	cases := []struct {
		name, query string
		want        []string
	}{
		{
			"all the way", "",
			[]string{"/music/one (2).flac", "/music/one (3).flac", "/music/one.flac", "/music/two.flac"},
		},
		{
			"keeping the top level", "&keep=1",
			[]string{
				"/music/abba/", "/music/abba/one (2).flac", "/music/abba/one.flac",
				"/music/queen/", "/music/queen/one.flac", "/music/queen/two.flac",
			},
		},
		{
			"keeping the leaf level, folders apart", "&keep=1&from=leaf",
			[]string{
				"/music/cd1 (2)/", "/music/cd1 (2)/one.flac", "/music/cd1 (2)/two.flac",
				"/music/cd1/", "/music/cd1/one.flac",
				"/music/cd2/", "/music/cd2/one.flac",
			},
		},
		{
			"keeping the leaf level, folders merged", "&keep=1&from=leaf&merge=1",
			[]string{
				"/music/cd1/", "/music/cd1/one (2).flac", "/music/cd1/one.flac", "/music/cd1/two.flac",
				"/music/cd2/", "/music/cd2/one.flac",
			},
		},
		{
			"merged, and named after what was lost", "&keep=1&from=leaf&merge=1&prefix=1",
			[]string{
				"/music/cd1/", "/music/cd1/abba - one.flac", "/music/cd1/queen - one.flac", "/music/cd1/queen - two.flac",
				"/music/cd2/", "/music/cd2/abba - one.flac",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t)
			c.setup("pw", 3)
			c.grow(t, discs...)

			c.runFlatten(t, c.flattenPlan(t, "/music", tc.query))

			if got := c.under(t, "/music"); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("after the flatten:\n got  %q\n want %q", got, tc.want)
			}
			// Pressing it again finds nothing to do.
			if again := c.flattenPlan(t, "/music", tc.query); len(again.Moves) != 0 || len(again.Emptied) != 0 {
				t.Errorf("a second plan moves %d files and removes %v", len(again.Moves), again.Emptied)
			}
		})
	}
}

func TestFlattenPlanRefusesABadQuestion(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.grow(t, "/music/abba/one.flac")

	for _, query := range []string{"&keep=some", "&keep=-1", "&from=middle"} {
		w := c.do(http.MethodGet, "/api/folders/flatten?path=%2Fmusic"+query, nil, "")
		if w.Code < 400 || w.Code >= 500 {
			t.Errorf("%s answered %d, want a refusal", query, w.Code)
		}
	}
	if w := c.do(http.MethodGet, "/api/folders/flatten?path=%2Fnowhere", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("a folder that is not there answered %d, want 404", w.Code)
	}
}
