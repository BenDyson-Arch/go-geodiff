package driver

import (
	"encoding/json"
	"testing"

	"github.com/BenDyson-Arch/go-geodiff/changeset"
)

// Tables keyed by text, by (text, text), and by blob, like
// _echidna_columns(table_name, column_name) or gpkg_data_columns.
var (
	textKeyTable      = changeset.ChangesetTable{Name: "tk", PrimaryKeys: []bool{true, false}}
	compositeKeyTable = changeset.ChangesetTable{Name: "ck", PrimaryKeys: []bool{true, true, false}}
	blobKeyTable      = changeset.ChangesetTable{Name: "bk", PrimaryKeys: []bool{true, false}}
	intPairKeyTable   = changeset.ChangesetTable{Name: "ik", PrimaryKeys: []bool{true, true, false}}
)

func txt(s string) changeset.Value { return changeset.NewValueText(s) }

func ins(tbl changeset.ChangesetTable, vals ...changeset.Value) changeset.ChangesetEntry {
	return changeset.ChangesetEntry{Op: changeset.OpInsert, Table: tbl, NewValues: vals}
}

func del(tbl changeset.ChangesetTable, vals ...changeset.Value) changeset.ChangesetEntry {
	return changeset.ChangesetEntry{Op: changeset.OpDelete, Table: tbl, OldValues: vals}
}

func upd(tbl changeset.ChangesetTable, old, new []changeset.Value) changeset.ChangesetEntry {
	return changeset.ChangesetEntry{Op: changeset.OpUpdate, Table: tbl, OldValues: old, NewValues: new}
}

var und = changeset.NewValueUndefined()

// rowValues returns the PK-and-data values an entry carries (new for insert).
func rowValues(e changeset.ChangesetEntry) []changeset.Value {
	if e.Op == changeset.OpInsert {
		return e.NewValues
	}
	return e.OldValues
}

func wantValues(t *testing.T, label string, got, want []changeset.Value) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d values %v, want %v", label, len(got), got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) || got[i].Type() != want[i].Type() {
			t.Fatalf("%s: column %d = %s, want %s (all: %v)", label, i, got[i], want[i], got)
		}
	}
}

// QA 107: both sides insert different rows into a (text, text)-keyed table.
// Ours must come out exactly as written, not renumbered to (1,1).
func TestRebase_CompositeTextKeyInsertsKeepTheirKeys(t *testing.T) {
	entries, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{
			ins(compositeKeyTable, txt("other"), txt("name"), txt("TEXT")),
			ins(compositeKeyTable, txt("other"), txt("depth"), txt("REAL")),
		},
		[]changeset.ChangesetEntry{ins(compositeKeyTable, txt("finds"), txt("category"), txt("TEXT"))},
	)
	if len(conflicts) != 0 {
		t.Fatalf("conflicts: %+v", conflicts)
	}
	if len(entries) != 1 || entries[0].Op != changeset.OpInsert {
		t.Fatalf("entries: %+v", entries)
	}
	wantValues(t, "insert", entries[0].NewValues, []changeset.Value{txt("finds"), txt("category"), txt("TEXT")})
}

// Two text keys that would both read as integer 0 are different rows.
func TestRebase_TextKeyInsertsDoNotCollide(t *testing.T) {
	entries, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{ins(textKeyTable, txt("alpha"), txt("theirs"))},
		[]changeset.ChangesetEntry{ins(textKeyTable, txt("beta"), txt("ours")), ins(textKeyTable, txt("gamma"), txt("ours"))},
	)
	if len(conflicts) != 0 {
		t.Fatalf("conflicts: %+v", conflicts)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	wantValues(t, "insert beta", entries[0].NewValues, []changeset.Value{txt("beta"), txt("ours")})
	wantValues(t, "insert gamma", entries[1].NewValues, []changeset.Value{txt("gamma"), txt("ours")})
}

// Same composite key inserted on both sides with the same values: nothing to
// do, no conflict.
func TestRebase_SameTextKeyInsertSameValues(t *testing.T) {
	row := []changeset.Value{txt("finds"), txt("category"), txt("TEXT")}
	entries, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{ins(compositeKeyTable, row...)},
		[]changeset.ChangesetEntry{ins(compositeKeyTable, row...)},
	)
	if len(conflicts) != 0 || len(entries) != 0 {
		t.Fatalf("entries %+v conflicts %+v", entries, conflicts)
	}
}

