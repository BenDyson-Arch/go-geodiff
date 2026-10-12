/*
 GEODIFF - MIT License
 Copyright (C) 2020 Martin Dobias

 Ported to Go: changesetutils.cpp + changesetutils.h
*/

package changeset

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ----- Conflict types -----

// ConflictItem describes a single column conflict during merge.
// Column is a 0-based index into the table schema (names are not in the blob).
type ConflictItem struct {
	Column int   `json:"column"`
	Base   Value `json:"base"`
	Theirs Value `json:"theirs"`
	Ours   Value `json:"ours"`
}

// ConflictFeature describes all conflicts for a single feature (row).
// This is the typed value the product stores and the review UI paints.
// It is not written into the changeset blob.
//
// PKValues holds the row's primary-key values in column order (one per PK
// column), exactly as in the changeset: text, blob and composite keys are
// kept as they are. PK is the key as a number, set only when the table has
// a single integer primary key (0 otherwise). Use FID for a display/JSON key.
type ConflictFeature struct {
	PK        int            `json:"pk"`
	PKValues  []Value        `json:"pk_values,omitempty"`
	TableName string         `json:"table"`
	Items     []ConflictItem `json:"items"`
}

// FID is the row's key as a string: the number for a single integer key
// (as before), the text of a single text key, base64 of a single blob key,
// and otherwise (composite or other types) a JSON array of the key values.
func (cf ConflictFeature) FID() string {
	switch len(cf.PKValues) {
	case 0:
		return fmt.Sprintf("%d", cf.PK)
	case 1:
		v := cf.PKValues[0]
		switch v.Type() {
		case TypeInt:
			n, _ := v.AsInt()
			return fmt.Sprintf("%d", n)
		case TypeText:
			s, _ := v.AsText()
			return s
		case TypeBlob:
			b, _ := v.AsBlob()
			return base64.StdEncoding.EncodeToString(b)
		}
	}
	vals := make([]any, len(cf.PKValues))
	for i, v := range cf.PKValues {
		vals[i], _ = valueToJSON(v)
	}
	raw, _ := json.Marshal(vals)
	return string(raw)
}

// IsValid reports whether this feature has at least one conflict item.
func (cf ConflictFeature) IsValid() bool { return len(cf.Items) > 0 }

// ----- Hex / Bin helpers -----

// HexToBin decodes a hexadecimal string into raw bytes.
// Uses encoding/hex.DecodeString under the hood.
func HexToBin(hexStr string) ([]byte, error) {
	return hex.DecodeString(hexStr)
}

// BinToHex encodes raw bytes as an uppercase hexadecimal string.
// Uses encoding/hex.EncodeToString under the hood (produces lowercase);
// the result is uppercased to match the C++ bin2hex behaviour.
func BinToHex(data []byte) string {
	return hex.EncodeToString(data)
}

// ----- valueToJSON -----

// valueToJSON converts a Value to a JSON-compatible Go value.
// The returned bool indicates whether the value should be included
// (false for TypeUndefined). For TypeNull, returns (nil, true) so the
// caller emits JSON null.
func valueToJSON(v Value) (any, bool) {
	switch v.Type() {
	case TypeUndefined:
		return nil, false
	case TypeNull:
		return nil, true
	case TypeInt:
		n, _ := v.AsInt()
		return n, true
	case TypeDouble:
		f, _ := v.AsDouble()
		return f, true
	case TypeText:
		s, _ := v.AsText()
		return s, true
	case TypeBlob:
		b, _ := v.AsBlob()
		return base64.StdEncoding.EncodeToString(b), true
	default:
		return "(unknown)", true
	}
}

// ----- Changeset to JSON -----

// changesetEntryToJSON converts a single ChangesetEntry to a
// map suitable for JSON marshalling (TableName → table key, etc.).
func changesetEntryToJSON(entry *ChangesetEntry) (map[string]any, error) {
	var status string
	switch entry.Op {
	case OpInsert:
		status = "insert"
	case OpUpdate:
		status = "update"
	case OpDelete:
		status = "delete"
	default:
		return nil, fmt.Errorf("unknown operation type %d", entry.Op)
	}

	nCol := entry.Table.ColumnCount()

	// Validate column counts (mirrors C++ check)
	if (entry.Op == OpUpdate || entry.Op == OpInsert) && nCol != len(entry.NewValues) {
		return nil, fmt.Errorf("table column count doesn't match new value list size")
	}
	if (entry.Op == OpUpdate || entry.Op == OpDelete) && nCol != len(entry.OldValues) {
		return nil, fmt.Errorf("table column count doesn't match old value list size")
	}

	changes := make([]map[string]any, 0)

	for i := 0; i < nCol; i++ {
		var valueNew, valueOld Value
		if entry.Op == OpUpdate || entry.Op == OpInsert {
			valueNew = entry.NewValues[i]
		}
		if entry.Op == OpUpdate || entry.Op == OpDelete {
			valueOld = entry.OldValues[i]
		}

		if valueNew.Type() == TypeUndefined && valueOld.Type() == TypeUndefined {
			continue
		}

		jsonValueOld, includeOld := valueToJSON(valueOld)
		jsonValueNew, includeNew := valueToJSON(valueNew)

		change := map[string]any{"column": i}

		if includeOld {
			change["old"] = jsonValueOld
		}
		if includeNew {
			change["new"] = jsonValueNew
		}

		changes = append(changes, change)
	}

	return map[string]any{
		"table":   entry.Table.Name,
		"type":    status,
		"changes": changes,
	}, nil
}

