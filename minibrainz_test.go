package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
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
	b, err := newBuilder(path)
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
