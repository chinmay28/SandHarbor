package vault

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/thumb"
)

func pictureBytes(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var buf bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A folder and the one below it, with every kind of file a backfill meets.
func thumbFixture(t *testing.T) (*Vault, map[string]string) {
	t.Helper()
	v, _ := newTestVault(t, 3)
	ctx := context.Background()
	if err := v.Mkdirs(MainScope, []string{"/photos/2024", "/elsewhere"}); err != nil {
		t.Fatalf("Mkdirs: %v", err)
	}
	heicData, err := os.ReadFile(filepath.Join("..", "..", "tests", "sample.heic"))
	if err != nil {
		t.Fatalf("reading the sample: %v", err)
	}
	ids := map[string]string{}
	for _, f := range []struct {
		dir, name string
		data      []byte
	}{
		{"/photos", "beach.jpg", pictureBytes(t, "jpeg")},
		{"/photos", "map.png", pictureBytes(t, "png")},
		{"/photos/2024", "IMG_3157.HEIC", heicData},
		{"/photos/2024", "broken.jpg", []byte("not a jpeg at all")},
		{"/photos", "notes.txt", []byte("words")},
		{"/elsewhere", "other.jpg", pictureBytes(t, "jpeg")},
	} {
		entry, _, err := v.Upload(ctx, MainScope, f.dir, f.name, f.data, UploadOptions{})
		if err != nil {
			t.Fatalf("Upload %s: %v", f.name, err)
		}
		ids[f.name] = entry.ID
	}
	return v, ids
}

func TestPlanThumbsCountsWhatTheServerCanDraw(t *testing.T) {
	v, _ := thumbFixture(t)
	// One too large to bother with, recorded without being stored.
	v.mu.Lock()
	v.manifest.Entries = append(v.manifest.Entries, &Entry{
		ID: "huge", Dir: "/photos", Name: "panorama.jpg", MIME: "image/jpeg", Size: thumb.MaxSourceBytes + 1,
	})
	v.mu.Unlock()

	plan, err := v.PlanThumbs(MainScope, "/photos")
	if err != nil {
		t.Fatalf("PlanThumbs: %v", err)
	}
	// beach, map, the HEIC and the broken one; not the text file, not the
	// panorama, not the folder beside.
	if plan.Files != 4 || plan.Folders != 2 || plan.TooLarge != 1 || plan.OverFull != 0 {
		t.Errorf("plan = %+v", plan)
	}
}

func TestMakeThumbsDrawsEveryFileAndCountsWhatFailed(t *testing.T) {
	v, ids := thumbFixture(t)
	// One whose parts are nowhere: it cannot be read back.
	v.mu.Lock()
	v.manifest.Entries = append(v.manifest.Entries, &Entry{
		ID: "partless", Dir: "/photos", Name: "lost.jpg", MIME: "image/jpeg", Size: 10,
	})
	v.mu.Unlock()

	var seen []ThumbRun
	run, err := v.MakeThumbs(context.Background(), MainScope, "/photos", func(r ThumbRun) { seen = append(seen, r) })
	if err != nil {
		t.Fatalf("MakeThumbs: %v", err)
	}
	if run.Total != 5 || run.Done != 5 || run.Made != 3 || run.Undrawn != 1 || run.Unread != 1 ||
		run.Unstored != 0 || run.Failed != 2 || run.Stopped {
		t.Errorf("run = %+v", run)
	}
	if len(run.Errors) != 2 {
		t.Errorf("errors = %+v", run.Errors)
	}
	if len(seen) == 0 || seen[len(seen)-1].Done != 5 {
		t.Errorf("progress was not reported to the end: %d reports", len(seen))
	}

	for _, name := range []string{"beach.jpg", "map.png", "IMG_3157.HEIC"} {
		data, err := v.Thumb(context.Background(), ids[name])
		if err != nil {
			t.Errorf("Thumb(%s): %v", name, err)
			continue
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Errorf("%s: the stored thumbnail is not a JPEG: %v", name, err)
			continue
		}
		if b := img.Bounds(); b.Dx() > thumb.Size || b.Dy() > thumb.Size {
			t.Errorf("%s: thumbnail is %dx%d", name, b.Dx(), b.Dy())
		}
	}
	if _, err := v.Thumb(context.Background(), ids["other.jpg"]); err == nil {
		t.Error("a file outside the folder got a thumbnail")
	}

	// Run again: only the two that failed are tried.
	again, err := v.MakeThumbs(context.Background(), MainScope, "/photos", nil)
	if err != nil || again.Total != 2 || again.Made != 0 {
		t.Errorf("second run = %+v, %v", again, err)
	}
}

// Locked in the middle of a run, it waits, and finishes once unlocked —
// nothing counted as failed for the wait.
func TestMakeThumbsWaitsOutALock(t *testing.T) {
	old := thumbLockPoll
	thumbLockPoll = 10 * time.Millisecond
	t.Cleanup(func() { thumbLockPoll = old })

	v, _ := thumbFixture(t)
	var once sync.Once
	sawLocked := false
	unlocked := make(chan struct{})
	run, err := v.MakeThumbs(context.Background(), MainScope, "/photos", func(r ThumbRun) {
		if r.Locked {
			sawLocked = true
		}
		if r.Done == 1 {
			once.Do(func() {
				v.Lock()
				go func() {
					time.Sleep(50 * time.Millisecond)
					if err := v.Unlock(testPassword); err != nil {
						t.Errorf("Unlock: %v", err)
					}
					close(unlocked)
				}()
			})
		}
	})
	<-unlocked
	if err != nil {
		t.Fatalf("MakeThumbs: %v", err)
	}
	if !sawLocked {
		t.Error("the wait for an unlock was never reported")
	}
	if run.Done != 4 || run.Made != 3 || run.Undrawn != 1 || run.Unread != 0 || run.Failed != 1 || run.Stopped {
		t.Errorf("run = %+v", run)
	}
}

func TestMakeThumbsStopsWhenAsked(t *testing.T) {
	v, _ := thumbFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := v.MakeThumbs(ctx, MainScope, "/photos", func(r ThumbRun) {
		if r.Done == 1 {
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("MakeThumbs: %v", err)
	}
	if !run.Stopped || run.Done >= run.Total {
		t.Errorf("run = %+v", run)
	}
}

func TestMakeThumbsOfAMissingFolderIsAnError(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if _, err := v.MakeThumbs(context.Background(), MainScope, "/nowhere", nil); err == nil {
		t.Error("a folder that does not exist was run")
	}
}

// A run that starts while the vault is locked waits for the unlock too.
func TestMakeThumbsStartedWhileLockedWaits(t *testing.T) {
	old := thumbLockPoll
	thumbLockPoll = 10 * time.Millisecond
	t.Cleanup(func() { thumbLockPoll = old })

	v, _ := thumbFixture(t)
	v.Lock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := v.Unlock(testPassword); err != nil {
			t.Errorf("Unlock: %v", err)
		}
	}()
	run, err := v.MakeThumbs(context.Background(), MainScope, "/photos", nil)
	if err != nil {
		t.Fatalf("MakeThumbs: %v", err)
	}
	if run.Total != 4 || run.Made != 3 || run.Failed != 1 {
		t.Errorf("run = %+v", run)
	}
}