// ChangesetToJSON reads all entries from reader and returns the full
// changeset as JSON bytes. The top-level key is "geodiff".
func ChangesetToJSON(reader *Reader) ([]byte, error) {
	entries := make([]map[string]any, 0)

	for {
		entry, err := reader.NextEntry()
		if err != nil {
			return nil, err
		}
		if entry == nil {
			break
		}

		msg, err := changesetEntryToJSON(entry)
		if err != nil {
			return nil, err
		}
		entries = append(entries, msg)
	}

	return json.Marshal(map[string]any{"geodiff": entries})
}

// ChangesetToJSONSummary reads all entries from reader and returns a summary
// of insert/update/delete counts per table as JSON bytes.
// The top-level key is "geodiff_summary".
func ChangesetToJSONSummary(reader *Reader) ([]byte, error) {
	type tableSummary struct {
		inserts int
		updates int
		deletes int
	}
	summary := make(map[string]*tableSummary)

	for {
		entry, err := reader.NextEntry()
		if err != nil {
			return nil, err
		}
		if entry == nil {
			break
		}

		tableName := entry.Table.Name
		ts, ok := summary[tableName]
		if !ok {
			ts = &tableSummary{}
			summary[tableName] = ts
		}

		switch entry.Op {
		case OpInsert:
			ts.inserts++
		case OpUpdate:
			ts.updates++
		case OpDelete:
			ts.deletes++
		}
	}

	entries := make([]map[string]any, 0, len(summary))
	for tableName, ts := range summary {
		entries = append(entries, map[string]any{
			"table":  tableName,
			"insert": ts.inserts,
			"update": ts.updates,
			"delete": ts.deletes,
		})
	}

	return json.Marshal(map[string]any{"geodiff_summary": entries})
}

// ----- Conflicts to JSON -----

// conflictToJSON converts a single ConflictFeature to a JSON-compatible map.
func conflictToJSON(cf *ConflictFeature) (map[string]any, error) {
	changes := make([]map[string]any, 0, len(cf.Items))

	for _, item := range cf.Items {
		change := map[string]any{"column": item.Column}

		valBase, incBase := valueToJSON(item.Base)
		valTheirs, incTheirs := valueToJSON(item.Theirs)
		valOurs, incOurs := valueToJSON(item.Ours)

		if incBase {
			change["base"] = valBase
		}
		if incTheirs {
			change["theirs"] = valTheirs
		}
		if incOurs {
			change["ours"] = valOurs
		}

		changes = append(changes, change)
	}

	return map[string]any{
		"table":   cf.TableName,
		"type":    "conflict",
		"fid":     cf.FID(),
		"changes": changes,
	}, nil
}

// ConflictsToJSON converts a slice of ConflictFeature to JSON bytes.
// The top-level key is "geodiff".
func ConflictsToJSON(conflicts []ConflictFeature) ([]byte, error) {
	entries := make([]map[string]any, 0, len(conflicts))

	for i := range conflicts {
		msg, err := conflictToJSON(&conflicts[i])
		if err != nil {
			return nil, err
		}
		entries = append(entries, msg)
	}

	return json.Marshal(map[string]any{"geodiff": entries})
}

// ----- Invert -----

// InvertChangeset reads a changeset from reader and writes its inverse
// to writer.  INSERT ↔ DELETE, UPDATE swaps old/new with PK-column
// undefined-to-old-value reversal.
func InvertChangeset(reader *Reader, writer *Writer) error {
	var currentTableName string

	for {
		entry, err := reader.NextEntry()
		if err != nil {
			return err
		}
		if entry == nil {
			break
		}

		tableName := entry.Table.Name
		if tableName != currentTableName {
			if err := writer.BeginTable(entry.Table); err != nil {
				return err
			}
			currentTableName = tableName
		}

		switch entry.Op {
		case OpInsert:
			// INSERT becomes DELETE; old values are the original new values.
			out := ChangesetEntry{
				Op:        OpDelete,
				OldValues: cloneValues(entry.NewValues),
				Table:     entry.Table,
			}
			if err := writer.WriteEntry(out); err != nil {
				return err
			}

		case OpDelete:
			// DELETE becomes INSERT; new values are the original old values.
			out := ChangesetEntry{
				Op:        OpInsert,
				NewValues: cloneValues(entry.OldValues),
				Table:     entry.Table,
			}
			if err := writer.WriteEntry(out); err != nil {
				return err
			}

		case OpUpdate:
			// UPDATE: swap old and new, then reverse PK undefined-to-old.
			out := ChangesetEntry{
				Op:        OpUpdate,
				OldValues: cloneValues(entry.NewValues),
				NewValues: cloneValues(entry.OldValues),
				Table:     entry.Table,
			}
			// If a PK column in old was undefined, it means the original
			// entry carried the PK value only in "new" and left "old"
			// undefined.  After the swap we need to fix that.
			for i, isPK := range entry.Table.PrimaryKeys {
				if isPK && out.OldValues[i].Type() == TypeUndefined {
					out.OldValues[i] = out.NewValues[i]
					out.NewValues[i] = NewValueUndefined()
				}
			}
			if err := writer.WriteEntry(out); err != nil {
				return err
			}

		default:
			return fmt.Errorf("InvertChangeset: unknown entry operation %d", entry.Op)
		}
	}

	return nil
}

// cloneValues returns a deep copy of a []Value slice.
func cloneValues(src []Value) []Value {
	if src == nil {
		return nil
	}
	dst := make([]Value, len(src))
	copy(dst, src)
	return dst
}
