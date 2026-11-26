package main

import "strings"

// The MusicBrainz tables kept, with every column in dump order. A leading "-"
// drops the column; ":t" is text, ":g" an MBID (stored as 16 bytes), and the
// rest are integers. Column lists come from MusicBrainz's
// admin/sql/CreateTables.sql; a row with a different column count stops the
// import, so a schema change can't load silently wrong data.
//
// To keep more of MusicBrainz (relationships, credits, tags), add tables here.
var manifest = parseManifest(`
area                  id -gid name:t -type -edits_pending -last_updated -begin_date_year -begin_date_month -begin_date_day -end_date_year -end_date_month -end_date_day -ended -comment
iso_3166_1            area code:t
artist                id gid:g name:t sort_name:t begin_date_year begin_date_month begin_date_day end_date_year end_date_month end_date_day type area -gender comment:t -edits_pending -last_updated ended begin_area -end_area
artist_type           id name:t -parent -child_order -description -gid
artist_alias          -id artist name:t locale:t -edits_pending -last_updated type sort_name:t -begin_date_year -begin_date_month -begin_date_day -end_date_year -end_date_month -end_date_day -primary_for_locale -ended
artist_credit         id name:t artist_count -ref_count -created -edits_pending -gid
artist_credit_name    artist_credit position artist name:t join_phrase:t
release_group         id gid:g name:t artist_credit type comment:t -edits_pending -last_updated
release_group_meta    id release_count first_release_date_year first_release_date_month first_release_date_day rating rating_count
release_group_primary_type        id name:t -parent -child_order -description -gid
release_group_secondary_type      id name:t -parent -child_order -description -gid
release_group_secondary_type_join release_group secondary_type -created
release               id gid:g name:t artist_credit release_group status -packaging -language -script barcode:t comment:t -edits_pending -quality -last_updated
release_status        id name:t -parent -child_order -description -gid
release_country       release country date_year date_month date_day
release_unknown_country release date_year date_month date_day
release_label         -id release label catalog_number:t -last_updated
label                 id gid:g name:t -begin_date_year -begin_date_month -begin_date_day -end_date_year -end_date_month -end_date_day -label_code -type -area -comment -edits_pending -last_updated -ended
medium                id release position format name:t -edits_pending -last_updated track_count -gid
medium_format         id name:t -parent -child_order -year -has_discids -description -gid
track                 id gid:g recording medium position number:t name:t artist_credit length -edits_pending -last_updated -is_data_track
recording             id gid:g name:t artist_credit length comment:t -edits_pending -last_updated video
url                   id -gid url:t -edits_pending -last_updated
l_artist_url          -id link entity0 entity1 -edits_pending -last_updated -link_order -entity0_credit -entity1_credit
link                  id link_type -begin_date_year -begin_date_month -begin_date_day -end_date_year -end_date_month -end_date_day -attribute_count -created -ended
link_type             id -parent -child_order -gid -entity_type0 -entity_type1 name:t -description -link_phrase -reverse_link_phrase -long_link_phrase -last_updated -is_deprecated -has_dates -entity0_cardinality -entity1_cardinality
genre                 -id -gid name:t -comment -edits_pending -last_updated
tag                   id name:t -ref_count
artist_tag            artist tag count -last_updated
`)

// Built after loading: indexes, search and popularity.
const postSQL = `
CREATE UNIQUE INDEX artist_gid ON artist(gid);
CREATE UNIQUE INDEX release_group_gid ON release_group(gid);
CREATE UNIQUE INDEX release_gid ON release(gid);
CREATE UNIQUE INDEX recording_gid ON recording(gid);
CREATE INDEX artist_alias_artist ON artist_alias(artist);
CREATE INDEX artist_credit_name_ac ON artist_credit_name(artist_credit, position);
CREATE INDEX artist_credit_name_artist ON artist_credit_name(artist);
CREATE INDEX release_group_ac ON release_group(artist_credit);
CREATE INDEX rg_secondary_rg ON release_group_secondary_type_join(release_group);
CREATE INDEX release_rg ON release(release_group);
CREATE INDEX release_barcode ON release(barcode) WHERE barcode <> '';
CREATE INDEX release_country_release ON release_country(release);
CREATE INDEX release_unknown_country_release ON release_unknown_country(release);
CREATE INDEX release_label_release ON release_label(release);
CREATE INDEX medium_release ON medium(release, position);
CREATE INDEX track_medium ON track(medium, position);
CREATE INDEX l_artist_url_artist ON l_artist_url(entity0);
CREATE INDEX artist_tag_artist ON artist_tag(artist);
CREATE INDEX genre_name ON genre(name);
CREATE INDEX iso_3166_1_area ON iso_3166_1(area);

-- Contentless: only the index is stored; rows are joined back by rowid (the MusicBrainz id).
CREATE VIRTUAL TABLE artist_fts USING fts5(name, aliases, content='', tokenize='unicode61 remove_diacritics 2', prefix='2 3');
INSERT INTO artist_fts(rowid, name, aliases)
  SELECT a.id, a.name, a.sort_name || ' ' || coalesce((SELECT group_concat(name, ' ') FROM artist_alias WHERE artist = a.id), '')
  FROM artist a;
CREATE VIRTUAL TABLE release_group_fts USING fts5(title, artist, content='', tokenize='unicode61 remove_diacritics 2', prefix='2 3');
INSERT INTO release_group_fts(rowid, title, artist)
  SELECT rg.id, rg.name, ac.name FROM release_group rg JOIN artist_credit ac ON ac.id = rg.artist_credit;

-- Popularity until ListenBrainz counts are added: how many release groups an
-- artist is credited on, and (release_group_meta) how many releases a group has.
CREATE TABLE artist_popularity (artist INTEGER PRIMARY KEY, score INTEGER);
INSERT INTO artist_popularity
  SELECT acn.artist, count(*) FROM release_group rg JOIN artist_credit_name acn ON acn.artist_credit = rg.artist_credit GROUP BY acn.artist;

ANALYZE;
`

type column struct {
	name string
	kind byte // 'i' integer, 't' text, 'g' MBID
	keep bool
}

type table struct {
	name string
	cols []column
}

func (t table) kept() []column {
	var out []column
	for _, c := range t.cols {
		if c.keep {
			out = append(out, c)
		}
	}
	return out
}

func (t table) createSQL() string {
	var defs []string
	for i, c := range t.kept() {
		typ := map[byte]string{'i': "INTEGER", 't': "TEXT", 'g': "BLOB"}[c.kind]
		if i == 0 && c.name == "id" {
			typ += " PRIMARY KEY"
		}
		defs = append(defs, c.name+" "+typ)
	}
	return "CREATE TABLE " + t.name + " (" + strings.Join(defs, ", ") + ")"
}

func parseManifest(spec string) []table {
	var tables []table
	for _, line := range strings.Split(strings.TrimSpace(spec), "\n") {
		fields := strings.Fields(line)
		t := table{name: fields[0]}
		for _, f := range fields[1:] {
			c := column{keep: !strings.HasPrefix(f, "-"), kind: 'i'}
			f = strings.TrimPrefix(f, "-")
			if name, kind, ok := strings.Cut(f, ":"); ok {
				f, c.kind = name, kind[0]
			}
			c.name = f
			t.cols = append(t.cols, c)
		}
		tables = append(tables, t)
	}
	return tables
}
