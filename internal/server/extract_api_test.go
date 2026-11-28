package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"mime/multipart"
	"net/http"
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

			code, results := c.uploadExtract(name, "/inbox", build(t, map[string]string{
				"readme.txt":         "hello",
				"day1/hike.txt":      "a ridge",
				"day1/night/sky.txt": "stars",
			}))
			if code != http.StatusCreated {
				t.Fatalf("status = %d, results = %v", code, results)
			}
			if len(results) != 1 {
				t.Fatalf("results = %v, want the archive's own line and nothing else", results)
			}
			if ok, _ := results[0]["ok"].(bool); !ok || results[0]["extracted"] != float64(3) || results[0]["name"] != name {
				t.Errorf("result = %v, want %s ok with 3 extracted", results[0], name)
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

	c.uploadExtract("old.zip", "/", zipOf(t, map[string]string{"photo.txt": "x"}))
	if got := c.modified("/old", "photo.txt"); got.Year() != 2019 {
		t.Errorf("modified = %v, want the 2019 the archive recorded", got)
	}
}

// project-1.0.tar.gz holding project-1.0/ is how most archives are made, and
// unpacking it to project-1.0/project-1.0 is what nobody wants.
func TestExtractDoesNotNestAFolderNamedForTheArchive(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	code, _ := c.uploadExtract("project-1.0.tar.gz", "/", tarGzOf(t, map[string]string{
		"project-1.0/":            "",
		"project-1.0/main.go":     "package main",
		"project-1.0/docs/":       "",
		"project-1.0/docs/use.md": "use it",
	}))
	if code != http.StatusCreated {
		t.Fatalf("status = %d", code)
	}
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

	code, results := c.uploadExtract("shell.tgz", "/", tarGzOf(t, map[string]string{"drafts/old/": ""}))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, results = %v — an archive of folders is still an archive that unpacked", code, results)
	}
	if got := strings.Join(c.folderPaths(), " "); got != "/ /shell /shell/drafts /shell/drafts/old" {
		t.Errorf("folders = %q", got)
	}
}

func TestExtractRefusesAPathThatClimbsOut(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.mkdir("/inbox")

	code, results := c.uploadExtract("evil.zip", "/inbox", zipOf(t, map[string]string{
		"../escaped.txt":        "out",
		"/etc/escaped.txt":      "root",
		"a/../../../escape.txt": "far",
		"fine.txt":              "in",
	}))
	if code != http.StatusCreated {
		t.Fatalf("status = %d", code)
	}

	// The archive's line, then one for each entry that was turned away.
	if len(results) != 4 {
		t.Fatalf("results = %v, want the archive and three refusals", results)
	}
	if results[0]["extracted"] != float64(1) {
		t.Errorf("extracted = %v, want only fine.txt", results[0]["extracted"])
	}
	for _, r := range results[1:] {
		if ok, _ := r["ok"].(bool); ok || r["stage"] != "placing" {
			t.Errorf("%v was not refused before anything was sent", r)
		}
		if !strings.HasPrefix(r["name"].(string), "evil/") {
			t.Errorf("name = %v, want the entry's path under the folder it was bound for", r["name"])
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

	_, results := c.uploadExtract("mixed.tar.gz", "/", buf.Bytes())
	if got := c.names("/mixed"); got != "real.txt" {
		t.Errorf("/mixed holds %q", got)
	}
	warnings, _ := results[0]["warnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "shadow (a symbolic link)") {
		t.Errorf("warnings = %v, want the link named", warnings)
	}
}

func TestExtractRefusesWhatIsNotAnArchive(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	// By name: nothing says what a .txt would unpack as.
	code, results := c.uploadExtract("notes.txt", "/", []byte("plain"))
	if code != http.StatusBadGateway || results[0]["stage"] != "placing" {
		t.Errorf("notes.txt: %d %v", code, results)
	}
	// By contents: named like one, but not one.
	code, results = c.uploadExtract("fake.zip", "/", []byte("plain"))
	if code != http.StatusBadGateway || results[0]["stage"] != "reading" {
		t.Errorf("fake.zip: %d %v", code, results)
	}
	if got := c.names("/"); got != "" {
		t.Errorf("%q was stored — asked to extract, it is not stored as it is instead", got)
	}
}

func TestExtractRefusesAnArchiveThatUnpacksToTooMuch(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.server.MaxExtractSize = 1000

	// A few hundred bytes on the wire, well over the limit once out.
	code, results := c.uploadExtract("bomb.zip", "/", zipOf(t, map[string]string{
		"zeros.bin": strings.Repeat("0", 100_000),
	}))
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d", code)
	}
	if msg, _ := results[0]["error"].(string); !strings.Contains(msg, "unpacks to more than") {
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
	if got := c.names("/"); got != "keep.zip" {
		t.Errorf("/ holds %q, want only the archive that was not opened", got)
	}
	if got := c.names("/open"); got != "inside.txt" {
		t.Errorf("/open holds %q", got)
	}
	var resp struct {
		Stored int `json:"stored"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Stored != 2 {
		t.Errorf("stored = %d, want the archive kept and the one file unpacked", resp.Stored)
	}
}
