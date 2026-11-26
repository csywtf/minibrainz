# minibrainz

A small, fast, read-only copy of [MusicBrainz](https://musicbrainz.org), built from its
[public data dumps](https://musicbrainz.org/doc/MusicBrainz_Database/Download) and served over
HTTP in the same JSON as the musicbrainz.org `/ws/2` API.

musicbrainz.org asks clients to make at most one request per second. That's fine for tagging,
but slow for an app whose every search, artist page and album page is a MusicBrainz call.
minibrainz answers the same requests from a local SQLite file in a few milliseconds.

## What it serves

A subset of `/ws/2`, the requests a music app typically makes:

| Request | |
| --- | --- |
| `artist?query=` | Artist search, with a 0–100 `score` |
| `artist/{mbid}?inc=url-rels+genres` | Artist lookup |
| `release-group?query=` | Album search |
| `release-group?artist={mbid}&type=album\|ep\|single` | An artist's albums, all of them (no 100-item pages) |
| `release-group/{mbid}` | Album lookup |
| `release?release-group={mbid}&inc=media` | An album's releases |
| `release/{mbid}?inc=recordings+artist-credits+release-groups+labels` | A release with its track list |
| `recording/{mbid}` | Recording lookup |

Anything else returns 404, so a client can fall back to musicbrainz.org. Search ranks exact
matches first, then by popularity (for now, how many releases an album has).

## Data

The copy holds artists (with aliases), artist credits, release groups, releases (barcode, date,
country, label), mediums, tracks, recordings, artist links and genres: about 12 GB. It is built
from `mbdump.tar.bz2` and `mbdump-derived.tar.bz2`, streamed straight from the MetaBrainz
mirror, so nothing but the database is written to disk. Each download is checked against the
published SHA-256 sums, and a new build only replaces the old one if it looks complete.

Dumps come out twice a week; the server checks every six hours and rebuilds once the copy is a
week old. A build takes roughly an hour or two and needs room for two copies of the database.

MusicBrainz data is [CC0 and CC BY-NC-SA](https://musicbrainz.org/doc/About/Data_License).
Consider [supporting MetaBrainz](https://metabrainz.org/donate).

## Running it

```sh
docker build -t minibrainz .
docker run -d -p 8080:8080 -v minibrainz:/data -e MINIBRAINZ_KEY=change-me minibrainz
curl -H "Authorization: Bearer change-me" "localhost:8080/ws/2/artist?query=bjork"
```

The first build starts on launch; until it finishes, requests get 503. `GET /health` reports the
dump in use.

| Setting | Default | |
| --- | --- | --- |
| `MINIBRAINZ_DATA` | `/data` in Docker | Folder holding `mb.sqlite` |
| `MINIBRAINZ_ADDR` | `:8080` | Listen address |
| `MINIBRAINZ_KEY` | none | If set, requests need `Authorization: Bearer <key>` |
| `MINIBRAINZ_REFRESH` | `on` | `off` to never rebuild while serving |
| `MINIBRAINZ_REFRESH_DAYS` | `7` | Minimum days between rebuilds |
| `MINIBRAINZ_DUMPS` | MetaBrainz full export | Base URL of the dumps |

Without Docker (needs a C compiler for SQLite):

```sh
go build -tags sqlite_fts5 .
./minibrainz refresh        # build data/mb.sqlite from the latest dump
./minibrainz serve
./minibrainz import out.sqlite mbdump-sample.tar.xz   # or build from local dump files
```

Tests: `go test -tags sqlite_fts5 ./...`

## Plans

- Popularity from ListenBrainz listen counts
- Song search and typo-tolerant search
- Artist photos and biographies from Wikipedia and Wikidata
- Optionally, falling back to musicbrainz.org itself for anything newer than the dump
