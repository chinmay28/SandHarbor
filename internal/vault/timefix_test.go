package vault

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"
)

// clipMade is the least of an MP4 that states when it was made: a movie header
// counting seconds from 1904, after a stretch of picture.
func clipMade(made time.Time) []byte {
	box := func(kind string, body []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(body)+8))
		return append(append(out, kind...), body...)
	}
	epoch := time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	header := binary.BigEndian.AppendUint32([]byte{0, 0, 0, 0}, uint32(made.Sub(epoch)/time.Second))
	header = append(header, make([]byte, 92)...)

	out := box("ftyp", []byte("isom\x00\x00\x02\x00isom"))
	out = append(out, box("mdat", make([]byte, 4096))...)
	return append(out, box("moov", box("mvhd", header))...)
}

// kept puts one file with given bytes and a given modified time in a folder.
func kept(t *testing.T, v *Vault, dir, name string, data []byte, when time.Time) *Entry {
	t.Helper()
	if !v.FolderExists(MainScope, dir) {
		if err := v.Mkdir(MainScope, dir); err != nil {
			t.Fatalf("Mkdir %s: %v", dir, err)
		}
	}
	e, _, err := v.Upload(context.Background(), MainScope, dir, name, data, UploadOptions{ModifiedAt: when})
	if err != nil {
		t.Fatalf("Upload %s/%s: %v", dir, name, err)
	}
	return e
}

func timePlan(t *testing.T, v *Vault, dir string, opts TimeFixOptions) *TimePlan {
	t.Helper()
	plan, err := v.TimeFixes(MainScope, dir, opts)
	if err != nil {
		t.Fatalf("TimeFixes %s: %v", dir, err)
	}
	return plan
}

// The plan, from plain entries: which a name corrects, which are worth
// opening, which are left alone.
func TestPlanTimeFixesSortsFilesByWhatCanBeSaidOfThem(t *testing.T) {
	uploaded := at("2026-03-01T15:00:00Z")
	now := at("2026-11-28T12:00:00Z")
	files := []*Entry{
		{ID: "wrong", Dir: "/p", Name: "IMG_20230514_101502.jpg", Size: 10, ModifiedAt: uploaded},
		{ID: "right", Dir: "/p", Name: "IMG_20230515_101502.jpg", ModifiedAt: at("2023-05-15T09:00:00Z")},
		{ID: "day", Dir: "/p", Name: "2019-07-04 statement.pdf", ModifiedAt: uploaded},
		{ID: "none", Dir: "/p", Name: "VID_20200101_000000.mp4"},
		{ID: "open", Dir: "/p", Name: "IMG_1234.HEIC", Size: 7, ModifiedAt: uploaded},
		{ID: "clip", Dir: "/a", Name: "holiday.mov", ModifiedAt: uploaded},
		{ID: "text", Dir: "/p", Name: "notes.txt", ModifiedAt: uploaded},
		// Due next year: a date, but not the date of the file.
		{ID: "due", Dir: "/p", Name: "invoice due 2027-12-01.pdf", ModifiedAt: uploaded},
		{ID: "soon", Dir: "/p", Name: "renewal 2027-12-01.jpg", ModifiedAt: uploaded},
	}

	plan := planTimeFixes(files, time.UTC, now)

	want := []TimeFix{
		{ID: "day", Name: "2019-07-04 statement.pdf", Dir: "/p", Was: uploaded, To: at("2019-07-04T12:00:00Z"), DayOnly: true},
		{ID: "wrong", Name: "IMG_20230514_101502.jpg", Dir: "/p", Size: 10, Was: uploaded, To: at("2023-05-14T10:15:02Z")},
		{ID: "none", Name: "VID_20200101_000000.mp4", Dir: "/p", To: at("2020-01-01T00:00:00Z"), Undated: true},
	}
	if !reflect.DeepEqual(plan.Fixes, want) {
		t.Errorf("fixes:\n got %+v\nwant %+v", plan.Fixes, want)
	}
	readable := []TimeCandidate{
		{ID: "clip", Name: "holiday.mov", Dir: "/a"},
		{ID: "open", Name: "IMG_1234.HEIC", Dir: "/p", Size: 7},
		{ID: "soon", Name: "renewal 2027-12-01.jpg", Dir: "/p"},
	}
	if !reflect.DeepEqual(plan.Readable, readable) {
		t.Errorf("readable:\n got %+v\nwant %+v", plan.Readable, readable)
	}
	if plan.Settled != 1 || plan.Unknown != 2 {
		t.Errorf("settled %d and unknown %d, want 1 and 2", plan.Settled, plan.Unknown)
	}
}

