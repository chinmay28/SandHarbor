package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Uploading an archive to be unpacked rather than stored.
//
// The archive never lands: a folder named after it does, holding what it
// held. The paths inside it are its author's word and get the same distrust a
// dropped folder's do.
//
// The upload only hands the archive over. Unpacking it is a run the server
// carries out afterwards, so these tests upload and then wait for the run.

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range sortedKeys(files) {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name: name, Method: zip.Deflate,
			Modified: time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(files[name]))
	}
	zw.Close()
	return buf.Bytes()
}

func tarGzOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range sortedKeys(files) {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}
		if strings.HasSuffix(name, "/") {
			hdr.Typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(files[name]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// uploadExtract posts one archive the way the browser does when the box is
// ticked: the file, and "extract-0" beside it.
func (c *testClient) uploadExtract(name, dir string, content []byte) (int, []map[string]any) {
	c.t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("files[]", name)
	if err != nil {
		c.t.Fatalf("CreateFormFile: %v", err)
	}
	part.Write(content)
	mw.WriteField("extract-0", "1")
	mw.WriteField("path", dir)
	mw.Close()

	w := c.do(http.MethodPost, "/api/files", &buf, mw.FormDataContentType())
	var resp struct {
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Results
}

// extracted waits for the run an upload result names to be over, and returns
// it as GET /api/extracts lists it.
func (c *testClient) extracted(result map[string]any) map[string]any {
	c.t.Helper()
	id, _ := result["run"].(string)
	if ok, _ := result["ok"].(bool); !ok || id == "" {
		c.t.Fatalf("result = %v, want the archive taken and a run named", result)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, body := c.json(http.MethodGet, "/api/extracts", nil)
		runs, _ := body["runs"].([]any)
		for _, raw := range runs {
			run := raw.(map[string]any)
			if done, _ := run["done"].(bool); done && run["id"] == id {
				return run
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("run %s never finished", id)
	return nil
}

// extract uploads one archive to be unpacked and waits for it to have been.
func (c *testClient) extract(name, dir string, content []byte) map[string]any {
	c.t.Helper()
	code, results := c.uploadExtract(name, dir, content)
	if code != http.StatusCreated || len(results) != 1 {
		c.t.Fatalf("upload %s: %d %v", name, code, results)
	}
	return c.extracted(results[0])
}

// reportOf is a finished run's report.
func reportOf(run map[string]any) map[string]any {
	report, _ := run["report"].(map[string]any)
	return report
}

// names lists a folder's files, sorted.
func (c *testClient) names(path string) string {
	c.t.Helper()
	names := c.listNames(c.t, "", path)
	sort.Strings(names)
	return strings.Join(names, " ")
}

func TestExtractUnpacksAnArchiveIntoAFolderNamedAfterIt(t *testing.T) {
	archives := map[string]func(*testing.T, map[string]string) []byte{
		"trip.zip":    zipOf,
		"trip.tar.gz": tarGzOf,
		"trip.tgz":    tarGzOf,
	}
	for name, build := range archives {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t)
			c.setup("pw", 3)
			c.mkdir("/inbox")

			run := c.extract(name, "/inbox", build(t, map[string]string{
				"readme.txt":         "hello",
				"day1/hike.txt":      "a ridge",
				"day1/night/sky.txt": "stars",
			}))
			if run["name"] != name || run["dir"] != "/inbox/trip" || run["error"] != nil {
				t.Errorf("run = %v, want %s unpacked into /inbox/trip", run, name)
			}
			if got := reportOf(run)["extracted"]; got != float64(3) {
				t.Errorf("extracted = %v, want 3", got)
			}

			got := strings.Join(c.folderPaths(), " ")
			if want := "/ /inbox /inbox/trip /inbox/trip/day1 /inbox/trip/day1/night"; got != want {
				t.Errorf("folders = %q, want %q", got, want)
			}
			if got := c.names("/inbox"); got != "" {
				t.Errorf("/inbox holds %q — the archive itself is not stored", got)
			}
			if got := c.names("/inbox/trip"); got != "readme.txt" {
				t.Errorf("/inbox/trip holds %q", got)
			}
			if got := c.names("/inbox/trip/day1/night"); got != "sky.txt" {
				t.Errorf("night holds %q", got)
			}

			// What came out is what went in.
			file := c.fileAt("/inbox/trip/day1", "hike.txt")
			w := c.do(http.MethodGet, "/api/files/"+file["id"].(string)+"/content", nil, "")
			if w.Body.String() != "a ridge" {
				t.Errorf("downloaded %q, want the file's own contents", w.Body.String())
			}
		})
	}
}

func TestExtractKeepsTheTimesTheArchiveRecorded(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.extract("old.zip", "/", zipOf(t, map[string]string{"photo.txt": "x"}))
	if got := c.modified("/old", "photo.txt"); got.Year() != 2019 {
		t.Errorf("modified = %v, want the 2019 the archive recorded", got)
	}
}

// project-1.0.tar.gz holding project-1.0/ is how most archives are made, and
// unpacking it to project-1.0/project-1.0 is what nobody wants.
func TestExtractDoesNotNestAFolderNamedForTheArchive(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.extract("project-1.0.tar.gz", "/", tarGzOf(t, map[string]string{
		"project-1.0/":            "",
		"project-1.0/main.go":     "package main",
		"project-1.0/docs/":       "",
		"project-1.0/docs/use.md": "use it",
	}))
	got := strings.Join(c.folderPaths(), " ")
	if want := "/ /project-1.0 /project-1.0/docs"; got != want {
		t.Errorf("folders = %q, want %q", got, want)
	}
	if got := c.names("/project-1.0"); got != "main.go" {
		t.Errorf("/project-1.0 holds %q", got)
	}
}

func TestExtractKeepsAnEmptyFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	run := c.extract("shell.tgz", "/", tarGzOf(t, map[string]string{"drafts/old/": ""}))
	if run["error"] != nil {
		t.Fatalf("run = %v — an archive of folders is still an archive that unpacked", run)
	}
	if got := strings.Join(c.folderPaths(), " "); got != "/ /shell /shell/drafts /shell/drafts/old" {
		t.Errorf("folders = %q", got)
	}
}

func TestExtractRefusesAPathThatClimbsOut(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.mkdir("/inbox")

	run := c.extract("evil.zip", "/inbox", zipOf(t, map[string]string{
		"../escaped.txt":        "out",
		"/etc/escaped.txt":      "root",
		"a/../../../escape.txt": "far",
		"fine.txt":              "in",
	}))

	report := reportOf(run)
	if report["extracted"] != float64(1) || report["failed"] != float64(3) {
		t.Errorf("report = %v, want fine.txt stored and the three others refused", report)
	}
	failures, _ := report["failures"].([]any)
	if len(failures) != 3 {
		t.Fatalf("failures = %v, want each refusal named", failures)
	}
	for _, line := range failures {
		if !strings.Contains(line.(string), "escape") {
			t.Errorf("failure %q does not name the entry", line)
		}
	}

	if got := strings.Join(c.folderPaths(), " "); got != "/ /inbox /inbox/evil" {
		t.Errorf("folders = %q — the tree grew somewhere it should not have", got)
	}
	if got := c.names("/") + c.names("/inbox"); got != "" {
		t.Errorf("%q landed outside the archive's folder", got)
	}
}

func TestExtractLeavesOutLinksAndSaysSo(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "real.txt", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
	tw.Write([]byte("kept"))
	tw.WriteHeader(&tar.Header{Name: "shadow", Linkname: "/etc/shadow", Typeflag: tar.TypeSymlink})
	tw.Close()
	gz.Close()

	report := reportOf(c.extract("mixed.tar.gz", "/", buf.Bytes()))
	if got := c.names("/mixed"); got != "real.txt" {
		t.Errorf("/mixed holds %q", got)
	}
	skips, _ := report["skips"].([]any)
	if report["skipped"] != float64(1) || len(skips) != 1 || skips[0] != "shadow (a symbolic link)" {
		t.Errorf("report = %v, want the link named", report)
	}
}

func TestExtractRefusesWhatIsNotAnArchive(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	// By name: nothing says what a .txt would unpack as, and the upload
	// itself is told so.
	code, results := c.uploadExtract("notes.txt", "/", []byte("plain"))
	if code != http.StatusBadGateway || results[0]["stage"] != "placing" || results[0]["run"] != nil {
		t.Errorf("notes.txt: %d %v", code, results)
	}
	// By contents: named like one, but not one — which only reading it finds.
	run := c.extract("fake.zip", "/", []byte("plain"))
	if msg, _ := run["error"].(string); !strings.Contains(msg, "not a readable zip") {
		t.Errorf("fake.zip: %v", run)
	}
	if got := c.names("/"); got != "" {
		t.Errorf("%q was stored — asked to extract, it is not stored as it is instead", got)
	}
	if got := strings.Join(c.folderPaths(), " "); got != "/" {
		t.Errorf("folders = %q — nothing unpacked, so nothing made", got)
	}
}

func TestExtractRefusesAnArchiveThatUnpacksToTooMuch(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.server.MaxExtractSize = 1000

	// A few hundred bytes on the wire, well over the limit once out.
	run := c.extract("bomb.zip", "/", zipOf(t, map[string]string{
		"zeros.bin": strings.Repeat("0", 100_000),
	}))
	if msg, _ := run["error"].(string); !strings.Contains(msg, "unpacks to more than") {
		t.Errorf("error = %q", msg)
	}
	if got := strings.Join(c.folderPaths(), " "); got != "/" {
		t.Errorf("folders = %q — an archive over the limit leaves nothing behind", got)
	}
}

// Extracting is asked for file by file, so an archive can be unpacked in the
// same request that stores another file as it is.
func TestExtractIsPerFile(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, name := range []string{"keep.zip", "open.zip"} {
		part, _ := mw.CreateFormFile("files[]", name)
		part.Write(zipOf(t, map[string]string{"inside.txt": "x"}))
	}
	mw.WriteField("extract-1", "1")
	mw.WriteField("path", "/")
	mw.Close()

	w := c.do(http.MethodPost, "/api/files", &buf, mw.FormDataContentType())
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Stored  int              `json:"stored"`
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Stored != 1 || resp.Results[0]["run"] != nil {
		t.Errorf("response = %+v, want the first archive stored as it is", resp)
	}
	c.extracted(resp.Results[1])

	if got := c.names("/"); got != "keep.zip" {
		t.Errorf("/ holds %q, want only the archive that was not opened", got)
	}
	if got := c.names("/open"); got != "inside.txt" {
		t.Errorf("/open holds %q", got)
	}
}

// The point of the run: the request that brought the archive is long over, and
// the page that made it may be gone, while the unpacking is still going.
func TestExtractCarriesOnAfterTheUploadHasBeenAnswered(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	// Something already has the turn, so the archive uploaded next can only
	// be waiting when its upload is answered.
	hold := newGate()
	c.server.extracts.add(extractRun{Name: "ahead.zip"}, hold.work, func() {})
	<-hold.started

	code, results := c.uploadExtract("later.zip", "/", zipOf(t, map[string]string{"a.txt": "x"}))
	if code != http.StatusCreated || results[0]["run"] == nil {
		t.Fatalf("upload: %d %v — the archive is taken whether or not its turn has come", code, results)
	}
	if got := strings.Join(c.folderPaths(), " "); got != "/" {
		t.Errorf("folders = %q — nothing is unpacked before its turn", got)
	}
	_, body := c.json(http.MethodGet, "/api/extracts", nil)
	listed := body["runs"].([]any)
	if last := listed[len(listed)-1].(map[string]any); last["waiting"] != true || last["name"] != "later.zip" {
		t.Errorf("run = %v, want it listed as waiting", last)
	}

	close(hold.finish)
	c.extracted(results[0])
	if got := c.names("/later"); got != "a.txt" {
		t.Errorf("/later holds %q", got)
	}
}

func TestExtractCanBeStoppedAndDismissed(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	hold := newGate()
	c.server.extracts.add(extractRun{Name: "ahead.zip"}, hold.work, func() {})
	<-hold.started
	_, results := c.uploadExtract("never.zip", "/", zipOf(t, map[string]string{"a.txt": "x"}))
	id := results[0]["run"].(string)

	if w, _ := c.json(http.MethodDelete, "/api/extracts/"+id, nil); w.Code != http.StatusOK {
		t.Fatalf("stop: %d", w.Code)
	}
	run := c.extracted(results[0])
	if run["cancelled"] != true {
		t.Errorf("run = %v, want it cancelled", run)
	}
	close(hold.finish)

	// The same request again dismisses the result; a third finds nothing.
	if w, _ := c.json(http.MethodDelete, "/api/extracts/"+id, nil); w.Code != http.StatusOK {
		t.Errorf("dismiss: %d", w.Code)
	}
	if w, _ := c.json(http.MethodDelete, "/api/extracts/"+id, nil); w.Code != http.StatusNotFound {
		t.Errorf("gone: %d, want 404", w.Code)
	}
	if got := strings.Join(c.folderPaths(), " "); got != "/" {
		t.Errorf("folders = %q — a run stopped while waiting touched nothing", got)
	}
}

// The archive is kept beside the vault only until it has been unpacked.
func TestExtractLeavesNoSpoolBehind(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	c.extract("tidy.zip", "/", zipOf(t, map[string]string{"a.txt": "x"}))
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(c.server.VaultPath), ".sand-upload-*"))
	if len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestLockingTheVaultForgetsItsExtractions(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.extract("seen.zip", "/", zipOf(t, map[string]string{"a.txt": "x"}))

	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	if runs := c.server.extracts.all(); len(runs) != 0 {
		t.Errorf("runs = %+v — a result names files, and the vault is shut", runs)
	}
}
