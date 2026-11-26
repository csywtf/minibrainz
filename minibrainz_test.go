package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// row writes one dump line for a table, with \N for columns not given.
func row(table string, values map[string]string) string {
	for _, t := range manifest {
		if t.name != table {
			continue
		}
		fields := make([]string, len(t.cols))
		for i, c := range t.cols {
			if v, ok := values[c.name]; ok {
				fields[i] = v
			} else {
				fields[i] = `\N`
			}
		}
		return strings.Join(fields, "\t") + "\n"
	}
	panic("no table " + table)
}

const (
	artistGID = "b7ffd2af-418f-4be2-bdd1-22f8b48613da"
	groupGID  = "7c4cab8d-dead-3870-b501-93c90fd0a580"
)

func fixture(t *testing.T) *httptest.Server {
	rows := map[string]string{
		"artist":             row("artist", map[string]string{"id": "1", "gid": artistGID, "name": "Nine Inch Nails", "sort_name": "Nine Inch Nails", "comment": "", "ended": "0", "begin_date_year": "1988", "type": "2", "area": "222"}),
		"artist_type":        row("artist_type", map[string]string{"id": "2", "name": "Group"}),
		"area":               row("area", map[string]string{"id": "222", "name": "United States"}),
		"iso_3166_1":         row("iso_3166_1", map[string]string{"area": "222", "code": "US"}),
		"artist_alias":       row("artist_alias", map[string]string{"artist": "1", "name": "NIN", "sort_name": "NIN"}),
		"artist_credit":      row("artist_credit", map[string]string{"id": "5", "name": "Nine Inch Nails", "artist_count": "1"}),
		"artist_credit_name": row("artist_credit_name", map[string]string{"artist_credit": "5", "position": "0", "artist": "1", "name": "Nine Inch Nails", "join_phrase": ""}),
		"release_group": row("release_group", map[string]string{"id": "10", "gid": groupGID, "name": "The Downward Spiral", "artist_credit": "5", "type": "1", "comment": ""}) +
			row("release_group", map[string]string{"id": "11", "gid": "53204e85-0778-4121-baad-f2d2a10827e8", "name": "Downward Spiral Remixes", "artist_credit": "5", "type": "1", "comment": ""}),
		"release_group_meta":                row("release_group_meta", map[string]string{"id": "10", "release_count": "30", "first_release_date_year": "1994", "first_release_date_month": "3", "first_release_date_day": "8"}),
		"release_group_primary_type":        row("release_group_primary_type", map[string]string{"id": "1", "name": "Album"}),
		"release_group_secondary_type":      row("release_group_secondary_type", map[string]string{"id": "7", "name": "Remix"}),
		"release_group_secondary_type_join": row("release_group_secondary_type_join", map[string]string{"release_group": "11", "secondary_type": "7"}),
		"release":                           row("release", map[string]string{"id": "20", "gid": "11111111-2222-3333-4444-555555555555", "name": "The Downward Spiral", "artist_credit": "5", "release_group": "10", "status": "1", "barcode": "606949209424", "comment": ""}),
		"release_status":                    row("release_status", map[string]string{"id": "1", "name": "Official"}),
		"release_country":                   row("release_country", map[string]string{"release": "20", "country": "222", "date_year": "1994", "date_month": "3", "date_day": "8"}),
		"medium":                            row("medium", map[string]string{"id": "30", "release": "20", "position": "1", "format": "1", "name": "", "track_count": "1"}),
		"medium_format":                     row("medium_format", map[string]string{"id": "1", "name": "CD"}),
		"recording":                         row("recording", map[string]string{"id": "40", "gid": "22222222-2222-3333-4444-555555555555", "name": `Mr. Self Destruct \\ Live`, "artist_credit": "5", "length": "270000", "comment": "", "video": "f"}),
		"track":                             row("track", map[string]string{"id": "50", "gid": "33333333-2222-3333-4444-555555555555", "recording": "40", "medium": "30", "position": "1", "number": "1", "name": `Mr. Self Destruct \\ Live`, "artist_credit": "5", "length": "270000"}),
		"url":                               row("url", map[string]string{"id": "60", "url": "https://en.wikipedia.org/wiki/Nine_Inch_Nails"}),
		"l_artist_url":                      row("l_artist_url", map[string]string{"link": "70", "entity0": "1", "entity1": "60"}),
		"link":                              row("link", map[string]string{"id": "70", "link_type": "80"}),
		"link_type":                         row("link_type", map[string]string{"id": "80", "name": "wikipedia"}),
		"genre":                             row("genre", map[string]string{"name": "industrial rock"}),
		"tag":                               row("tag", map[string]string{"id": "90", "name": "industrial rock"}) + row("tag", map[string]string{"id": "91", "name": "seen live"}),
		"artist_tag":                        row("artist_tag", map[string]string{"artist": "1", "tag": "90", "count": "40"}) + row("artist_tag", map[string]string{"artist": "1", "tag": "91", "count": "9"}),
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, tbl := range manifest {
		body := rows[tbl.name]
		tw.WriteHeader(&tar.Header{Name: "mbdump/" + tbl.name, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()

	path := filepath.Join(t.TempDir(), "mb.sqlite")
	b, err := newBuilder(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.load(&buf); err != nil {
		t.Fatal(err)
	}
	if err := b.finish("test"); err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(&server{store: &store{path: path}, key: "secret"})
	t.Cleanup(s.Close)
	return s
}

func get(t *testing.T, s *httptest.Server, path string, want int) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", s.URL+path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s: status %d, want %d", path, resp.StatusCode, want)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func asJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestAPI(t *testing.T) {
	s := fixture(t)

	// The key is required.
	if resp, _ := http.Get(s.URL + "/ws/2/artist/" + artistGID); resp.StatusCode != 401 {
		t.Fatalf("no key: status %d", resp.StatusCode)
	}

	artist := get(t, s, "/ws/2/artist/"+artistGID+"?inc=url-rels+genres", 200)
	for _, want := range []string{`"name":"Nine Inch Nails"`, `"type":"Group"`, `"country":"US"`, `"begin":"1988"`,
		`"genres":[{"count":40,"name":"industrial rock"}]`, `"type":"wikipedia","url":{"resource":"https://en.wikipedia.org/wiki/Nine_Inch_Nails"}`} {
		if !strings.Contains(asJSON(artist), want) {
			t.Errorf("artist lookup lacks %s: %s", want, asJSON(artist))
		}
	}

	// Search: an alias is an exact match; the more popular group ranks first.
	if got := asJSON(get(t, s, "/ws/2/artist?query=nin&limit=8", 200)); !strings.Contains(got, `"score":100`) {
		t.Errorf("alias search: %s", got)
	}
	groups := get(t, s, "/ws/2/release-group?query=downward+spiral&limit=40", 200)["release-groups"].([]any)
	if len(groups) != 2 || groups[0].(map[string]any)["id"] != groupGID {
		t.Errorf("release group search: %s", asJSON(groups))
	}

	browse := asJSON(get(t, s, "/ws/2/release-group?artist="+artistGID+"&type=album|ep|single&limit=100", 200))
	if !strings.Contains(browse, `"release-group-count":2`) || !strings.Contains(browse, `"first-release-date":"1994-03-08"`) {
		t.Errorf("browse: %s", browse)
	}

	releases := asJSON(get(t, s, "/ws/2/release?release-group="+groupGID+"&inc=media&limit=100", 200))
	for _, want := range []string{`"status":"Official"`, `"track-count":1`, `"date":"1994-03-08"`, `"barcode":"606949209424"`} {
		if !strings.Contains(releases, want) {
			t.Errorf("release browse lacks %s: %s", want, releases)
		}
	}

	release := asJSON(get(t, s, "/ws/2/release/11111111-2222-3333-4444-555555555555?inc=recordings+artist-credits+release-groups", 200))
	for _, want := range []string{`"title":"Mr. Self Destruct \\ Live"`, `"length":270000`, `"primary-type":"Album"`, `"joinphrase":""`} {
		if !strings.Contains(release, want) {
			t.Errorf("release lookup lacks %s: %s", want, release)
		}
	}

	get(t, s, "/ws/2/release/00000000-0000-0000-0000-000000000000", 404)
	get(t, s, "/ws/2/work/"+groupGID, 404) // not served: callers fall back
}

func TestMatchScore(t *testing.T) {
	for _, c := range []struct {
		want  string
		names []string
		score int
	}{
		{"beatles", []string{"The Beatles"}, 100},
		{"beat", []string{"The Beatles"}, 90},
		{"the beatles", []string{"The Beatles"}, 100},
		{"björk", []string{"Björk"}, 100},
		{"blue", []string{"Blue Lines"}, 90},
		{"blue", []string{"Kind of Blue"}, 80},
	} {
		if got := matchScore(c.want, c.names...); got != c.score {
			t.Errorf("matchScore(%q, %v) = %d, want %d", c.want, c.names, got, c.score)
		}
	}
}

func TestDownloadResumesAndChecks(t *testing.T) {
	body := bytes.Repeat([]byte("musicbrainz "), 10000)
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "dump", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	path := filepath.Join(t.TempDir(), "dumps", "x", "mbdump.tar.bz2")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, body[:5000], 0o644) // a download cut off by a restart
	if err := download(srv.URL, path, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, body) {
		t.Fatalf("resumed file is %d bytes, want %d", len(got), len(body))
	}
	if ranges[0] != "bytes=5000-" {
		t.Errorf("didn't resume: Range %q", ranges[0])
	}
	if err := download(srv.URL, path, want); err != nil || len(ranges) != 1 {
		t.Errorf("a checked file was fetched again (%v, %d requests)", err, len(ranges))
	}

	bad := filepath.Join(filepath.Dir(path), "other.tar.bz2")
	if err := download(srv.URL, bad, strings.Repeat("0", 64)); err == nil {
		t.Fatal("a checksum mismatch passed")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("a file that failed its checksum was kept")
	}
}

// cutAfter stops a reader after n bytes, like a build killed partway.
type cutAfter struct {
	r io.Reader
	n int
}

func (c *cutAfter) Read(b []byte) (int, error) {
	if c.n <= 0 {
		return 0, errors.New("killed")
	}
	if len(b) > c.n {
		b = b[:c.n]
	}
	n, err := c.r.Read(b)
	c.n -= n
	return n, err
}

func TestBuildResumesMidTable(t *testing.T) {
	old := batchRows
	batchRows = 100
	defer func() { batchRows = old }()

	// A dump with every table empty but tag: 1000 rows.
	var rows strings.Builder
	for i := 1; i <= 1000; i++ {
		rows.WriteString(row("tag", map[string]string{"id": strconv.Itoa(i), "name": "tag " + strconv.Itoa(i)}))
	}
	var dump bytes.Buffer
	tw := tar.NewWriter(&dump)
	for _, tbl := range manifest {
		body := ""
		if tbl.name == "tag" {
			body = rows.String()
		}
		tw.WriteHeader(&tar.Header{Name: "mbdump/" + tbl.name, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	tagStart := bytes.Index(dump.Bytes(), []byte("1\ttag 1\t"))

	path := filepath.Join(t.TempDir(), "mb.sqlite.new")
	b, err := newBuilder(path, "D")
	if err != nil {
		t.Fatal(err)
	}
	// Cut off about 450 rows into the tag table.
	if err := b.load(&cutAfter{bytes.NewReader(dump.Bytes()), tagStart + len(rows.String())*45/100}); err == nil {
		t.Fatal("the cut-off load succeeded")
	}
	b.db.Close()

	b, err = newBuilder(path, "D")
	if err != nil {
		t.Fatal(err)
	}
	if n, done := b.state("tag"); n != 400 || done {
		t.Fatalf("resumed at %d rows (done %v), want 400 committed", n, done)
	}
	if err := b.load(bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := b.finish("D"); err != nil {
		t.Fatal(err)
	}

	db, _ := sql.Open("sqlite3", "file:"+path+"?mode=ro&immutable=1")
	defer db.Close()
	var count, distinct, maxID int
	db.QueryRow("SELECT count(*), count(DISTINCT id), max(id) FROM tag").Scan(&count, &distinct, &maxID)
	if count != 1000 || distinct != 1000 || maxID != 1000 {
		t.Errorf("tag has %d rows (%d distinct, max id %d), want 1000", count, distinct, maxID)
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Error("a WAL file was left beside the finished database")
	}
	var building string
	if db.QueryRow("SELECT value FROM meta WHERE key = 'building'").Scan(&building) == nil {
		t.Error("the finished database still says it's building")
	}

	// Another dump starts over instead of resuming.
	b, err = newBuilder(path, "E")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := b.state("tag"); n != 0 {
		t.Errorf("a different dump resumed at %d rows", n)
	}
	b.db.Close()
}

func TestAdmin(t *testing.T) {
	s := fixture(t)
	status := get(t, s, "/admin/status", 200)
	if status["ready"] != true || status["dump_in_use"] != "test" || status["counts"].(map[string]any)["release_group"] != float64(2) {
		t.Errorf("status: %s", asJSON(status))
	}

	srv := s.Config.Handler.(*server)
	srv.refresher = &refresher{trigger: make(chan bool, 1)}
	srv.adminKey = "admin-secret"
	post := func(path string) int {
		req, _ := http.NewRequest("POST", s.URL+path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("X-Admin-Key", "admin-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("/admin/refresh?force=1"); code != 202 {
		t.Fatalf("refresh: %d", code)
	}
	if force := <-srv.refresher.trigger; !force {
		t.Error("force wasn't passed on")
	}
	post("/admin/refresh")
	if code := post("/admin/refresh"); code != 409 {
		t.Errorf("a second waiting refresh: %d, want 409", code)
	}

	// Only the admin key controls it, wherever the request comes from.
	local := httptest.NewRequest("POST", "/admin/refresh", nil)
	local.RemoteAddr = "127.0.0.1:51000"
	if srv.adminAllowed(local) {
		t.Error("a request without the admin key was allowed")
	}
	local.Header.Set("X-Admin-Key", "wrong")
	if srv.adminAllowed(local) {
		t.Error("a wrong admin key was accepted")
	}

	// Without a configured key, one is generated once and kept.
	path := filepath.Join(t.TempDir(), "admin-key")
	first, err := loadAdminKey("", path)
	if err != nil || len(first) != 64 {
		t.Fatalf("generated key %q: %v", first, err)
	}
	if again, _ := loadAdminKey("", path); again != first {
		t.Error("the saved key wasn't reused")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", info.Mode().Perm())
	}
}