// A name's wall clock is the viewer's, so the same name is a different instant
// from a different zone.
func TestPlanTimeFixesReadsNamesInTheViewersZone(t *testing.T) {
	files := []*Entry{{ID: "a", Dir: "/", Name: "IMG_20230514_101502.jpg", ModifiedAt: at("2026-03-01T15:00:00Z")}}

	plan := planTimeFixes(files, time.FixedZone("", -7*3600), at("2026-11-28T12:00:00Z"))

	if len(plan.Fixes) != 1 || !plan.Fixes[0].To.Equal(at("2023-05-14T17:15:02Z")) {
		t.Errorf("got %+v, want 10:15 seven hours west of UTC", plan.Fixes)
	}
}

func TestTimeFixesLooksAtThisFolderOrEverythingUnderIt(t *testing.T) {
	v, _ := newTestVault(t, 3)
	uploaded := at("2026-03-01T15:00:00Z")
	kept(t, v, "/photos", "IMG_20230514_101502.jpg", []byte("a"), uploaded)
	kept(t, v, "/photos/trip", "IMG_20230601_090000.jpg", []byte("b"), uploaded)
	kept(t, v, "/elsewhere", "IMG_20230701_090000.jpg", []byte("c"), uploaded)

	shallow := timePlan(t, v, "/photos", TimeFixOptions{})
	if len(shallow.Fixes) != 1 || shallow.Fixes[0].Name != "IMG_20230514_101502.jpg" {
		t.Errorf("this folder alone: %+v", shallow.Fixes)
	}
	deep := timePlan(t, v, "/photos", TimeFixOptions{Deep: true})
	if len(deep.Fixes) != 2 || deep.Path != "/photos" || !deep.Deep {
		t.Errorf("everything under it: %+v", deep)
	}

	if _, err := v.TimeFixes(MainScope, "/nowhere", TimeFixOptions{}); err == nil {
		t.Error("a folder that is not there was planned")
	}
	if _, err := v.TimeFixes(MainScope, "/photos", TimeFixOptions{Offset: 2000}); err == nil {
		t.Error("an offset of more than a day was accepted")
	}
}

// Running a plan settles it: the same question afterwards has nothing to fix.
func TestSetTimesAppliesAPlanAndAskingAgainFindsNothing(t *testing.T) {
	v, _ := newTestVault(t, 3)
	uploaded := at("2026-03-01T15:00:00Z")
	entry := kept(t, v, "/photos", "IMG_20230514_101502.jpg", []byte("a"), uploaded)
	created, shards := entry.CreatedAt, len(entry.Shards)

	plan := timePlan(t, v, "/photos", TimeFixOptions{})
	orders := []TimeOrder{{ID: "no-such-file", Modified: uploaded}, {ID: entry.ID}}
	for _, fix := range plan.Fixes {
		orders = append(orders, TimeOrder{ID: fix.ID, Modified: fix.To})
	}

	report, err := v.SetTimes(orders)
	if err != nil {
		t.Fatalf("SetTimes: %v", err)
	}
	if report.Fixed != 1 || !reflect.DeepEqual(report.Missing, []string{"no-such-file"}) {
		t.Errorf("report %+v, want one fixed and one missing", report)
	}

	// Only the modified time moved, and it survives a reload.
	v = reopen(t, v)
	got := v.manifest.ByID(entry.ID)
	if got == nil || !got.ModifiedAt.Equal(at("2023-05-14T10:15:02Z")) {
		t.Fatalf("after reopening the time is %v", got)
	}
	if !got.CreatedAt.Equal(created) || len(got.Shards) != shards {
		t.Errorf("more than the modified time changed: %+v", got)
	}

	again := timePlan(t, v, "/photos", TimeFixOptions{})
	if len(again.Fixes) != 0 || again.Settled != 1 {
		t.Errorf("asked again: %+v, want nothing to fix and one settled", again)
	}
	if report, err := v.SetTimes(orders); err != nil || report.Fixed != 0 {
		t.Errorf("applied again: %+v, %v; want nothing changed", report, err)
	}
}

