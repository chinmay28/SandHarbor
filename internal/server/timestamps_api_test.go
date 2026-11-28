package server

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// The timestamp plan, decoded into what the browser reads off it.
type timePlanBody struct {
	Path  string `json:"path"`
	Deep  bool   `json:"deep"`
	Fixes []struct {
		ID      string    `json:"id"`
		Name    string    `json:"name"`
		Dir     string    `json:"dir"`
		Was     time.Time `json:"was"`
		To      time.Time `json:"to"`
		DayOnly bool      `json:"day_only"`
	} `json:"fixes"`
	Readable []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"readable"`
	Settled int `json:"settled"`
	Unknown int `json:"unknown"`
}

// What POST /api/folders/timestamps answers with.
type timeFixBody struct {
	Fixed   int      `json:"fixed"`
	Settled int      `json:"settled"`
	Undated int      `json:"undated"`
	Missing []string `json:"missing"`
	Failed  []string `json:"failed"`
}

func (c *testClient) timePlan(t *testing.T, dir, query string) timePlanBody {
	t.Helper()

	w := c.do(http.MethodGet, "/api/folders/timestamps?path="+url.QueryEscape(dir)+query, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("timestamp plan %s: %d %s", dir, w.Code, w.Body.String())
	}
	var out timePlanBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("timestamp plan %s: %v", dir, err)
	}
	return out
}

func (c *testClient) fixTimes(t *testing.T, body map[string]any) timeFixBody {
	t.Helper()

	w, _ := c.json(http.MethodPost, "/api/folders/timestamps", body)
	if w.Code != http.StatusOK {
		t.Fatalf("fix timestamps: %d %s", w.Code, w.Body.String())
	}
	var out timeFixBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("fix timestamps: %v", err)
	}
	return out
}

// clip is the least of an MP4 that says when it was recorded.
func clip(made time.Time) []byte {
	box := func(kind string, body []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(body)+8))
		return append(append(out, kind...), body...)
	}
	epoch := time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	header := binary.BigEndian.AppendUint32([]byte{0, 0, 0, 0}, uint32(made.Sub(epoch)/time.Second))
	header = append(header, make([]byte, 92)...)

	out := box("ftyp", []byte("isom\x00\x00\x02\x00isom"))
	out = append(out, box("mdat", make([]byte, 2048))...)
	return append(out, box("moov", box("mvhd", header))...)
}

// The whole round trip, as the browser makes it: a folder of files all dated
// the afternoon they were uploaded is planned, the names are applied, the rest
// are opened, and asking again finds nothing left to do.
func TestTimestampsArePlannedFixedAndThenSettled(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	if w, body := c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/roll"}); w.Code != http.StatusCreated {
		t.Fatalf("create /roll: %d %v", w.Code, body)
	}
	uploaded := time.Date(2026, time.March, 1, 15, 0, 0, 0, time.UTC)
	recorded := time.Date(2022, time.August, 9, 18, 30, 0, 0, time.UTC)
	c.uploadAt("IMG_20230514_101502.jpg", "/roll", []byte("one"), uploaded)
	c.uploadAt("IMG-20190704-WA0003.jpg", "/roll", []byte("two"), uploaded)
	c.uploadAt("IMG_20260301_140000.jpg", "/roll", []byte("three"), uploaded)
	c.uploadAt("holiday.mp4", "/roll", clip(recorded), uploaded)
	c.uploadAt("screen.mov", "/roll", []byte("no header in here"), uploaded)
	c.uploadAt("notes.txt", "/roll", []byte("four"), uploaded)

	// Two hours east: the names are that wall's clock.
	plan := c.timePlan(t, "/roll", "&offset=120")
	if len(plan.Fixes) != 2 || len(plan.Readable) != 2 || plan.Settled != 1 || plan.Unknown != 1 {
		t.Fatalf("plan: %d to fix, %d to read, %d settled, %d unknown; want 2, 2, 1, 1: %+v",
			len(plan.Fixes), len(plan.Readable), plan.Settled, plan.Unknown, plan)
	}
	if got := plan.Fixes[0]; got.Name != "IMG-20190704-WA0003.jpg" || !got.DayOnly ||
		!got.To.Equal(time.Date(2019, time.July, 4, 10, 0, 0, 0, time.UTC)) || !got.Was.Equal(uploaded) {
		t.Errorf("the day-only name: %+v", got)
	}
	if got := plan.Fixes[1]; !got.To.Equal(time.Date(2023, time.May, 14, 8, 15, 2, 0, time.UTC)) {
		t.Errorf("the timed name goes to %s, want 08:15:02 UTC", got.To)
	}

	set := []map[string]any{}
	for _, fix := range plan.Fixes {
		set = append(set, map[string]any{"id": fix.ID, "modified": fix.To})
	}
	read := []string{"no-such-file"}
	for _, file := range plan.Readable {
		read = append(read, file.ID)
	}
	got := c.fixTimes(t, map[string]any{"set": set, "read": read, "offset": 120})
	if got.Fixed != 3 || got.Undated != 1 || got.Settled != 0 || len(got.Failed) != 0 ||
		len(got.Missing) != 1 || got.Missing[0] != "no-such-file" {
		t.Errorf("fixing: %+v; want three fixed, one with no date, one missing", got)
	}

	// The listing shows the dates the files were made.
	_, listing := c.json(http.MethodGet, "/api/files?path=/roll", nil)
	when := map[string]string{}
	for _, raw := range listing["files"].([]any) {
		file := raw.(map[string]any)
		when[file["name"].(string)], _ = file["modified_at"].(string)
	}
	for name, want := range map[string]time.Time{
		"IMG_20230514_101502.jpg": time.Date(2023, time.May, 14, 8, 15, 2, 0, time.UTC),
		"holiday.mp4":             recorded,
		"screen.mov":              uploaded,
		"notes.txt":               uploaded,
	} {
		got, err := time.Parse(time.RFC3339, when[name])
		if err != nil || !got.Equal(want) {
			t.Errorf("%s is dated %q, want %s", name, when[name], want)
		}
	}

	// Nothing is left that a name can fix, and what was read agrees now.
	again := c.timePlan(t, "/roll", "&offset=120")
	if len(again.Fixes) != 0 || again.Settled != 3 {
		t.Errorf("asked again: %+v; want nothing to fix and three settled", again)
	}
	if got := c.fixTimes(t, map[string]any{"read": read[1:]}); got.Fixed != 0 || got.Settled != 1 || got.Undated != 1 {
		t.Errorf("read again: %+v; want one settled and one with no date", got)
	}
}

func TestTimestampsRefuseWhatIsNotABatch(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	tooMany := make([]string, maxTimeReads+1)
	for why, body := range map[string]map[string]any{
		"nothing asked":      {},
		"a set with no time": {"set": []map[string]any{{"id": "abc"}}},
		"a set with no file": {"set": []map[string]any{{"modified": "2023-05-14T10:15:02Z"}}},
		"too many to read":   {"read": tooMany},
	} {
		if w, _ := c.json(http.MethodPost, "/api/folders/timestamps", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", why, w.Code)
		}
	}
	if w := c.do(http.MethodGet, "/api/folders/timestamps?path=/&offset=soon", nil, ""); w.Code != http.StatusBadRequest {
		t.Errorf("an offset that is not a number: %d, want 400", w.Code)
	}
	if w := c.do(http.MethodGet, "/api/folders/timestamps?path=/nowhere", nil, ""); w.Code == http.StatusOK {
		t.Errorf("a folder that is not there was planned")
	}
}
