/*
GEODIFF - MIT License
Copyright (C) 2019 Peter Petrik

Go port of geodiffrebase.cpp — the 3-way merge/rebase engine.
*/

package driver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/BenDyson-Arch/go-geodiff/changeset"
)

// ConflictItem / ConflictFeature are the typed rebase conflicts.
// Canonical definition is changeset.ConflictFeature — the public geodiff
// API re-exports the same type. Values are not written into the blob.
type ConflictItem = changeset.ConflictItem
type ConflictFeature = changeset.ConflictFeature

// ---------------------------------------------------------------------------
// Internal types for tracking rebase state
// ---------------------------------------------------------------------------

// Rows are identified by changeset.RowKey: every primary-key column, typed.
// Text, blob and composite keys are compared and written back exactly as
// they are. Only a table with a single integer primary key can have our
// insert renumbered when theirs inserted the same key.

// tableRebaseInfo tracks the state of a single table extracted from the
// "theirs" changeset.
type tableRebaseInfo struct {
	inserted map[string][]changeset.Value // row key → theirs' inserted values
	deleted  map[string]bool              // row keys theirs deleted
	updated  map[string][]changeset.Value // row key → theirs' new column values

	hasIntInserted bool  // theirs inserted a single-integer key
	maxIntInserted int64 // the largest such key
}

// databaseRebaseInfo tracks the state of all tables from the "theirs" changeset.
type databaseRebaseInfo struct {
	tables map[string]*tableRebaseInfo
}

// pkeyMapping is what happens to one of our rows: renumbered to newPK, or
// gone (theirs deleted it).
type pkeyMapping struct {
	newPK int64
	gone  bool
}

// rebaseMapping tracks how primary keys need to be remapped in the rebased changeset.
type rebaseMapping struct {
	// table name → row key → mapping
	mapIds map[string]map[string]pkeyMapping

	// table name → integer insert PKs (→ row key) that haven't been remapped yet
	// (important because our mapping could cause FID conflicts with PKs
	// that weren't previously in conflict, e.g. if 4,5,6 get mapped
	// 4→6, 5→7 then the original 6 will need to be remapped too: 6→8)
	unmappedInsertIds map[string]map[int64]string

	// table name → row keys both sides inserted that can't be renumbered
	// (text, blob or composite keys): theirs' row stands.
	collided map[string]map[string]bool
}

func newRebaseMapping() *rebaseMapping {
	return &rebaseMapping{
		mapIds:            make(map[string]map[string]pkeyMapping),
		unmappedInsertIds: make(map[string]map[int64]string),
		collided:          make(map[string]map[string]bool),
	}
}

func (m *rebaseMapping) add(table, key string, pm pkeyMapping) {
	ids, ok := m.mapIds[table]
	if !ok {
		ids = make(map[string]pkeyMapping)
		m.mapIds[table] = ids
	}
	ids[key] = pm
}

func (m *rebaseMapping) lookup(table, key string) (pkeyMapping, bool) {
	pm, ok := m.mapIds[table][key]
	return pm, ok
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// errNoPrimaryKey is returned when a changeset entry lacks a primary key column.
var errNoPrimaryKey = fmt.Errorf("entry has no primary key column")

// rowKey identifies the entry's row (see changeset.RowKey).
func rowKey(entry *changeset.ChangesetEntry) (string, error) {
	if !hasPrimaryKey(entry) {
		return "", errNoPrimaryKey
	}
	return changeset.RowKey(entry), nil
}

func hasPrimaryKey(entry *changeset.ChangesetEntry) bool {
	for _, isPK := range entry.Table.PrimaryKeys {
		if isPK {
			return true
		}
	}
	return false
}

// singleIntPK returns the key of a table with exactly one primary-key
// column holding an integer — the only kind of key rebase may renumber.
func singleIntPK(entry *changeset.ChangesetEntry) (int64, bool) {
	vals := changeset.PrimaryKeyValues(entry)
	if len(vals) != 1 || vals[0].Type() != changeset.TypeInt {
		return 0, false
	}
	n, _ := vals[0].AsInt()
	return n, true
}

// newConflict starts a conflict for the entry's row, carrying its key.
func newConflict(entry *changeset.ChangesetEntry) ConflictFeature {
	cf := ConflictFeature{TableName: entry.Table.Name, PKValues: changeset.PrimaryKeyValues(entry)}
	if n, ok := singleIntPK(entry); ok {
		cf.PK = int(n)
	}
	return cf
}

// setPK writes a renumbered single-integer key into vals.
func setPK(entry *changeset.ChangesetEntry, vals []changeset.Value, newPK int64) {
	for i, isPK := range entry.Table.PrimaryKeys {
		if isPK {
			vals[i] = changeset.NewValueInt(newPK)
		}
	}
}

// fileBytesEmpty returns true if the file does not exist or is empty.
func fileBytesEmpty(filename string) bool {
	data, err := os.ReadFile(filename)
	if err != nil {
		return true
	}
	return len(data) == 0
}

// copyFile copies a file from src to dst.
func copyFileContents(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("copyFileContents read: %w", err)
	}
	return os.WriteFile(dst, data, 0644)
}