func TestSetTimesOnALockedVaultIsRefused(t *testing.T) {
	v, _ := newTestVault(t, 3)
	entry := kept(t, v, "/", "a.jpg", []byte("a"), time.Time{})
	v.Lock()

	if _, err := v.SetTimes([]TimeOrder{{ID: entry.ID, Modified: time.Now()}}); !errors.Is(err, ErrLocked) {
		t.Errorf("got %v, want ErrLocked", err)
	}
	if _, err := v.FixTimesFromContent(context.Background(), []string{entry.ID}, TimeFixOptions{}); !errors.Is(err, ErrLocked) {
		t.Errorf("reading: got %v, want ErrLocked", err)
	}
}

// The date inside the file, read back from the accounts holding it.
func TestFixTimesFromContentCorrectsWhatIsOutAndLeavesTheRest(t *testing.T) {
	v, _ := newTestVault(t, 3)
	uploaded := at("2026-03-01T15:00:00Z")
	made := at("2022-08-09T18:30:00Z")

	wrong := kept(t, v, "/clips", "holiday.mp4", clipMade(made), uploaded)
	right := kept(t, v, "/clips", "beach.mp4", clipMade(made), made.Add(40*time.Minute))
	blank := kept(t, v, "/clips", "screen.mp4", []byte("not a movie at all"), uploaded)
	unset := kept(t, v, "/clips", "dashcam.mov", clipMade(time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)), uploaded)
	other := kept(t, v, "/clips", "notes.txt", clipMade(made), uploaded)

	ids := []string{wrong.ID, right.ID, "no-such-file", blank.ID, unset.ID, other.ID}
	reading, err := v.FixTimesFromContent(context.Background(), ids, TimeFixOptions{})
	if err != nil {
		t.Fatalf("FixTimesFromContent: %v", err)
	}

	want := &TimeReading{Fixed: 1, Settled: 1, Undated: 3, Missing: []string{"no-such-file"}, Failed: []string{}}
	if !reflect.DeepEqual(reading, want) {
		t.Errorf("reading %+v, want %+v", reading, want)
	}
	if !wrong.ModifiedAt.Equal(made) {
		t.Errorf("the file that was out says %s, want %s", wrong.ModifiedAt, made)
	}
	if !right.ModifiedAt.Equal(made.Add(40 * time.Minute)) {
		t.Errorf("a file within a day of its date was moved to %s", right.ModifiedAt)
	}
	for _, e := range []*Entry{blank, unset, other} {
		if !e.ModifiedAt.Equal(uploaded) {
			t.Errorf("%s carries no date and was moved to %s", e.Name, e.ModifiedAt)
		}
	}

	// Asked again, everything that has a date agrees with it.
	again, err := v.FixTimesFromContent(context.Background(), ids, TimeFixOptions{})
	if err != nil || again.Fixed != 0 || again.Settled != 2 {
		t.Errorf("asked again: %+v, %v; want nothing fixed and two settled", again, err)
	}
}

// A file whose parts cannot be had is listed, and the files beside it are
// still corrected.
func TestFixTimesFromContentListsAFileItCouldNotRead(t *testing.T) {
	v, _ := newTestVault(t, 3)
	made := at("2022-08-09T18:30:00Z")
	good := kept(t, v, "/clips", "holiday.mp4", clipMade(made), at("2026-03-01T15:00:00Z"))
	lost := kept(t, v, "/clips", "lost.mp4", clipMade(made), at("2026-03-01T15:00:00Z"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reading, err := v.FixTimesFromContent(ctx, []string{good.ID, lost.ID}, TimeFixOptions{})
	if err != nil {
		t.Fatalf("FixTimesFromContent: %v", err)
	}
	if reading.Fixed != 0 || len(reading.Failed) != 2 {
		t.Errorf("a batch that was stopped: %+v, want both listed as not read", reading)
	}
	if good.ModifiedAt.Equal(made) {
		t.Error("a file that was not read was retimed")
	}
}