// Same text key inserted on both sides with different values: a text key
// can't be renumbered, so theirs stands and the difference is a conflict.
func TestRebase_SameTextKeyInsertDifferentValues(t *testing.T) {
	entries, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{ins(compositeKeyTable, txt("finds"), txt("category"), txt("TEXT"))},
		[]changeset.ChangesetEntry{ins(compositeKeyTable, txt("finds"), txt("category"), txt("INTEGER"))},
	)
	if len(entries) != 0 {
		t.Fatalf("entries: %+v", entries)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts: %+v", conflicts)
	}
	cf := conflicts[0]
	wantValues(t, "conflict key", cf.PKValues, []changeset.Value{txt("finds"), txt("category")})
	if len(cf.Items) != 1 || cf.Items[0].Column != 2 {
		t.Fatalf("items: %+v", cf.Items)
	}
	if th, _ := cf.Items[0].Theirs.AsText(); th != "TEXT" {
		t.Fatalf("theirs %q", th)
	}
	if o, _ := cf.Items[0].Ours.AsText(); o != "INTEGER" {
		t.Fatalf("ours %q", o)
	}
	if fid := cf.FID(); fid != `["finds","category"]` {
		t.Fatalf("fid %s", fid)
	}
}

// Updates and deletes of different text-keyed rows pass through untouched;
// before the fix every text key read as 0, so theirs' update of one row
// was taken as an edit of ours' row (and a delete of one as a delete of all).
func TestRebase_TextKeyUpdatesDeletesDifferentRows(t *testing.T) {
	entries, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{
			upd(compositeKeyTable, []changeset.Value{txt("finds"), txt("kind"), txt("TEXT")}, []changeset.Value{und, und, txt("INTEGER")}),
			del(compositeKeyTable, txt("finds"), txt("note"), txt("TEXT")),
		},
		[]changeset.ChangesetEntry{
			upd(compositeKeyTable, []changeset.Value{txt("finds"), txt("depth"), txt("REAL")}, []changeset.Value{und, und, txt("TEXT")}),
			del(compositeKeyTable, txt("finds"), txt("site"), txt("TEXT")),
		},
	)
	if len(conflicts) != 0 {
		t.Fatalf("conflicts: %+v", conflicts)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	if entries[0].Op != changeset.OpUpdate {
		t.Fatalf("entry 0: %+v", entries[0])
	}
	wantValues(t, "update old", entries[0].OldValues, []changeset.Value{txt("finds"), txt("depth"), txt("REAL")})
	wantValues(t, "update new", entries[0].NewValues, []changeset.Value{und, und, txt("TEXT")})
	if entries[1].Op != changeset.OpDelete {
		t.Fatalf("entry 1: %+v", entries[1])
	}
	wantValues(t, "delete", entries[1].OldValues, []changeset.Value{txt("finds"), txt("site"), txt("TEXT")})
}

// Same text-keyed row: update/update conflicts carry the real key; update vs
// delete is dropped with a conflict; delete/delete is dropped.
func TestRebase_TextKeySameRow(t *testing.T) {
	key := []changeset.Value{txt("finds"), txt("kind")}
	old := append(append([]changeset.Value{}, key...), txt("TEXT"))
	entries, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{upd(compositeKeyTable, old, []changeset.Value{und, und, txt("INTEGER")})},
		[]changeset.ChangesetEntry{upd(compositeKeyTable, old, []changeset.Value{und, und, txt("REAL")})},
	)
	if len(entries) != 0 || len(conflicts) != 1 {
		t.Fatalf("update/update: entries %+v conflicts %+v", entries, conflicts)
	}
	wantValues(t, "update/update key", conflicts[0].PKValues, key)

	entries, conflicts = rebaseEntries(t,
		[]changeset.ChangesetEntry{del(compositeKeyTable, old...)},
		[]changeset.ChangesetEntry{upd(compositeKeyTable, old, []changeset.Value{und, und, txt("REAL")})},
	)
	if len(entries) != 0 || len(conflicts) != 1 {
		t.Fatalf("delete/update: entries %+v conflicts %+v", entries, conflicts)
	}
	wantValues(t, "delete/update key", conflicts[0].PKValues, key)

	entries, conflicts = rebaseEntries(t,
		[]changeset.ChangesetEntry{del(compositeKeyTable, old...)},
		[]changeset.ChangesetEntry{del(compositeKeyTable, old...)},
	)
	if len(entries) != 0 || len(conflicts) != 0 {
		t.Fatalf("delete/delete: entries %+v conflicts %+v", entries, conflicts)
	}

	// Theirs edited one column, ours another: ours' column survives, keyed
	// by the original text key.
	theirsTbl := changeset.ChangesetTable{Name: "ck4", PrimaryKeys: []bool{true, true, false, false}}
	base := []changeset.Value{txt("finds"), txt("kind"), txt("TEXT"), txt("a")}
	entries, conflicts = rebaseEntries(t,
		[]changeset.ChangesetEntry{upd(theirsTbl, base, []changeset.Value{und, und, txt("INTEGER"), und})},
		[]changeset.ChangesetEntry{upd(theirsTbl, base, []changeset.Value{und, und, und, txt("b")})},
	)
	if len(conflicts) != 0 || len(entries) != 1 {
		t.Fatalf("different columns: entries %+v conflicts %+v", entries, conflicts)
	}
	wantValues(t, "different columns old", entries[0].OldValues, []changeset.Value{txt("finds"), txt("kind"), und, txt("a")})
	wantValues(t, "different columns new", entries[0].NewValues, []changeset.Value{und, und, und, txt("b")})
}

