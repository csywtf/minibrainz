package main

import (
	"archive/tar"
	"bufio"
	"compress/bzip2"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// builder writes a new database from one or more dump tarballs.
type builder struct {
	db     *sql.DB
	loaded map[string]int64
}

func newBuilder(path string) (*builder, error) {
	os.Remove(path)
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		"PRAGMA journal_mode = OFF", "PRAGMA synchronous = OFF", "PRAGMA cache_size = -1000000",
		"PRAGMA temp_store = FILE", "CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT)",
	} {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, err
		}
	}
	for _, t := range manifest {
		if _, err := db.Exec(t.createSQL()); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
	}
	return &builder{db: db, loaded: map[string]int64{}}, nil
}

// load reads a dump tarball (mbdump/<table> entries) and loads the kept tables.
func (b *builder) load(r io.Reader) error {
	byName := map[string]table{}
	for _, t := range manifest {
		byName[t.name] = t
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		t, ok := byName[strings.TrimPrefix(h.Name, "mbdump/")]
		if !ok || !strings.HasPrefix(h.Name, "mbdump/") {
			continue
		}
		progress.mu.Lock()
		if progress.running {
			progress.step = "loading " + t.name
		}
		progress.mu.Unlock()
		start := time.Now()
		n, err := b.loadTable(t, tr)
		if err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		b.loaded[t.name] = n
		log.Printf("loaded %s: %d rows in %s", t.name, n, time.Since(start).Round(time.Second))
	}
}

func (b *builder) loadTable(t table, r io.Reader) (int64, error) {
	kept := t.kept()
	marks := strings.TrimSuffix(strings.Repeat("?,", len(kept)), ",")
	tx, err := b.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("INSERT INTO " + t.name + " VALUES (" + marks + ")")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	in := bufio.NewReaderSize(r, 1<<20)
	values := make([]any, len(kept))
	var n int64
	for {
		line, err := in.ReadString('\n')
		if err == io.EOF && line == "" {
			break
		}
		if err != nil && err != io.EOF {
			return n, err
		}
		fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(fields) != len(t.cols) {
			return n, fmt.Errorf("row %d has %d columns, expected %d (did the MusicBrainz schema change?)", n+1, len(fields), len(t.cols))
		}
		i := 0
		for j, c := range t.cols {
			if !c.keep {
				continue
			}
			if values[i], err = convert(fields[j], c.kind); err != nil {
				return n, fmt.Errorf("row %d, %s: %w", n+1, c.name, err)
			}
			i++
		}
		if _, err := stmt.Exec(values...); err != nil {
			return n, err
		}
		n++
	}
	return n, tx.Commit()
}

func convert(s string, kind byte) (any, error) {
	if s == `\N` {
		return nil, nil
	}
	switch kind {
	case 't':
		return unescape(s), nil
	case 'g':
		b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
		if err != nil || len(b) != 16 {
			return nil, fmt.Errorf("bad MBID %q", s)
		}
		return b, nil
	}
	switch s {
	case "t":
		return int64(1), nil
	case "f":
		return int64(0), nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// unescape undoes PostgreSQL COPY text escaping.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			out.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			out.WriteByte('\t')
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		default:
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

// finish checks every table arrived, builds indexes and search, and records
// which dump this is.
func (b *builder) finish(dump string) error {
	defer b.db.Close()
	var missing []string
	for _, t := range manifest {
		if _, ok := b.loaded[t.name]; !ok {
			missing = append(missing, t.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the dump had no %s", strings.Join(missing, ", "))
	}
	start := time.Now()
	if _, err := b.db.Exec(postSQL); err != nil {
		return fmt.Errorf("building indexes: %w", err)
	}
	log.Printf("built indexes and search in %s", time.Since(start).Round(time.Second))
	_, err := b.db.Exec("INSERT INTO meta VALUES ('dump', ?), ('built', ?)", dump, time.Now().UTC().Format(time.RFC3339))
	return err
}

// decompress picks a decompressor from the file name: lbzip2 or xz when
// installed (much faster), else Go's own bzip2.
func decompress(name string, r io.Reader) (io.Reader, func() error, error) {
	external := func(cmd string, args ...string) (io.Reader, func() error, error) {
		c := exec.Command(cmd, args...)
		c.Stdin, c.Stderr = r, os.Stderr
		out, err := c.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		return out, c.Wait, c.Start()
	}
	none := func() error { return nil }
	switch {
	case strings.HasSuffix(name, ".bz2"):
		if _, err := exec.LookPath("lbzip2"); err == nil {
			return external("lbzip2", "-dc")
		}
		return bzip2.NewReader(r), none, nil
	case strings.HasSuffix(name, ".xz"):
		return external("xz", "-dc")
	case strings.HasSuffix(name, ".tar"):
		return r, none, nil
	}
	return nil, nil, errors.New("unknown dump format: " + name)
}

// loadFile loads one local dump file.
func (b *builder) loadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil {
		progress.set("loading "+filepath.Base(path), 0, info.Size())
	}
	r, wait, err := decompress(path, counting{f})
	if err != nil {
		return err
	}
	if err := b.load(r); err != nil {
		return err
	}
	io.Copy(io.Discard, r) // let the decompressor finish
	return wait()
}

// counts returns row counts of the big tables, to compare builds.
func counts(db *sql.DB) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range []string{"artist", "release_group", "release", "track", "recording"} {
		var n int64
		if err := db.QueryRow("SELECT count(*) FROM " + t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}
