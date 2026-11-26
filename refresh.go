package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The full export, published twice a week.
const defaultDumps = "https://data.metabrainz.org/pub/musicbrainz/data/fullexport"

// Files streamed from each export; together they hold every table in manifest.
var dumpFiles = []string{"mbdump.tar.bz2", "mbdump-derived.tar.bz2"}

type refresher struct {
	dumps  string // base URL
	dbPath string
	minAge time.Duration // don't rebuild a database younger than this
}

func currentDump(path string) (dump string, built time.Time) {
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		return "", time.Time{}
	}
	defer db.Close()
	var b string
	db.QueryRow("SELECT value FROM meta WHERE key = 'dump'").Scan(&dump)
	db.QueryRow("SELECT value FROM meta WHERE key = 'built'").Scan(&b)
	built, _ = time.Parse(time.RFC3339, b)
	return dump, built
}

// run rebuilds the database from the latest export if it's newer than the
// one in use and the current build is at least minAge old. The new database
// replaces the old only once it's complete and checked.
//
// The dump files are downloaded first, into dumps/<dump>/ beside the
// database, and kept until the build succeeds: a restart resumes a download
// where it stopped and skips files already checked, so only the import
// itself starts over.
func (f *refresher) run() (err error) {
	latest, err := fetchText(f.dumps + "/LATEST")
	if err != nil {
		return err
	}
	latest = strings.TrimSpace(latest)
	dump, built := currentDump(f.dbPath)
	if _, err := os.Stat(f.dbPath); err == nil && (dump == latest || time.Since(built) < f.minAge) {
		return nil
	}
	log.Printf("building from dump %s", latest)
	progress.begin(latest)
	defer func() { progress.end(err) }()
	start := time.Now()
	sums, err := fetchText(f.dumps + "/" + latest + "/SHA256SUMS")
	if err != nil {
		return err
	}
	dumps := filepath.Join(filepath.Dir(f.dbPath), "dumps")
	dir := filepath.Join(dumps, latest)
	removeOthers(dumps, latest) // older, unfinished dumps
	var files []string
	for _, name := range dumpFiles {
		path := filepath.Join(dir, name)
		if err := download(f.dumps+"/"+latest+"/"+name, path, checksum(sums, name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		files = append(files, path)
	}

	tmp := f.dbPath + ".new"
	defer os.Remove(tmp)
	b, err := newBuilder(tmp)
	if err != nil {
		return err
	}
	for _, path := range files {
		if err := b.loadFile(path); err != nil {
			b.db.Close()
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
	}
	progress.set("building indexes and search", 0, 0)
	if err := b.finish(latest); err != nil {
		return err
	}
	if err := checkAgainst(f.dbPath, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.dbPath); err != nil {
		return err
	}
	os.RemoveAll(dir)
	log.Printf("dump %s is live, built in %s", latest, time.Since(start).Round(time.Minute))
	return nil
}

func removeOthers(dir, keep string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != keep {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// download fetches url to path, resuming a partial file, and checks its
// SHA-256. A checked file is marked with path+".ok" and not fetched again.
func download(url, path, want string) error {
	if want == "" {
		return fmt.Errorf("no checksum published")
	}
	if _, err := os.Stat(path + ".ok"); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	have, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("GET", url, nil)
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		log.Printf("resuming %s at %d MB", filepath.Base(path), have>>20)
	case http.StatusRequestedRangeNotSatisfiable:
		// Already complete; checked below.
	case http.StatusOK:
		if have, err = 0, out.Truncate(0); err != nil {
			return err
		}
		if _, err := out.Seek(0, io.SeekStart); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s", resp.Status)
	}
	total := have
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		total += resp.ContentLength
		progress.set("downloading "+filepath.Base(path), have, total)
		if _, err := io.Copy(out, counting{resp.Body}); err != nil {
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}

	progress.set("checking "+filepath.Base(path), 0, total)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, counting{f}); err != nil {
		return err
	}
	if got := hexSum(sum); got != want {
		os.Remove(path) // start this file over next time
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	return os.WriteFile(path+".ok", nil, 0o644)
}

func hexSum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// checksum finds a file's hash in a SHA256SUMS listing ("<hash> *<name>").
func checksum(sums, name string) string {
	s := bufio.NewScanner(strings.NewReader(sums))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0]
		}
	}
	return ""
}

// checkAgainst refuses a new build that lost more than 5% of any big table,
// which would mean a truncated or broken dump.
func checkAgainst(oldPath, newPath string) error {
	newDB, err := sql.Open("sqlite3", "file:"+newPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer newDB.Close()
	fresh, err := counts(newDB)
	if err != nil {
		return err
	}
	if _, err := os.Stat(oldPath); err != nil {
		return nil // first build
	}
	oldDB, err := sql.Open("sqlite3", "file:"+oldPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer oldDB.Close()
	old, err := counts(oldDB)
	if err != nil {
		return nil // an unreadable old build shouldn't block a new one
	}
	for t, n := range old {
		if fresh[t] < n*95/100 {
			return fmt.Errorf("new build has %d %s rows, down from %d; keeping the old one", fresh[t], t, n)
		}
	}
	return nil
}

func fetchText(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}