// ---------------------------------------------------------------------------
// Step 1: Parse the "theirs" changeset into databaseRebaseInfo
// ---------------------------------------------------------------------------

func parseOldChangeset(reader *changeset.Reader, dbInfo *databaseRebaseInfo) error {
	for {
		entry, err := reader.NextEntry()
		if err != nil {
			return fmt.Errorf("parseOldChangeset: %w", err)
		}
		if entry == nil {
			break
		}

		tableName := entry.Table.Name
		tableInfo, ok := dbInfo.tables[tableName]
		if !ok {
			tableInfo = &tableRebaseInfo{
				inserted: make(map[string][]changeset.Value),
				deleted:  make(map[string]bool),
				updated:  make(map[string][]changeset.Value),
			}
			dbInfo.tables[tableName] = tableInfo
		}

		key, err := rowKey(entry)
		if err != nil {
			return fmt.Errorf("parseOldChangeset: %w", err)
		}

		switch entry.Op {
		case changeset.OpInsert:
			tableInfo.inserted[key] = append([]changeset.Value(nil), entry.NewValues...)
			if n, ok := singleIntPK(entry); ok {
				if !tableInfo.hasIntInserted || n > tableInfo.maxIntInserted {
					tableInfo.maxIntInserted = n
				}
				tableInfo.hasIntInserted = true
			}
		case changeset.OpDelete:
			tableInfo.deleted[key] = true
		case changeset.OpUpdate:
			tableInfo.updated[key] = append([]changeset.Value(nil), entry.NewValues...)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Step 2: Build RebaseMapping from "ours" changeset
// ---------------------------------------------------------------------------

func findMappingForNewChangeset(
	reader *changeset.Reader,
	dbInfo *databaseRebaseInfo,
	mapping *rebaseMapping,
	freeIndices map[string]int64,
) error {
	// Calculate initial free indices: max(theirs.inserted) + 1 per table
	// (at least 1, as upstream).
	for tableName, tableInfo := range dbInfo.tables {
		if tableInfo.hasIntInserted {
			freeIndices[tableName] = max(tableInfo.maxIntInserted, 0) + 1
		}
	}

	// First pass: detect conflicts
	for {
		entry, err := reader.NextEntry()
		if err != nil {
			return fmt.Errorf("findMappingForNewChangeset: %w", err)
		}
		if entry == nil {
			break
		}

		tableName := entry.Table.Name
		tableInfo, ok := dbInfo.tables[tableName]
		if !ok {
			continue // table not in theirs records, no rebasing needed
		}

		key, err := rowKey(entry)
		if err != nil {
			return fmt.Errorf("findMappingForNewChangeset: %w", err)
		}

		switch entry.Op {
		case changeset.OpInsert:
			pk, isInt := singleIntPK(entry)
			if _, theirsToo := tableInfo.inserted[key]; theirsToo {
				if !isInt {
					// Text, blob or composite key: there is no "next free"
					// value, so theirs' row stands (see handleInsert).
					set, ok := mapping.collided[tableName]
					if !ok {
						set = make(map[string]bool)
						mapping.collided[tableName] = set
					}
					set[key] = true
					continue
				}
				// Both theirs and ours inserted the same PK → renumber ours
				freeIdx, exists := freeIndices[tableName]
				if !exists {
					return fmt.Errorf("internal error: freeIndices missing for %s", tableName)
				}
				mapping.add(tableName, key, pkeyMapping{newPK: freeIdx})
				freeIndices[tableName] = freeIdx + 1
			} else if isInt {
				// Keep track of unmapped inserts for later conflict resolution
				set, exists := mapping.unmappedInsertIds[tableName]
				if !exists {
					set = make(map[int64]string)
					mapping.unmappedInsertIds[tableName] = set
				}
				set[pk] = key
			}

		case changeset.OpUpdate, changeset.OpDelete:
			if tableInfo.deleted[key] {
				// Update or delete of a row theirs deleted
				mapping.add(tableName, key, pkeyMapping{gone: true})
			}
		}
	}

	// Finalize: if unmapped insert PKs conflict with remapped PKs, remap those
	// too. Ascending order, so a kept key is never handed out later.
	for tableName, unmapped := range mapping.unmappedInsertIds {
		usedNewPKs := make(map[int64]bool)
		for _, pm := range mapping.mapIds[tableName] {
			if !pm.gone {
				usedNewPKs[pm.newPK] = true
			}
		}

		pks := make([]int64, 0, len(unmapped))
		for pk := range unmapped {
			pks = append(pks, pk)
		}
		slices.Sort(pks)
		for _, pk := range pks {
			if usedNewPKs[pk] {
				// Our mapping has introduced a conflict in IDs → remap this old PK too
				freeIdx, exists := freeIndices[tableName]
				if !exists {
					return fmt.Errorf("internal error: freeIndices missing (2) for %s", tableName)
				}
				mapping.add(tableName, unmapped[pk], pkeyMapping{newPK: freeIdx})
				usedNewPKs[freeIdx] = true
				freeIndices[tableName] = freeIdx + 1
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Step 3: Entry handlers for producing rebased changeset
// ---------------------------------------------------------------------------

func handleInsert(
	entry *changeset.ChangesetEntry,
	mapping *rebaseMapping,
	tableInfo *tableRebaseInfo,
	outEntry *changeset.ChangesetEntry,
	conflicts *[]ConflictFeature,
) (bool, error) {
	key, err := rowKey(entry)
	if err != nil {
		return false, err
	}

	if mapping.collided[entry.Table.Name][key] {
		// Both inserted a row with this key and it can't be renumbered:
		// theirs stands; any value ours inserted differently is a conflict.
		theirsVals := tableInfo.inserted[key]
		cf := newConflict(entry)
		for i, isPK := range entry.Table.PrimaryKeys {
			if isPK || i >= len(theirsVals) || theirsVals[i].Equal(entry.NewValues[i]) {
				continue
			}
			addConflictItem(&cf, i, changeset.NewValueUndefined(), theirsVals[i], entry.NewValues[i])
		}
		if cf.IsValid() {
			*conflicts = append(*conflicts, cf)
		}
		return false, nil
	}

	outEntry.Op = changeset.OpInsert
	outEntry.NewValues = append([]changeset.Value(nil), entry.NewValues...)
	if pm, ok := mapping.lookup(entry.Table.Name, key); ok && !pm.gone {
		setPK(entry, outEntry.NewValues, pm.newPK)
	}
	return true, nil
}

func handleDelete(
	entry *changeset.ChangesetEntry,
	mapping *rebaseMapping,
	tableInfo *tableRebaseInfo,
	outEntry *changeset.ChangesetEntry,
) (bool, error) {
	numColumns := entry.Table.ColumnCount()
	outEntry.Op = changeset.OpDelete
	outEntry.OldValues = make([]changeset.Value, numColumns)

	key, err := rowKey(entry)
	if err != nil {
		return false, err
	}

	pm, mapped := mapping.lookup(entry.Table.Name, key)
	if mapped && pm.gone {
		// Both deleted: skip
		return false, nil
	}
	renumbered := mapped

	// Find previously new values (from theirs update) to use as base.
	// A renumbered key is our own inserted row, not theirs' row of that number.
	patchedVals, wasUpdated := tableInfo.updated[key]
	if !wasUpdated || renumbered {
		patchedVals = make([]changeset.Value, numColumns)
	}

	for i := 0; i < numColumns; i++ {
		patchedVal := patchedVals[i]
		if !entry.Table.PrimaryKeys[i] && !patchedVal.IsUndefined() {
			outEntry.OldValues[i] = patchedVal
		} else {
			outEntry.OldValues[i] = entry.OldValues[i]
		}
	}
	if renumbered {
		setPK(entry, outEntry.OldValues, pm.newPK)
	}
	return true, nil
}

func addConflictItem(cf *changeset.ConflictFeature, col int, base, theirs, ours changeset.Value) {
	// Column 4 of gpkg_contents is the last_change timestamp — not a conflict
	if cf.TableName == "gpkg_contents" && col == 4 {
		return
	}
	cf.Items = append(cf.Items, changeset.ConflictItem{
		Column: col,
		Base:   base,
		Theirs: theirs,
		Ours:   ours,
	})
}

func handleUpdate(
	entry *changeset.ChangesetEntry,
	mapping *rebaseMapping,
	tableInfo *tableRebaseInfo,
	outEntry *changeset.ChangesetEntry,
	conflicts *[]ConflictFeature,
) (bool, error) {
	numColumns := entry.Table.ColumnCount()
	outEntry.Op = changeset.OpUpdate
	outEntry.OldValues = make([]changeset.Value, numColumns)
	outEntry.NewValues = make([]changeset.Value, numColumns)

	key, err := rowKey(entry)
	if err != nil {
		return false, err
	}

	// Check if this update conflicts with a theirs delete
	pm, mapped := mapping.lookup(entry.Table.Name, key)
	if mapped && pm.gone {
		// Our UPDATE conflicts with their DELETE: delete wins, record conflict
		cf := newConflict(entry)
		for i := 0; i < numColumns; i++ {
			if !entry.NewValues[i].IsUndefined() {
				addConflictItem(&cf, i, entry.OldValues[i], changeset.NewValueUndefined(), entry.NewValues[i])
			}
		}
		if cf.IsValid() {
			*conflicts = append(*conflicts, cf)
		}
		return false, nil
	}

	// Find previously new values from theirs update (will be used as old values in rebased version).
	// A renumbered key is our own inserted row: theirs' row with the old
	// number is a different feature, so its updates don't apply.
	renumbered := mapped
	patchedVals, wasUpdated := tableInfo.updated[key]
	if !wasUpdated || renumbered {
		patchedVals = make([]changeset.Value, numColumns)
	}

	cf := newConflict(entry)
	entryHasChanges := false

	for i := 0; i < numColumns; i++ {
		patchedVal := patchedVals[i]
		if entry.Table.PrimaryKeys[i] {
			// The key identifies the row: always carried through as is.
			outEntry.OldValues[i] = entry.OldValues[i]
			outEntry.NewValues[i] = entry.NewValues[i]
			continue
		}
		if !patchedVal.IsUndefined() && !entry.NewValues[i].IsUndefined() {
			if patchedVal.Equal(entry.NewValues[i]) {
				// Both theirs and ours modified to the same value → no change
				outEntry.OldValues[i] = changeset.NewValueUndefined()
				outEntry.NewValues[i] = changeset.NewValueUndefined()
			} else {
				// Edit conflict: both modified the same column to different values.
				// Don't write either value to the changeset — flag conflict only.
				// The row stays at theirs value; caller must resolve via conflict JSON.
				outEntry.OldValues[i] = changeset.NewValueUndefined()
				outEntry.NewValues[i] = changeset.NewValueUndefined()
				addConflictItem(&cf, i, entry.OldValues[i], patchedVal, entry.NewValues[i])
			}
		} else if !patchedVal.IsUndefined() {
			// Theirs modified this column, ours didn't.
			// Don't include in rebased output — theirs is already applied.
			outEntry.OldValues[i] = changeset.NewValueUndefined()
			outEntry.NewValues[i] = changeset.NewValueUndefined()
		} else {
			// Unchanged by theirs → pass through ours
			outEntry.OldValues[i] = entry.OldValues[i]
			outEntry.NewValues[i] = entry.NewValues[i]
			if !entry.NewValues[i].IsUndefined() {
				entryHasChanges = true
			}
		}
	}

	if renumbered {
		setPK(entry, outEntry.OldValues, pm.newPK)
	}

	if cf.IsValid() {
		*conflicts = append(*conflicts, cf)
	}

	return entryHasChanges, nil
}

// ---------------------------------------------------------------------------
// Step 3 driver: produce rebased changeset file
// ---------------------------------------------------------------------------

func prepareNewChangeset(
	reader *changeset.Reader,
	output string,
	mapping *rebaseMapping,
	dbInfo *databaseRebaseInfo,
	conflicts *[]ConflictFeature,
) error {
	type tableDef struct {
		table   changeset.ChangesetTable
		entries []changeset.ChangesetEntry
	}

	tableDefinitions := make(map[string]*changeset.ChangesetTable)
	tableChanges := make(map[string][]changeset.ChangesetEntry)
	var tableOrder []string // preserve insertion order

	for {
		entry, err := reader.NextEntry()
		if err != nil {
			return fmt.Errorf("prepareNewChangeset: %w", err)
		}
		if entry == nil {
			break
		}

		tableName := entry.Table.Name

		if _, exists := tableDefinitions[tableName]; !exists {
			t := entry.Table // copy
			tableDefinitions[tableName] = &t
			tableOrder = append(tableOrder, tableName)
		}

		tableIt, inDBInfo := dbInfo.tables[tableName]
		if !inDBInfo {
			// Table not modified by theirs → copy entry as-is
			tableChanges[tableName] = append(tableChanges[tableName], *entry)
			continue
		}

		writeEntry := false
		var outEntry changeset.ChangesetEntry

		switch entry.Op {
		case changeset.OpUpdate:
			var handleErr error
			writeEntry, handleErr = handleUpdate(entry, mapping, tableIt, &outEntry, conflicts)
			if handleErr != nil {
				return fmt.Errorf("prepareNewChangeset: handleUpdate: %w", handleErr)
			}
		case changeset.OpInsert:
			var handleErr error
			writeEntry, handleErr = handleInsert(entry, mapping, tableIt, &outEntry, conflicts)
			if handleErr != nil {
				return fmt.Errorf("prepareNewChangeset: handleInsert: %w", handleErr)
			}
		case changeset.OpDelete:
			var handleErr error
			writeEntry, handleErr = handleDelete(entry, mapping, tableIt, &outEntry)
			if handleErr != nil {
				return fmt.Errorf("prepareNewChangeset: handleDelete: %w", handleErr)
			}
		}

		if writeEntry {
			outEntry.Table = entry.Table
			tableChanges[tableName] = append(tableChanges[tableName], outEntry)
		}
	}

	writer, err := changeset.NewWriter(output)
	if err != nil {
		return fmt.Errorf("prepareNewChangeset: create writer: %w", err)
	}
	defer writer.Close()

	for _, tableName := range tableOrder {
		changes, ok := tableChanges[tableName]
		if !ok || len(changes) == 0 {
			continue
		}

		def := tableDefinitions[tableName]
		if err := writer.BeginTable(*def); err != nil {
			return fmt.Errorf("prepareNewChangeset: beginTable %s: %w", tableName, err)
		}
		for i := range changes {
			if err := writer.WriteEntry(changes[i]); err != nil {
				return fmt.Errorf("prepareNewChangeset: writeEntry: %w", err)
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Rebase — produce THEIRS→MERGED changeset
// ---------------------------------------------------------------------------

// Rebase takes BASE→THEIRS and BASE→OURS changeset files (both on disk)
// and produces a THEIRS→MERGED changeset. Returns any conflicts found.
func Rebase(
	baseTheirs string, // path to BASE→THEIRS changeset
	baseOurs string, // path to BASE→OURS changeset
	theirsMerged string, // path to write THEIRS→MERGED changeset
) ([]ConflictFeature, error) {
	// Remove output file
	os.Remove(theirsMerged)

	// Open BASE→THEIRS
	readerTheirs, err := changeset.NewReader(baseTheirs)
	if err != nil {
		return nil, fmt.Errorf("Rebase: open baseTheirs: %w", err)
	}
	defer readerTheirs.Close()

	if readerTheirs.IsEmpty() {
		// No theirs changes → ours is already the rebased version
		if err := copyFileContents(baseOurs, theirsMerged); err != nil {
			return nil, fmt.Errorf("Rebase: copy baseOurs to theirsMerged: %w", err)
		}
		return nil, nil
	}

	// Open BASE→OURS
	readerOurs, err := changeset.NewReader(baseOurs)
	if err != nil {
		return nil, fmt.Errorf("Rebase: open baseOurs: %w", err)
	}
	defer readerOurs.Close()

	if readerOurs.IsEmpty() {
		// No ours changes → theirs is the rebased version
		if err := copyFileContents(baseTheirs, theirsMerged); err != nil {
			return nil, fmt.Errorf("Rebase: copy baseTheirs to theirsMerged: %w", err)
		}
		return nil, nil
	}

	// Step 1: Parse theirs changeset
	dbInfo := &databaseRebaseInfo{
		tables: make(map[string]*tableRebaseInfo),
	}
	if err := parseOldChangeset(readerTheirs, dbInfo); err != nil {
		return nil, fmt.Errorf("Rebase: parse theirs: %w", err)
	}

	// Step 2: Build mapping from ours changeset
	mapping := newRebaseMapping()
	freeIndices := make(map[string]int64)
	if err := findMappingForNewChangeset(readerOurs, dbInfo, mapping, freeIndices); err != nil {
		return nil, fmt.Errorf("Rebase: find mapping: %w", err)
	}

	// Rewind ours reader for step 3
	readerOurs.Rewind()

	// Step 3: Produce rebased changeset
	var conflicts []ConflictFeature
	if err := prepareNewChangeset(readerOurs, theirsMerged, mapping, dbInfo, &conflicts); err != nil {
		return nil, fmt.Errorf("Rebase: prepare changeset: %w", err)
	}

	return conflicts, nil
}

// ---------------------------------------------------------------------------
// Invert changeset
// ---------------------------------------------------------------------------

// InvertChangeset inverts a changeset: INSERT↔DELETE, UPDATE rows are swapped.
// Writes the result to outputPath.
func InvertChangeset(inputPath, outputPath string) error {
	reader, err := changeset.NewReader(inputPath)
	if err != nil {
		return fmt.Errorf("InvertChangeset: %w", err)
	}
	defer reader.Close()
	writer, err := changeset.NewWriter(outputPath)
	if err != nil {
		return fmt.Errorf("InvertChangeset: %w", err)
	}
	defer writer.Close()
	return changeset.InvertChangeset(reader, writer)
}

// ---------------------------------------------------------------------------
// RebaseDirect — in-place rebase of a GPKG file
// ---------------------------------------------------------------------------

// RebaseDirect rebases the OURS GPKG directly (in-place modification).
//
//	base    — path to original BASE GPKG file
//	theirs  — path to THEIRS GPKG file (remote version)
//	ours    — path to OURS GPKG file (local copy, modified in place)
//	conflictFile — optional C++-compat sidecar (written only if conflicts exist)
//
// Returns typed conflicts. The changeset blob is unchanged.
func RebaseDirect(
	base string,
	theirs string,
	ours string,
	conflictFile string,
) ([]ConflictFeature, error) {
	// --- Step 0: create temp directory for intermediate files ---
	tmpDir, err := os.MkdirTemp("", "go-geodiff-rebase-")
	if err != nil {
		return nil, fmt.Errorf("RebaseDirect: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	baseOurs := tmpDir + "/base2ours.bin"
	oursBase := tmpDir + "/ours2base.bin"
	baseTheirs := tmpDir + "/base2theirs.bin"
	theirsMerged := tmpDir + "/theirs2merged.bin"

	// --- Step 1: Undo local changes (OURS → BASE) ---

	// 1a. Create BASE→OURS changeset
	d1 := NewSqliteDriver()
	if err := d1.Open(context.Background(), ConnInfo{Base: base, Modified: ours}); err != nil {
		return nil, fmt.Errorf("RebaseDirect: open base→ours: %w", err)
	}
	w1, err := changeset.NewWriter(baseOurs)
	if err != nil {
		d1.Close()
		return nil, fmt.Errorf("RebaseDirect: create base2ours writer: %w", err)
	}
	if err := d1.CreateChangeset(context.Background(), w1); err != nil {
		w1.Close()
		d1.Close()
		return nil, fmt.Errorf("RebaseDirect: createChangeset base→ours: %w", err)
	}
	w1.Close()
	d1.Close()

	// If ours has no changes, skip the undo+theirs apply
	oursHasChanges := !fileBytesEmpty(baseOurs)

	if oursHasChanges {
		// 1b. Invert BASE→OURS to OURS→BASE
		if err := InvertChangeset(baseOurs, oursBase); err != nil {
			return nil, fmt.Errorf("RebaseDirect: invert base→ours: %w", err)
		}

		// 1c. Apply OURS→BASE to OURS (undo local changes)
		rInv, err := changeset.NewReader(oursBase)
		if err != nil {
			return nil, fmt.Errorf("RebaseDirect: open inverted changeset: %w", err)
		}
		dApply := NewSqliteDriver()
		if err := dApply.Open(context.Background(), ConnInfo{Base: ours}); err != nil {
			rInv.Close()
			return nil, fmt.Errorf("RebaseDirect: open ours for undo: %w", err)
		}
		if err := dApply.ApplyChangeset(context.Background(), rInv); err != nil {
			rInv.Close()
			dApply.Close()
			return nil, fmt.Errorf("RebaseDirect: apply undo changeset: %w", err)
		}
		rInv.Close()
		dApply.Close()
	}

	// --- Step 2: Apply theirs changes (BASE → THEIRS) ---

	// 2a. Create BASE→THEIRS changeset
	d2 := NewSqliteDriver()
	if err := d2.Open(context.Background(), ConnInfo{Base: base, Modified: theirs}); err != nil {
		return nil, fmt.Errorf("RebaseDirect: open base→theirs: %w", err)
	}
	w2, err := changeset.NewWriter(baseTheirs)
	if err != nil {
		d2.Close()
		return nil, fmt.Errorf("RebaseDirect: create base2theirs writer: %w", err)
	}
	if err := d2.CreateChangeset(context.Background(), w2); err != nil {
		w2.Close()
		d2.Close()
		return nil, fmt.Errorf("RebaseDirect: createChangeset base→theirs: %w", err)
	}
	w2.Close()
	d2.Close()

	theirsHasChanges := !fileBytesEmpty(baseTheirs)

	if theirsHasChanges {
		// 2b. Apply BASE→THEIRS to OURS
		rTheirs, err := changeset.NewReader(baseTheirs)
		if err != nil {
			return nil, fmt.Errorf("RebaseDirect: open theirs changeset: %w", err)
		}
		dApply2 := NewSqliteDriver()
		if err := dApply2.Open(context.Background(), ConnInfo{Base: ours}); err != nil {
			rTheirs.Close()
			return nil, fmt.Errorf("RebaseDirect: open ours for theirs apply: %w", err)
		}
		if err := dApply2.ApplyChangeset(context.Background(), rTheirs); err != nil {
			rTheirs.Close()
			dApply2.Close()
			return nil, fmt.Errorf("RebaseDirect: apply theirs changeset: %w", err)
		}
		rTheirs.Close()
		dApply2.Close()
	}

	// --- Step 3: Rebase local changes on top ---

	var conflicts []ConflictFeature
	if oursHasChanges {
		// 3a. Create THEIRS→MERGED (rebased) changeset
		var rebaseErr error
		conflicts, rebaseErr = Rebase(baseTheirs, baseOurs, theirsMerged)
		if rebaseErr != nil {
			return nil, fmt.Errorf("RebaseDirect: rebase: %w", rebaseErr)
		}

		// 3b. Apply THEIRS→MERGED to OURS
		if !fileBytesEmpty(theirsMerged) {
			rMerged, err := changeset.NewReader(theirsMerged)
			if err != nil {
				return nil, fmt.Errorf("RebaseDirect: open merged changeset: %w", err)
			}
			dApply3 := NewSqliteDriver()
			if err := dApply3.Open(context.Background(), ConnInfo{Base: ours}); err != nil {
				rMerged.Close()
				return nil, fmt.Errorf("RebaseDirect: open ours for merged apply: %w", err)
			}
			if err := dApply3.ApplyChangeset(context.Background(), rMerged); err != nil {
				rMerged.Close()
				dApply3.Close()
				return nil, fmt.Errorf("RebaseDirect: apply merged changeset: %w", err)
			}
			rMerged.Close()
			dApply3.Close()
		}

		// Optional C++-compat sidecar — typed conflicts are the API.
		if conflictFile != "" && len(conflicts) > 0 {
			if err := writeConflictFile(conflictFile, conflicts); err != nil {
				return nil, fmt.Errorf("RebaseDirect: write conflicts: %w", err)
			}
		}
	}

	return conflicts, nil
}

// ---------------------------------------------------------------------------
// Conflict JSON serialization
// ---------------------------------------------------------------------------

// conflictItemJSON is the JSON-serializable form of a ConflictItem.
type conflictItemJSON struct {
	Column int `json:"column"`
	Base   any `json:"base,omitempty"`
	Old    any `json:"old,omitempty"`
	New    any `json:"new,omitempty"`
}

// conflictFeatureJSON is the JSON-serializable form of a ConflictFeature.
type conflictFeatureJSON struct {
	Table   string             `json:"table"`
	Type    string             `json:"type"`
	FID     string             `json:"fid"`
	Changes []conflictItemJSON `json:"changes"`
}

// conflictsJSON is the top-level JSON output.
type conflictsJSON struct {
	Geodiff []conflictFeatureJSON `json:"geodiff"`
}

// valueToJSON converts a changeset.Value to a JSON-compatible any value.
func valueToJSON(v changeset.Value) any {
	switch v.Type() {
	case changeset.TypeUndefined:
		return nil
	case changeset.TypeInt:
		n, _ := v.AsInt()
		return n
	case changeset.TypeDouble:
		f, _ := v.AsDouble()
		return f
	case changeset.TypeText:
		s, _ := v.AsText()
		return s
	case changeset.TypeBlob:
		// Base64-encode blob data
		b, _ := v.AsBlob()
		return base64.StdEncoding.EncodeToString(b)
	case changeset.TypeNull:
		return nil
	default:
		return nil
	}
}

// MarshalConflicts serializes rebase conflicts to the geodiff JSON object
// ({"geodiff":[...]}). Empty or invalid conflicts yield an empty object.
func MarshalConflicts(conflicts []ConflictFeature) ([]byte, error) {
	var entries []conflictFeatureJSON
	for _, cf := range conflicts {
		if !cf.IsValid() {
			continue
		}
		fj := conflictFeatureJSON{
			Table: cf.TableName,
			Type:  "conflict",
			FID:   cf.FID(),
		}
		for _, item := range cf.Items {
			cij := conflictItemJSON{Column: item.Column}
			if v := valueToJSON(item.Base); v != nil {
				cij.Base = v
			}
			if v := valueToJSON(item.Theirs); v != nil {
				cij.Old = v
			}
			if v := valueToJSON(item.Ours); v != nil {
				cij.New = v
			}
			fj.Changes = append(fj.Changes, cij)
		}
		entries = append(entries, fj)
	}
	if len(entries) == 0 {
		return []byte(`{"geodiff":[]}`), nil
	}
	return json.Marshal(conflictsJSON{Geodiff: entries})
}

// writeConflictFile writes C++-compat sidecar JSON. Empty path is a no-op —
// typed []ConflictFeature is the API.
func writeConflictFile(path string, conflicts []ConflictFeature) error {
	if path == "" {
		return nil
	}
	data, err := MarshalConflicts(conflicts)
	if err != nil {
		return fmt.Errorf("writeConflictFile: marshal: %w", err)
	}
	if string(data) == `{"geodiff":[]}` {
		return nil
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}
