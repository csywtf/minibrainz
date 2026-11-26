// minibrainz serves a local, read-only copy of MusicBrainz built from its
// public data dumps, answering a subset of the musicbrainz.org /ws/2 API.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
		if env("MINIBRAINZ_REFRESH", "on") == "on" {
			go func() {
				for {
					if err := f.run(); err != nil {
						log.Printf("refresh failed, keeping the current copy: %v", err)
					}
					time.Sleep(6 * time.Hour)
				}
			}()
		}
		addr := env("MINIBRAINZ_ADDR", ":8080")
		log.Printf("listening on %s", addr)
		s := &server{store: &store{path: dbPath}, key: os.Getenv("MINIBRAINZ_KEY")}
		log.Fatal(http.ListenAndServe(addr, s))
	case "refresh":
		f.minAge = 0
		if err := f.run(); err != nil {
			log.Fatal(err)
		}
	case "import":
		// Build from local dump files: minibrainz import OUT.sqlite FILE...
		if len(os.Args) < 4 {
			usage()
		}
		b, err := newBuilder(os.Args[2])
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
  MINIBRAINZ_REFRESH       "off" to never rebuild while serving (default on)
  MINIBRAINZ_REFRESH_DAYS  minimum days between rebuilds (default 7)
  MINIBRAINZ_DUMPS         dump base URL (default the MetaBrainz full export)`)
	os.Exit(2)
}
