package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// store hands out the current database, reopening it when a rebuild has
// replaced the file.
type store struct {
	path    string
	mu      sync.Mutex
	db      *sql.DB
	modTime time.Time
	checked time.Time
}

var errNoDatabase = errors.New("no database yet")

func (s *store) get() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil && time.Since(s.checked) < 5*time.Second {
		return s.db, nil
	}
	s.checked = time.Now()
	info, err := os.Stat(s.path)
	if err != nil {
		if s.db != nil {
			return s.db, nil
		}
		return nil, errNoDatabase
	}
	if s.db != nil && info.ModTime().Equal(s.modTime) {
		return s.db, nil
	}
	db, err := sql.Open("sqlite3", "file:"+s.path+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	if old := s.db; old != nil {
		time.AfterFunc(time.Minute, func() { old.Close() }) // after in-flight requests
	}
	s.db, s.modTime = db, info.ModTime()
	log.Printf("opened %s", s.path)
	return db, nil
}

type server struct {
	store *store
	key   string
}

type notFound struct{}

func (notFound) Error() string { return "Not Found" }

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.key != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.key)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return
	}
	db, err := s.store.get()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	if r.URL.Path == "/health" {
		var dump string
		db.QueryRow("SELECT value FROM meta WHERE key = 'dump'").Scan(&dump)
		writeJSON(w, http.StatusOK, map[string]any{"dump": dump})
		return
	}
	q := query{db: db, credits: map[int64][]any{}}
	result, err := q.route(r)
	var nf notFound
	switch {
	case errors.As(err, &nf):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not Found"})
	case err != nil:
		log.Printf("%s: %v", r.URL, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal error"})
	default:
		writeJSON(w, http.StatusOK, result)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// query answers one request in musicbrainz.org's /ws/2 JSON shape, for the
// requests apollod makes. Anything else is a 404, so callers fall back to
// musicbrainz.org.
type query struct {
	db      *sql.DB
	credits map[int64][]any // artist credits already loaded, by id
}

func (q *query) route(r *http.Request) (any, error) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/ws/2/"), "/")
	p := r.URL.Query()
	entity, id, _ := strings.Cut(path, "/")
	inc := map[string]bool{}
	for _, i := range strings.FieldsFunc(p.Get("inc"), func(r rune) bool { return r == '+' || r == ' ' }) {
		inc[i] = true
	}
	limit, _ := strconv.Atoi(p.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	switch {
	case entity == "artist" && id == "" && p.Has("query"):
		return q.searchArtists(p.Get("query"), limit)
	case entity == "artist" && id != "":
		return q.artist(id, inc)
	case entity == "release-group" && id == "" && p.Has("query"):
		return q.searchReleaseGroups(p.Get("query"), limit)
	case entity == "release-group" && id == "" && p.Has("artist"):
		return q.artistReleaseGroups(p.Get("artist"), p.Get("type"))
	case entity == "release-group" && id != "":
		return q.releaseGroup(id)
	case entity == "release" && id == "" && p.Has("release-group"):
		return q.releaseGroupReleases(p.Get("release-group"), inc)
	case entity == "release" && id != "":
		return q.release(id, inc)
	case entity == "recording" && id != "":
		return q.recording(id)
	}
	return nil, notFound{}
}

func mbid(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != 16 {
		return nil, notFound{}
	}
	return b, nil
}

func formatMBID(b []byte) string {
	h := hex.EncodeToString(b)
	if len(h) != 32 {
		return ""
	}
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// date formats a partial date the way MusicBrainz does: "1991", "1991-10" or "1991-10-22".
func date(y, m, d sql.NullInt64) string {
	switch {
	case !y.Valid:
		return ""
	case !m.Valid:
		return fmt.Sprintf("%04d", y.Int64)
	case !d.Valid:
		return fmt.Sprintf("%04d-%02d", y.Int64, m.Int64)
	}
	return fmt.Sprintf("%04d-%02d-%02d", y.Int64, m.Int64, d.Int64)
}

func nullString(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

func (q *query) artistCredit(id int64) ([]any, error) {
	if c, ok := q.credits[id]; ok {
		return c, nil
	}
	rows, err := q.db.Query(`
		SELECT acn.name, acn.join_phrase, a.gid, a.name, a.sort_name, a.comment
		FROM artist_credit_name acn JOIN artist a ON a.id = acn.artist
		WHERE acn.artist_credit = ? ORDER BY acn.position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	credit := []any{}
	for rows.Next() {
		var name, join, artistName, sortName, comment string
		var gid []byte
		if err := rows.Scan(&name, &join, &gid, &artistName, &sortName, &comment); err != nil {
			return nil, err
		}
		credit = append(credit, map[string]any{
			"name": name, "joinphrase": join,
			"artist": map[string]any{"id": formatMBID(gid), "name": artistName, "sort-name": sortName, "disambiguation": comment},
		})
	}
	q.credits[id] = credit
	return credit, rows.Err()
}

// --- artists ---

func (q *query) artist(id string, inc map[string]bool) (any, error) {
	gid, err := mbid(id)
	if err != nil {
		return nil, err
	}
	var artistID int64
	var name, sortName, comment string
	var typ, area, beginArea, country sql.NullString
	var by, bm, bd, ey, em, ed sql.NullInt64
	var ended bool
	err = q.db.QueryRow(`
		SELECT a.id, a.name, a.sort_name, a.comment, t.name, ar.name, ba.name, iso.code,
		       a.begin_date_year, a.begin_date_month, a.begin_date_day, a.end_date_year, a.end_date_month, a.end_date_day, a.ended
		FROM artist a
		LEFT JOIN artist_type t ON t.id = a.type
		LEFT JOIN area ar ON ar.id = a.area
		LEFT JOIN area ba ON ba.id = a.begin_area
		LEFT JOIN iso_3166_1 iso ON iso.area = a.area
		WHERE a.gid = ?`, gid).Scan(&artistID, &name, &sortName, &comment, &typ, &area, &beginArea, &country,
		&by, &bm, &bd, &ey, &em, &ed, &ended)
	if err == sql.ErrNoRows {
		return nil, notFound{}
	}
	if err != nil {
		return nil, err
	}
	place := func(n sql.NullString) any {
		if !n.Valid {
			return nil
		}
		return map[string]any{"name": n.String}
	}
	out := map[string]any{
		"id": formatMBID(gid), "name": name, "sort-name": sortName, "disambiguation": comment,
		"type": nullString(typ), "country": nullString(country), "area": place(area), "begin-area": place(beginArea),
		"life-span": map[string]any{"begin": nilIfEmpty(date(by, bm, bd)), "end": nilIfEmpty(date(ey, em, ed)), "ended": ended},
	}
	if inc["url-rels"] {
		relations, err := q.urlRelations(artistID)
		if err != nil {
			return nil, err
		}
		out["relations"] = relations
	}
	if inc["genres"] {
		genres, err := q.genres(artistID)
		if err != nil {
			return nil, err
		}
		out["genres"] = genres
	}
	return out, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (q *query) urlRelations(artistID int64) ([]any, error) {
	rows, err := q.db.Query(`
		SELECT lt.name, u.url FROM l_artist_url l
		JOIN link ON link.id = l.link JOIN link_type lt ON lt.id = link.link_type
		JOIN url u ON u.id = l.entity1
		WHERE l.entity0 = ?`, artistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var typ, url string
		if err := rows.Scan(&typ, &url); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"type": typ, "target-type": "url", "url": map[string]any{"resource": url}})
	}
	return out, rows.Err()
}

func (q *query) genres(artistID int64) ([]any, error) {
	rows, err := q.db.Query(`
		SELECT g.name, at.count FROM artist_tag at
		JOIN tag t ON t.id = at.tag JOIN genre g ON g.name = t.name
		WHERE at.artist = ? AND at.count > 0 ORDER BY at.count DESC`, artistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"name": name, "count": count})
	}
	return out, rows.Err()
}

// --- search ---

// searchTerms turns a query (plain words or simple Lucene, such as
// artist:"Name") into FTS5 terms; the last word also matches as a prefix.
func searchTerms(text string) (string, []string) {
	text = strings.ToLower(text)
	for _, field := range []string{"artist:", "releasegroup:", "release:"} {
		text = strings.ReplaceAll(text, field, " ")
	}
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	if len(words) == 0 {
		return "", nil
	}
	terms := make([]string, len(words))
	for i, w := range words {
		terms[i] = `"` + w + `"`
	}
	terms[len(terms)-1] += "*"
	return strings.Join(terms, " "), words
}

// normalize lowercases, keeps letters and digits, and drops a leading
// article, for exact-match checks ("downward spiral" is The Downward Spiral).
func normalize(s string) string {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	if len(words) > 1 && (words[0] == "the" || words[0] == "a" || words[0] == "an") {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// matchScore is MusicBrainz's 0 to 100 search score, roughly: 100 for an
// exact match, 90 when the name starts with the query, 80 otherwise (every
// word matched somewhere).
func matchScore(want string, names ...string) int {
	want = normalize(want)
	best := 80
	for _, n := range names {
		n = normalize(n)
		if n == want {
			return 100
		}
		if strings.HasPrefix(n, want) {
			best = 90
		}
	}
	return best
}

// searchPool is how many text matches are re-ranked by score and popularity.
const searchPool = 1000

type ranked struct {
	id, popularity int64
	score          int
	result         map[string]any
}

func sortRanked(r []ranked, limit int) []any {
	sort.SliceStable(r, func(i, j int) bool {
		if r[i].score != r[j].score {
			return r[i].score > r[j].score
		}
		return r[i].popularity > r[j].popularity
	})
	out := []any{}
	for i := 0; i < len(r) && i < limit; i++ {
		r[i].result["score"] = r[i].score
		out = append(out, r[i].result)
	}
	return out
}

func (q *query) searchArtists(text string, limit int) (any, error) {
	match, words := searchTerms(text)
	out := map[string]any{"artists": []any{}}
	if match == "" {
		return out, nil
	}
	want := strings.Join(words, " ")
	rows, err := q.db.Query(`
		SELECT a.id, a.gid, a.name, a.sort_name, a.comment, t.name, coalesce(p.score, 0)
		FROM (SELECT rowid FROM artist_fts WHERE artist_fts MATCH ? ORDER BY rank LIMIT ?) f
		JOIN artist a ON a.id = f.rowid
		LEFT JOIN artist_type t ON t.id = a.type
		LEFT JOIN artist_popularity p ON p.artist = a.id`, match, searchPool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []ranked
	for rows.Next() {
		var r ranked
		var gid []byte
		var name, sortName, comment string
		var typ sql.NullString
		if err := rows.Scan(&r.id, &gid, &name, &sortName, &comment, &typ, &r.popularity); err != nil {
			return nil, err
		}
		r.score = matchScore(want, name, sortName)
		r.result = map[string]any{"id": formatMBID(gid), "name": name, "sort-name": sortName, "disambiguation": comment, "type": nullString(typ)}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Aliases count as exact matches too ("Beatles" for The Beatles).
	for i := range found {
		if found[i].score == 100 {
			continue
		}
		var aliases sql.NullString
		q.db.QueryRow("SELECT group_concat(name, char(31)) FROM artist_alias WHERE artist = ?", found[i].id).Scan(&aliases)
		if aliases.Valid && matchScore(want, strings.Split(aliases.String, "\x1f")...) == 100 {
			found[i].score = 100
		}
	}
	artists := sortRanked(found, limit)
	out["artists"] = artists
	out["count"] = len(artists)
	return out, nil
}

func (q *query) searchReleaseGroups(text string, limit int) (any, error) {
	match, words := searchTerms(text)
	out := map[string]any{"release-groups": []any{}}
	if match == "" {
		return out, nil
	}
	want := strings.Join(words, " ")
	rows, err := q.db.Query(`
		SELECT rg.id FROM (SELECT rowid FROM release_group_fts WHERE release_group_fts MATCH ? ORDER BY rank LIMIT ?) f
		JOIN release_group rg ON rg.id = f.rowid`, match, searchPool)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var found []ranked
	for _, id := range ids {
		g, err := q.releaseGroupByID(id)
		if err != nil {
			return nil, err
		}
		artist := g.result["artist-credit-phrase"].(string)
		delete(g.result, "artist-credit-phrase")
		title := g.result["title"].(string)
		g.score = matchScore(want, title, artist+" "+title, title+" "+artist)
		found = append(found, g)
	}
	groups := sortRanked(found, limit)
	out["release-groups"] = groups
	out["count"] = len(groups)
	return out, nil
}

// --- release groups ---

func (q *query) releaseGroupByID(id int64) (ranked, error) {
	var gid []byte
	var title, comment, credit string
	var ac int64
	var typ sql.NullString
	var y, m, d, releases sql.NullInt64
	err := q.db.QueryRow(`
		SELECT rg.gid, rg.name, rg.comment, rg.artist_credit, ac.name, pt.name,
		       meta.first_release_date_year, meta.first_release_date_month, meta.first_release_date_day, meta.release_count
		FROM release_group rg
		JOIN artist_credit ac ON ac.id = rg.artist_credit
		LEFT JOIN release_group_primary_type pt ON pt.id = rg.type
		LEFT JOIN release_group_meta meta ON meta.id = rg.id
		WHERE rg.id = ?`, id).Scan(&gid, &title, &comment, &ac, &credit, &typ, &y, &m, &d, &releases)
	if err == sql.ErrNoRows {
		return ranked{}, notFound{}
	}
	if err != nil {
		return ranked{}, err
	}
	secondary, err := q.strings("SELECT st.name FROM release_group_secondary_type_join j JOIN release_group_secondary_type st ON st.id = j.secondary_type WHERE j.release_group = ?", id)
	if err != nil {
		return ranked{}, err
	}
	credits, err := q.artistCredit(ac)
	if err != nil {
		return ranked{}, err
	}
	return ranked{id: id, popularity: releases.Int64, result: map[string]any{
		"id": formatMBID(gid), "title": title, "disambiguation": comment,
		"primary-type": nullString(typ), "secondary-types": secondary,
		"first-release-date": date(y, m, d), "artist-credit": credits, "artist-credit-phrase": credit,
	}}, nil
}

func (q *query) strings(sqlText string, args ...any) ([]string, error) {
	rows, err := q.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (q *query) idFor(table, id string) (int64, error) {
	gid, err := mbid(id)
	if err != nil {
		return 0, err
	}
	var n int64
	err = q.db.QueryRow("SELECT id FROM "+table+" WHERE gid = ?", gid).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, notFound{}
	}
	return n, err
}

func (q *query) releaseGroup(id string) (any, error) {
	rgID, err := q.idFor("release_group", id)
	if err != nil {
		return nil, err
	}
	g, err := q.releaseGroupByID(rgID)
	if err != nil {
		return nil, err
	}
	delete(g.result, "artist-credit-phrase")
	return g.result, nil
}

// artistReleaseGroups browses an artist's release groups. Unlike
// musicbrainz.org there's no 100-item page: all of them come back.
func (q *query) artistReleaseGroups(artist, types string) (any, error) {
	artistID, err := q.idFor("artist", artist)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, t := range strings.Split(strings.ToLower(types), "|") {
		if t != "" {
			want[t] = true
		}
	}
	rows, err := q.db.Query(`
		SELECT DISTINCT rg.id FROM artist_credit_name acn JOIN release_group rg ON rg.artist_credit = acn.artist_credit
		WHERE acn.artist = ?`, artistID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	groups := []any{}
	for _, id := range ids {
		g, err := q.releaseGroupByID(id)
		if err != nil {
			return nil, err
		}
		delete(g.result, "artist-credit-phrase")
		if len(want) > 0 && !typeMatches(g.result, want) {
			continue
		}
		groups = append(groups, g.result)
	}
	return map[string]any{"release-groups": groups, "release-group-count": len(groups), "release-group-offset": 0}, nil
}

func typeMatches(group map[string]any, want map[string]bool) bool {
	if t, ok := group["primary-type"].(string); ok && want[strings.ToLower(t)] {
		return true
	}
	for _, t := range group["secondary-types"].([]string) {
		if want[strings.ToLower(t)] {
			return true
		}
	}
	return false
}

// --- releases ---

func (q *query) releaseByID(id int64, inc map[string]bool) (map[string]any, error) {
	var gid []byte
	var title, comment string
	var barcode, status sql.NullString
	var ac, rg int64
	err := q.db.QueryRow(`
		SELECT r.gid, r.name, r.comment, r.barcode, s.name, r.artist_credit, r.release_group
		FROM release r LEFT JOIN release_status s ON s.id = r.status WHERE r.id = ?`, id).
		Scan(&gid, &title, &comment, &barcode, &status, &ac, &rg)
	if err == sql.ErrNoRows {
		return nil, notFound{}
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"id": formatMBID(gid), "title": title, "disambiguation": comment,
		"barcode": nullString(barcode), "status": nullString(status),
	}
	// The earliest release event gives the date and country.
	var country sql.NullString
	var y, m, d sql.NullInt64
	q.db.QueryRow(`
		SELECT country, date_year, date_month, date_day FROM (
		  SELECT iso.code AS country, rc.date_year, rc.date_month, rc.date_day FROM release_country rc
		  LEFT JOIN iso_3166_1 iso ON iso.area = rc.country WHERE rc.release = ?1
		  UNION ALL
		  SELECT NULL, date_year, date_month, date_day FROM release_unknown_country WHERE release = ?1)
		ORDER BY date_year IS NULL, date_year, date_month, date_day LIMIT 1`, id).Scan(&country, &y, &m, &d)
	out["date"], out["country"] = date(y, m, d), nullString(country)

	if inc["artist-credits"] {
		credits, err := q.artistCredit(ac)
		if err != nil {
			return nil, err
		}
		out["artist-credit"] = credits
	}
	if inc["release-groups"] {
		g, err := q.releaseGroupByID(rg)
		if err != nil {
			return nil, err
		}
		delete(g.result, "artist-credit-phrase")
		if !inc["artist-credits"] {
			delete(g.result, "artist-credit")
		}
		out["release-group"] = g.result
	}
	if inc["labels"] {
		labels, err := q.labels(id)
		if err != nil {
			return nil, err
		}
		out["label-info"] = labels
	}
	if inc["media"] || inc["recordings"] {
		media, err := q.media(id, inc)
		if err != nil {
			return nil, err
		}
		out["media"] = media
	}
	return out, nil
}

func (q *query) labels(releaseID int64) ([]any, error) {
	rows, err := q.db.Query(`
		SELECT rl.catalog_number, l.gid, l.name FROM release_label rl
		LEFT JOIN label l ON l.id = rl.label WHERE rl.release = ?`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var catalog, name sql.NullString
		var gid []byte
		if err := rows.Scan(&catalog, &gid, &name); err != nil {
			return nil, err
		}
		info := map[string]any{"catalog-number": nullString(catalog), "label": nil}
		if gid != nil {
			info["label"] = map[string]any{"id": formatMBID(gid), "name": name.String}
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

func (q *query) media(releaseID int64, inc map[string]bool) ([]any, error) {
	rows, err := q.db.Query(`
		SELECT m.id, m.position, f.name, m.name, m.track_count FROM medium m
		LEFT JOIN medium_format f ON f.id = m.format WHERE m.release = ? ORDER BY m.position`, releaseID)
	if err != nil {
		return nil, err
	}
	type medium struct {
		id, position, trackCount int64
		format                   sql.NullString
		title                    string
	}
	var media []medium
	for rows.Next() {
		var m medium
		if err := rows.Scan(&m.id, &m.position, &m.format, &m.title, &m.trackCount); err != nil {
			rows.Close()
			return nil, err
		}
		media = append(media, m)
	}
	rows.Close()
	out := []any{}
	for _, m := range media {
		entry := map[string]any{"position": m.position, "format": nullString(m.format), "title": m.title, "track-count": m.trackCount}
		if inc["recordings"] {
			tracks, err := q.tracks(m.id, inc["artist-credits"])
			if err != nil {
				return nil, err
			}
			entry["tracks"] = tracks
		}
		out = append(out, entry)
	}
	return out, nil
}

func (q *query) tracks(mediumID int64, withCredits bool) ([]any, error) {
	rows, err := q.db.Query(`
		SELECT t.gid, t.position, t.number, t.name, t.length, t.artist_credit,
		       r.gid, r.name, r.length, r.comment, r.video, r.artist_credit
		FROM track t JOIN recording r ON r.id = t.recording
		WHERE t.medium = ? ORDER BY t.position`, mediumID)
	if err != nil {
		return nil, err
	}
	type row struct {
		gid, rgid                []byte
		position, ac, rac        int64
		number, name, rname, rcm string
		length, rlength          sql.NullInt64
		video                    bool
	}
	var list []row
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.gid, &t.position, &t.number, &t.name, &t.length, &t.ac,
			&t.rgid, &t.rname, &t.rlength, &t.rcm, &t.video, &t.rac); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, t)
	}
	rows.Close()
	out := []any{}
	for _, t := range list {
		recording := map[string]any{"id": formatMBID(t.rgid), "title": t.rname, "length": nullInt(t.rlength), "disambiguation": t.rcm, "video": t.video}
		track := map[string]any{"id": formatMBID(t.gid), "position": t.position, "number": t.number, "title": t.name, "length": nullInt(t.length), "recording": recording}
		if withCredits {
			credit, err := q.artistCredit(t.ac)
			if err != nil {
				return nil, err
			}
			track["artist-credit"] = credit
			if recording["artist-credit"], err = q.artistCredit(t.rac); err != nil {
				return nil, err
			}
		}
		out = append(out, track)
	}
	return out, nil
}

func nullInt(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func (q *query) release(id string, inc map[string]bool) (any, error) {
	releaseID, err := q.idFor("release", id)
	if err != nil {
		return nil, err
	}
	return q.releaseByID(releaseID, inc)
}

func (q *query) releaseGroupReleases(releaseGroup string, inc map[string]bool) (any, error) {
	rgID, err := q.idFor("release_group", releaseGroup)
	if err != nil {
		return nil, err
	}
	rows, err := q.db.Query("SELECT id FROM release WHERE release_group = ? ORDER BY id", rgID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	releases := []any{}
	for _, id := range ids {
		r, err := q.releaseByID(id, map[string]bool{"media": inc["media"], "artist-credits": inc["artist-credits"], "labels": inc["labels"]})
		if err != nil {
			return nil, err
		}
		releases = append(releases, r)
	}
	return map[string]any{"releases": releases, "release-count": len(releases), "release-offset": 0}, nil
}

func (q *query) recording(id string) (any, error) {
	gid, err := mbid(id)
	if err != nil {
		return nil, err
	}
	var name, comment string
	var length sql.NullInt64
	var video bool
	err = q.db.QueryRow("SELECT name, comment, length, video FROM recording WHERE gid = ?", gid).Scan(&name, &comment, &length, &video)
	if err == sql.ErrNoRows {
		return nil, notFound{}
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": formatMBID(gid), "title": name, "disambiguation": comment, "length": nullInt(length), "video": video}, nil
}
