/*
 GEODIFF - MIT License
 Copyright (C) 2021 Martin Dobias

 Ported to Go: changesetconcat.cpp
*/

package changeset

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ----- PK key helpers (mirrors HashChangesetEntryPkey / EqualToChangesetEntryPkey) -----

// pkKey returns a string key that uniquely identifies a row by its primary-key
// column values.  For INSERT entries the PK is taken from NewValues; otherwise
// from OldValues.
func pkKey(entry *ChangesetEntry) string { return RowKey(entry) }

// PrimaryKeyValues returns the entry's primary-key values in column order:
// from NewValues for an INSERT, from OldValues otherwise.
func PrimaryKeyValues(entry *ChangesetEntry) []Value {
	values := entry.OldValues
	if entry.Op == OpInsert {
		values = entry.NewValues
	}
	var out []Value
	for i, isPK := range entry.Table.PrimaryKeys {
		if isPK && i < len(values) {
			out = append(out, values[i])
		}
	}
	return out
}

// RowKey returns a string that identifies the entry's row within its table:
// a type-tagged encoding of every primary-key column (text and blob values
// are length-prefixed, so no two distinct keys encode the same). Integer 1
// and text '1' are different keys.
func RowKey(entry *ChangesetEntry) string {
	var sb strings.Builder
	for _, v := range PrimaryKeyValues(entry) {
		switch v.Type() {
		case TypeInt:
			n, _ := v.AsInt()
			sb.WriteString("I")
			sb.WriteString(strconv.FormatInt(n, 10))
		case TypeDouble:
			f, _ := v.AsDouble()
			sb.WriteString("D")
			sb.WriteString(strconv.FormatUint(math.Float64bits(f), 16))
		case TypeText:
			s, _ := v.AsText()
			sb.WriteString("T")
			sb.WriteString(strconv.Itoa(len(s)))
			sb.WriteByte(':')
			sb.WriteString(s)
		case TypeBlob:
			b, _ := v.AsBlob()
			sb.WriteString("B")
			sb.WriteString(strconv.Itoa(len(b)))
			sb.WriteByte(':')
			sb.Write(b)
		case TypeNull:
			sb.WriteString("N")
		default:
			sb.WriteString("U")
		}
		sb.WriteByte(';')
	}
	return sb.String()
}

// ----- merge helpers (mirrors mergeValue / mergeUpdate) -----

// mergeValue returns vTwo if it is not undefined, otherwise vOne.
func mergeValue(vOne, vTwo Value) Value {
	if vTwo.Type() != TypeUndefined {
		return vTwo
	}
	return vOne
}

// mergeUpdate merges two UPDATE changes on the same row.  It returns
// false when the merge results in a no-op that can be discarded.
//
// Parameters map straight from the C++ version:
//
//	valuesOld1, valuesNew1 — the first update's old/new
//	valuesOld2, valuesNew2 — the second update's old/new (may be nil for DELETE→INSERT)
//	outputOld, outputNew   — output slices (appended to)
func mergeUpdate(
	t ChangesetTable,
	valuesOld1, valuesOld2 []Value,
	valuesNew1, valuesNew2 []Value,
	outputOld, outputNew *[]Value,
) bool {
	bRequired := false
	nCol := t.ColumnCount()

	for i := 0; i < nCol; i++ {
		var vOld2, vNew2 Value
		if len(valuesOld2) > 0 {
			vOld2 = valuesOld2[i]
		}
		if len(valuesNew2) > 0 {
			vNew2 = valuesNew2[i]
		}

		vOld := mergeValue(valuesOld1[i], vOld2)
		vNew := mergeValue(valuesNew1[i], vNew2)

		// If after merging there is no actual change on a non-PK column,
		// we can discard the whole merged update.
		if !vOld.Equal(vNew) && !t.PrimaryKeys[i] {
			bRequired = true
		}

		// write OLD
		if t.PrimaryKeys[i] || !vOld.Equal(vNew) {
			*outputOld = append(*outputOld, vOld)
		} else {
			*outputOld = append(*outputOld, NewValueUndefined())
		}

		// write NEW
		if t.PrimaryKeys[i] || vOld.Equal(vNew) {
			*outputNew = append(*outputNew, NewValueUndefined())
		} else {
			*outputNew = append(*outputNew, vNew)
		}
	}

	return bRequired
}

// ----- mergeEntries result (mirrors MergeEntriesResult enum) -----

type mergeResult int

const (
	mergeModified    mergeResult = iota // entry was updated in place
	mergeRemoved                        // entry should be removed
	mergeUnsupported                    // unexpected combination → discard newer
)

