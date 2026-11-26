// minibrainz serves a local, read-only copy of MusicBrainz built from its
// public data dumps, answering a subset of the musicbrainz.org /ws/2 API.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	dataDir := env("MINIBRAINZ_DATA", "data")
	dbPath := filepath.Join(dataDir, "mb.sqlite")
	// Building indexes over tens of millions of rows needs gigabytes of
	// scratch space: keep it on the data volume, not a small /tmp.
	if os.Getenv("SQLITE_TMPDIR") == "" {
		tmp := filepath.Join(dataDir, "tmp")
		os.MkdirAll(tmp, 0o755)
		os.Setenv("SQLITE_TMPDIR", tmp)
	}
	days, _ := strconv.Atoi(env("MINIBRAINZ_REFRESH_DAYS", "7"))
	f := &refresher{dumps: env("MINIBRAINZ_DUMPS", defaultDumps), dbPath: dbPath, minAge: time.Duration(days) * 24 * time.Hour}

	switch os.Args[1] {
	case "serve":
		os.MkdirAll(filepath.Dir(dbPath), 0o755)
		f.trigger = make(chan bool, 1)
		go f.loop(env("MINIBRAINZ_REFRESH", "on") == "on")
		addr := env("MINIBRAINZ_ADDR", ":8080")
		log.Printf("listening on %s", addr)
		adminKey, err := loadAdminKey(os.Getenv("MINIBRAINZ_ADMIN_KEY"), filepath.Join(dataDir, "admin-key"))
		if err != nil {
			log.Fatal(err)
		}
		s := &server{store: &store{path: dbPath}, key: os.Getenv("MINIBRAINZ_KEY"), adminKey: adminKey, refresher: f}
		log.Fatal(http.ListenAndServe(addr, s))
	case "refresh":
		if err := f.run(true); err != nil {
			log.Fatal(err)
		}
	case "import":
		// Build from local dump files: minibrainz import OUT.sqlite FILE...
		if len(os.Args) < 4 {
			usage()
		}
		b, err := newBuilder(os.Args[2], filepath.Base(os.Args[3]))
		if err != nil {
			log.Fatal(err)
		}
		for _, path := range os.Args[3:] {
			if err := b.loadFile(path); err != nil {
				log.Fatalf("%s: %v", path, err)
			}
		}
		if err := b.finish(filepath.Base(os.Args[3])); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  minibrainz serve                 serve the API, rebuilding weekly from the latest dump
  minibrainz refresh               rebuild now from the latest dump
  minibrainz import OUT FILE...    build OUT from local dump files (.tar, .tar.bz2, .tar.xz)

settings (environment):
  MINIBRAINZ_DATA          folder holding mb.sqlite (default ./data)
  MINIBRAINZ_ADDR          listen address (default :8080)
  MINIBRAINZ_KEY           if set, requests need "Authorization: Bearer <key>"
  MINIBRAINZ_ADMIN_KEY     key for POST /admin/* (X-Admin-Key header); if unset, one is
                           generated and kept in MINIBRAINZ_DATA/admin-key
  MINIBRAINZ_REFRESH       "off" to never rebuild while serving (default on)
  MINIBRAINZ_REFRESH_DAYS  minimum days between rebuilds (default 7)
  MINIBRAINZ_DUMPS         dump base URL (default the MetaBrainz full export)`)
	os.Exit(2)
}

// loadAdminKey returns the configured admin key, or the one saved at path,
// generating and saving one (readable only by this user) the first time.
func loadAdminKey(configured, path string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return key, os.WriteFile(path, []byte(key+"\n"), 0o600)
}
