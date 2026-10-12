package geodiff

import (
	"database/sql"
	"reflect"
	"testing"
)

// columnsTable mirrors a registry keyed by (table_name, column_name), like
// echidna's _echidna_columns or GeoPackage's gpkg_data_columns.
const columnsSchema = `CREATE TABLE cols (
	table_name TEXT NOT NULL,
	column_name TEXT NOT NULL,
	type TEXT,
	PRIMARY KEY (table_name, column_name)
);
CREATE TABLE tags (tag TEXT PRIMARY KEY, label TEXT);
INSERT INTO cols VALUES ('finds','kind','TEXT'), ('finds','depth','REAL'), ('finds','note','TEXT');
INSERT INTO tags VALUES ('a','A'), ('b','B');`

func dumpRows(t *testing.T, path, query string) [][]string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var a, b, c sql.NullString
		if err := rows.Scan(&a, &b, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, []string{a.String, b.String, c.String})
	}
	return out
}

// QA 107 end to end: theirs adds a table's column registrations and edits
// one row; ours renames a column (delete + insert of text-keyed rows) and
// edits another. Applying theirs and then the rebased ours gives both, with
// every key exactly as written.
func TestRebaseChangesets_TextAndCompositeKeysEndToEnd(t *testing.T) {
	base := tmpPath(t, "tk_base.sqlite")
	db, err := sql.Open("sqlite", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(columnsSchema); err != nil {
		t.Fatal(err)
	}
	db.Close()

	theirs := tmpPath(t, "tk_theirs.sqlite")
	ours := tmpPath(t, "tk_ours.sqlite")
	copyAndExec(t, base, theirs, `
		INSERT INTO cols VALUES ('other','name','TEXT'), ('other','depth','REAL');
		UPDATE cols SET type = 'INTEGER' WHERE table_name = 'finds' AND column_name = 'depth';
		INSERT INTO tags VALUES ('c','theirs');
		UPDATE tags SET label = 'A2' WHERE tag = 'a';`)
	copyAndExec(t, base, ours, `
		DELETE FROM cols WHERE table_name = 'finds' AND column_name = 'kind';
		INSERT INTO cols VALUES ('finds','category','TEXT');
		UPDATE cols SET type = 'BLOB' WHERE table_name = 'finds' AND column_name = 'note';
		INSERT INTO tags VALUES ('d','ours');
		DELETE FROM tags WHERE tag = 'b';`)

	baseTheirs := tmpPath(t, "tk_bt.diff")
	baseOurs := tmpPath(t, "tk_bo.diff")
	merged := tmpPath(t, "tk_merged.diff")
	if err := CreateChangeset(base, theirs, baseTheirs); err != nil {
		t.Fatal(err)
	}
	if err := CreateChangeset(base, ours, baseOurs); err != nil {
		t.Fatal(err)
	}
	conflicts, err := RebaseChangesets(baseTheirs, baseOurs, merged)
	if err != nil {
		t.Fatalf("RebaseChangesets: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("conflicts: %+v", conflicts)
	}

	// theirs + rebased ours, applied to a copy of theirs.
	result := tmpPath(t, "tk_result.sqlite")
	if err := FileCopy(result, theirs); err != nil {
		t.Fatal(err)
	}
	if err := ApplyChangeset(result, merged); err != nil {
		t.Fatalf("apply rebased: %v", err)
	}
	gotCols := dumpRows(t, result, "SELECT table_name, column_name, type FROM cols ORDER BY 1, 2")
	wantCols := [][]string{
		{"finds", "category", "TEXT"},
		{"finds", "depth", "INTEGER"},
		{"finds", "note", "BLOB"},
		{"other", "depth", "REAL"},
		{"other", "name", "TEXT"},
	}
	if !reflect.DeepEqual(gotCols, wantCols) {
		t.Fatalf("cols:\n got %v\nwant %v", gotCols, wantCols)
	}
	gotTags := dumpRows(t, result, "SELECT tag, label, NULL FROM tags ORDER BY 1")
	wantTags := [][]string{{"a", "A2", ""}, {"c", "theirs", ""}, {"d", "ours", ""}}
	if !reflect.DeepEqual(gotTags, wantTags) {
		t.Fatalf("tags:\n got %v\nwant %v", gotTags, wantTags)
	}

	// The in-place Rebase (ours file rebased onto theirs) agrees.
	if _, err := Rebase(base, theirs, ours, ""); err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if got := dumpRows(t, ours, "SELECT table_name, column_name, type FROM cols ORDER BY 1, 2"); !reflect.DeepEqual(got, wantCols) {
		t.Fatalf("Rebase cols:\n got %v\nwant %v", got, wantCols)
	}
	if got := dumpRows(t, ours, "SELECT tag, label, NULL FROM tags ORDER BY 1"); !reflect.DeepEqual(got, wantTags) {
		t.Fatalf("Rebase tags:\n got %v\nwant %v", got, wantTags)
	}
}
