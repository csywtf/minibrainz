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
func (f *refresher) run() error {
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
	start := time.Now()
	sums, err := fetchText(f.dumps + "/" + latest + "/SHA256SUMS")
	if err != nil {
		return err
	}
	tmp := f.dbPath + ".new"
	defer os.Remove(tmp)
	b, err := newBuilder(tmp)
	if err != nil {
		return err
	}
	for _, name := range dumpFiles {
		if err := b.loadURL(f.dumps+"/"+latest+"/"+name, checksum(sums, name)); err != nil {
			b.db.Close()
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := b.finish(latest); err != nil {
		return err
	}
	if err := checkAgainst(f.dbPath, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.dbPath); err != nil {
		return err
	}
	log.Printf("dump %s is live, built in %s", latest, time.Since(start).Round(time.Minute))
	return nil
}

// loadURL streams one dump file into the build, checking its SHA-256 at the end.
func (b *builder) loadURL(url, want string) error {
	if want == "" {
		return fmt.Errorf("no checksum published")
	}
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	sum := sha256.New()
	r, wait, err := decompress(url, io.TeeReader(resp.Body, sum))
	if err != nil {
		return err
	}
	if err := b.load(r); err != nil {
		return err
	}
	io.Copy(io.Discard, r)
	if err := wait(); err != nil {
		return err
	}
	io.Copy(sum, resp.Body) // anything the decompressor didn't read
	if got := hexSum(sum); got != want {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	return nil
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
