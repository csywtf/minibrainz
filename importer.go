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

// builder writes a new database from one or more dump tarballs. A build can
// be cut off at any point (a restart, a deploy) and picks up where it
// stopped: rows are committed in batches together with how far each table
// got, and each index or search step is committed with a mark that it's done.
type builder struct {
	db     *sql.DB
	loaded map[string]int64
}

// batchRows is how many rows go in one transaction: the most a cut-off
// build redoes per table.
var batchRows int64 = 500_000

// newBuilder opens the build at path, resuming it if it's an unfinished
// build of the same dump, or starts a new one.
func newBuilder(path, dump string) (*builder, error) {
	b, err := openBuild(path, dump)
	if err == nil && b != nil {
		log.Printf("resuming the build of %s", dump)
		return b, nil
	}
	if err != nil {
		log.Printf("can't resume the earlier build, starting over: %v", err)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		os.Remove(f)
	}
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	stmts := []string{
		"CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT)",
		"CREATE TABLE build_progress (step TEXT PRIMARY KEY, rows INTEGER NOT NULL, done INTEGER NOT NULL)",
	}
	for _, t := range manifest {
		stmts = append(stmts, t.createSQL())
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", s, err)
		}
	}
	if _, err := db.Exec("INSERT INTO meta VALUES ('building', ?)", dump); err != nil {
		db.Close()
		return nil, err
	}
	return &builder{db: db, loaded: map[string]int64{}}, nil
}

// openBuild reopens an unfinished build of dump at path, or returns nil.
func openBuild(path, dump string) (*builder, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	var building string
	if err := db.QueryRow("SELECT value FROM meta WHERE key = 'building'").Scan(&building); err != nil || building != dump {
		db.Close()
		return nil, nil
	}
	return &builder{db: db, loaded: map[string]int64{}}, nil
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// WAL keeps a cut-off batch from damaging what's committed; synchronous
	// OFF is still safe when the process is killed (only a power cut could
	// lose the build, which then starts over).
	for _, s := range []string{
		"PRAGMA journal_mode = WAL", "PRAGMA synchronous = OFF", "PRAGMA cache_size = -1000000", "PRAGMA temp_store = FILE",
	} {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// state is how far an earlier run got with a step.
func (b *builder) state(step string) (rows int64, done bool) {
	b.db.QueryRow("SELECT rows, done FROM build_progress WHERE step = ?", step).Scan(&rows, &done)
	return rows, done
}

func mark(tx *sql.Tx, step string, rows int64, done bool) error {
	_, err := tx.Exec(`INSERT INTO build_progress VALUES (?, ?, ?)
		ON CONFLICT(step) DO UPDATE SET rows = excluded.rows, done = excluded.done`, step, rows, done)
	return err
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
	n, done := b.state(t.name)
	if done {
		return n, nil // the tar reader skips the rest of this entry
	}
	in := bufio.NewReaderSize(r, 1<<20)
	if n > 0 {
		log.Printf("%s: skipping the %d rows already loaded", t.name, n)
		for i := int64(0); i < n; i++ {
			if _, err := in.ReadSlice('\n'); err != nil && err != bufio.ErrBufferFull {
				return n, fmt.Errorf("skipping loaded rows: %w", err)
			} else if err == bufio.ErrBufferFull {
				i-- // a line longer than the buffer: keep reading it
			}
		}
	}

	kept := t.kept()
	insert := "INSERT INTO " + t.name + " VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(kept)), ",") + ")"
	var tx *sql.Tx
	var stmt *sql.Stmt
	begin := func() error {
		var err error
		if tx, err = b.db.Begin(); err != nil {
			return err
		}
		stmt, err = tx.Prepare(insert)
		return err
	}
	commit := func(finished bool) error {
		if err := mark(tx, t.name, n, finished); err != nil {
			return err
		}
		stmt.Close()
		return tx.Commit()
	}
	if err := begin(); err != nil {
		return n, err
	}
	defer func() { tx.Rollback() }() // no-op once committed

	values := make([]any, len(kept))
	inBatch := int64(0)
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
		if inBatch++; inBatch == batchRows {
			if err := commit(false); err != nil {
				return n, err
			}
			if err := begin(); err != nil {
				return n, err
			}
			inBatch = 0
		}
	}
	return n, commit(true)
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

// finish checks every table arrived, builds indexes and search (each step
// committed with a mark, so a cut-off build skips the finished ones), records
// which dump this is, and leaves a plain single-file database.
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
	steps := postSteps()
	for i, step := range steps {
		name := fmt.Sprintf("post %d", i)
		progress.set(fmt.Sprintf("building indexes and search (%d of %d)", i+1, len(steps)), 0, 0)
		if _, done := b.state(name); done {
			continue
		}
		tx, err := b.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(step); err != nil {
			tx.Rollback()
			return fmt.Errorf("building indexes (%s): %w", strings.TrimSpace(step)[:min(60, len(strings.TrimSpace(step)))], err)
		}
		if err := mark(tx, name, 0, true); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	log.Printf("built indexes and search in %s", time.Since(start).Round(time.Second))
	for _, s := range []string{
		"DELETE FROM meta WHERE key = 'building'",
		"DROP TABLE build_progress",
		"PRAGMA wal_checkpoint(TRUNCATE)",
		// The server opens the file read-only and immutable, which ignores a WAL.
		"PRAGMA journal_mode = DELETE",
	} {
		if _, err := b.db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	_, err := b.db.Exec("INSERT INTO meta VALUES ('dump', ?), ('built', ?)", dump, time.Now().UTC().Format(time.RFC3339))
	return err
}

// postSteps splits postSQL into its statements.
func postSteps() []string {
	var out []string
	for _, chunk := range strings.Split(postSQL, ";\n") {
		var code []string
		for _, line := range strings.Split(chunk, "\n") {
			if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "--") {
				code = append(code, line)
			}
		}
		if len(code) > 0 {
			out = append(out, strings.Join(code, "\n"))
		}
	}
	return out
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