// mergeEntriesForRow merges entry e2 into e1 (e1 is the earlier entry,
// e2 is later).  e1 is modified in place on success; otherwise the
// caller must handle removal / unsupported cases.
func mergeEntriesForRow(e1, e2 *ChangesetEntry) mergeResult {
	// Unsupported combinations
	if (e1.Op == OpInsert && e2.Op == OpInsert) ||
		(e1.Op == OpUpdate && e2.Op == OpInsert) ||
		(e1.Op == OpDelete && e2.Op == OpUpdate) ||
		(e1.Op == OpDelete && e2.Op == OpDelete) {
		return mergeUnsupported
	}

	// INSERT + DELETE → cancel out
	if e1.Op == OpInsert && e2.Op == OpDelete {
		return mergeRemoved
	}

	// INSERT + UPDATE → modify INSERT with newer values
	if e1.Op == OpInsert && e2.Op == OpUpdate {
		nCol := e1.Table.ColumnCount()
		for i := 0; i < nCol; i++ {
			if e2.NewValues[i].Type() != TypeUndefined {
				e1.NewValues[i] = e2.NewValues[i]
			}
		}
		return mergeModified
	}

	// UPDATE + UPDATE → merge the two updates
	if e1.Op == OpUpdate && e2.Op == OpUpdate {
		var oldVals, newVals []Value
		if !mergeUpdate(e1.Table, e2.OldValues, e1.OldValues, e1.NewValues, e2.NewValues, &oldVals, &newVals) {
			return mergeRemoved
		}
		e1.OldValues = oldVals
		e1.NewValues = newVals
		return mergeModified
	}

	// UPDATE + DELETE → turn into DELETE, backfill old values from delete
	if e1.Op == OpUpdate && e2.Op == OpDelete {
		e1.Op = OpDelete
		nCol := e1.Table.ColumnCount()
		for i := 0; i < nCol; i++ {
			if e1.OldValues[i].Type() == TypeUndefined {
				e1.OldValues[i] = e2.OldValues[i]
			}
		}
		return mergeModified
	}

	// DELETE + INSERT → turn into UPDATE
	if e1.Op == OpDelete && e2.Op == OpInsert {
		var oldVals, newVals []Value
		if !mergeUpdate(e1.Table, e1.OldValues, nil, e2.NewValues, nil, &oldVals, &newVals) {
			return mergeRemoved
		}
		e1.Op = OpUpdate
		e1.OldValues = oldVals
		e1.NewValues = newVals
		return mergeModified
	}

	// All 9 cases covered; unreachable.
	return mergeUnsupported
}

// ----- concat table state -----

// concatTable holds the accumulated entries for one table during concatenation.
type concatTable struct {
	table   ChangesetTable             // owned copy of table schema
	entries map[string]*ChangesetEntry // PK key → entry (owned)
}

// ----- ConcatChangesets -----

// ConcatChangesets combines multiple changeset files into a single output
// file.  When the resulting changeset is applied, the effect is the same as
// applying the input files sequentially.  Incompatible changes that cannot
// be resolved are discarded.
//
// NOTE: This function buffers all entries in memory before writing the output.
// For very large changesets with many entries, this may require significant
// memory.
func ConcatChangesets(inputFiles []string, outputFile string) error {
	// table name → concatTable
	result := make(map[string]*concatTable)

	for _, inputFilename := range inputFiles {
		reader, err := NewReader(inputFilename)
		if err != nil {
			return fmt.Errorf("ConcatChangesets: unable to open input file %s: %w", inputFilename, err)
		}

		for {
			entry, err := reader.NextEntry()
			if err != nil {
				reader.Close()
				return err
			}
			if entry == nil {
				break
			}

			tableName := entry.Table.Name
			ct, ok := result[tableName]
			if !ok {
				// First time seeing this table — create a fresh accumulator.
				ct = &concatTable{
					table:   entry.Table,
					entries: make(map[string]*ChangesetEntry),
				}
				result[tableName] = ct

				e := &ChangesetEntry{
					Op:    entry.Op,
					Table: ct.table,
				}
				e.OldValues = append(e.OldValues, entry.OldValues...)
				e.NewValues = append(e.NewValues, entry.NewValues...)
				ct.entries[pkKey(entry)] = e
			} else {
				key := pkKey(entry)
				existing, found := ct.entries[key]
				if !found {
					// New PK for this table — just record it.
					e := &ChangesetEntry{
						Op:    entry.Op,
						Table: ct.table,
					}
					e.OldValues = append(e.OldValues, entry.OldValues...)
					e.NewValues = append(e.NewValues, entry.NewValues...)
					ct.entries[key] = e
				} else {
					// Merge the new entry into the existing one.
					mergeRes := mergeEntriesForRow(existing, entry)
					switch mergeRes {
					case mergeModified:
						// existing was updated in place — nothing more to do
					case mergeRemoved:
						delete(ct.entries, key)
					case mergeUnsupported:
						// Discard both entries (the C++ discards the newer entry
						// but also erases the old one when unsupported).
						delete(ct.entries, key)
					}
				}
			}
		}
		reader.Close()
	}

	// Write output.
	writer, err := NewWriter(outputFile)
	if err != nil {
		return err
	}

	for _, ct := range result {
		if len(ct.entries) == 0 {
			continue
		}
		if err := writer.BeginTable(ct.table); err != nil {
			writer.Close()
			return err
		}
		for _, e := range ct.entries {
			if err := writer.WriteEntry(*e); err != nil {
				writer.Close()
				return err
			}
		}
	}

	return writer.Close()
}