// Blob keys are compared and kept byte for byte.
func TestRebase_BlobKeyPreserved(t *testing.T) {
	k1 := changeset.NewValueBlob([]byte{0x00, 0x01})
	k2 := changeset.NewValueBlob([]byte{0x00, 0x02})
	entries, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{ins(blobKeyTable, k1, txt("theirs"))},
		[]changeset.ChangesetEntry{ins(blobKeyTable, k2, txt("ours")), del(blobKeyTable, k1, txt("x"))},
	)
	if len(conflicts) != 0 || len(entries) != 2 {
		t.Fatalf("entries %+v conflicts %+v", entries, conflicts)
	}
	wantValues(t, "blob insert", entries[0].NewValues, []changeset.Value{k2, txt("ours")})
	wantValues(t, "blob delete", entries[1].OldValues, []changeset.Value{k1, txt("x")})
}

// A composite integer key can't be renumbered either (which column?): both
// columns are kept; a same-key insert is a conflict, not (n, n).
func TestRebase_CompositeIntKeyNotRenumbered(t *testing.T) {
	i := func(n int64) changeset.Value { return changeset.NewValueInt(n) }
	entries, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{ins(intPairKeyTable, i(1), i(2), txt("theirs"))},
		[]changeset.ChangesetEntry{ins(intPairKeyTable, i(1), i(3), txt("ours")), ins(intPairKeyTable, i(1), i(2), txt("ours"))},
	)
	if len(entries) != 1 {
		t.Fatalf("entries: %+v", entries)
	}
	wantValues(t, "insert", entries[0].NewValues, []changeset.Value{i(1), i(3), txt("ours")})
	if len(conflicts) != 1 {
		t.Fatalf("conflicts: %+v", conflicts)
	}
	wantValues(t, "conflict key", conflicts[0].PKValues, []changeset.Value{i(1), i(2)})
}

// Integer keys still renumber, and the conflict FID of an integer key is
// the number.
func TestRebase_IntKeyConflictFID(t *testing.T) {
	_, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{makeUpdateEntry2("t", 7, "a", "theirs")},
		[]changeset.ChangesetEntry{makeUpdateEntry2("t", 7, "a", "ours")},
	)
	if len(conflicts) != 1 || conflicts[0].PK != 7 || conflicts[0].FID() != "7" {
		t.Fatalf("conflicts: %+v", conflicts)
	}
	raw, err := MarshalConflicts(conflicts)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Geodiff []struct {
			FID string `json:"fid"`
		} `json:"geodiff"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Geodiff) != 1 || doc.Geodiff[0].FID != "7" {
		t.Fatalf("json %s (%v)", raw, err)
	}
}

// A text key that happens to look like a number is still text.
func TestRebase_NumericLookingTextKey(t *testing.T) {
	entries, conflicts := rebaseEntries(t,
		[]changeset.ChangesetEntry{ins(textKeyTable, txt("1"), txt("theirs"))},
		[]changeset.ChangesetEntry{ins(textKeyTable, txt("2"), txt("ours"))},
	)
	if len(conflicts) != 0 || len(entries) != 1 {
		t.Fatalf("entries %+v conflicts %+v", entries, conflicts)
	}
	wantValues(t, "insert", rowValues(entries[0]), []changeset.Value{txt("2"), txt("ours")})
}
