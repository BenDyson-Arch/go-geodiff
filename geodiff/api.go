/*
 GEODIFF - MIT License
 Copyright (C) 2019 Peter Petrik

 Go port of geodiff.h + geodiff.cpp — the public geodiff API.
*/

package geodiff

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/BenDyson-Arch/go-geodiff/changeset"
	"github.com/BenDyson-Arch/go-geodiff/driver"
	"github.com/BenDyson-Arch/go-geodiff/schema"
)

// Version returns the library version string.
func Version() string {
	return "0.3.1"
}

// ---------------------------------------------------------------------------
// CreateChangeset
// ---------------------------------------------------------------------------

// CreateChangeset creates a binary diff between base and modified GPKG/SQLite files
// and writes it to changeset.
func CreateChangeset(base, modified, changesetPath string) error {
	gctx := NewContext()
	return createChangesetEx(gctx, "sqlite", nil, base, modified, changesetPath)
}

func createChangesetEx(gctx *Context, driverName string, driverExtraInfo map[string]string, base, modified, changesetPath string) error {
	if driverName == "" || base == "" || modified == "" || changesetPath == "" {
		return NewGeoDiffError("NULL arguments to createChangesetEx")
	}

	drv := newDriver(gctx, driverName)
	conn := driver.ConnInfo{
		Base:     base,
		Modified: modified,
	}

	_ = driverExtraInfo // reserved for future use
	if err := drv.Open(context.Background(), conn); err != nil {
		return wrapDriverError(gctx, "Unable to open databases for createChangeset", err)
	}
	defer drv.Close()

	w, err := changeset.NewWriter(changesetPath)
	if err != nil {
		return wrapError(gctx, "Unable to create changeset file", err)
	}
	defer w.Close()

	if err := drv.CreateChangeset(context.Background(), w); err != nil {
		return wrapError(gctx, "Failed to create changeset", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ApplyChangeset
// ---------------------------------------------------------------------------

// ApplyChangeset applies a changeset to a base GPKG/SQLite file (in-place).
// Returns nil on success. If conflicts were found, returns *GeoDiffError with Code=Conflicts.
func ApplyChangeset(base, changesetPath string) error {
	ctx := NewContext()
	return applyChangesetEx(ctx, "sqlite", nil, base, changesetPath)
}

func applyChangesetEx(ctx *Context, driverName string, driverExtraInfo map[string]string, base, changesetPath string) error {
	if driverName == "" || base == "" || changesetPath == "" {
		return NewGeoDiffError("NULL arguments to applyChangesetEx")
	}

	drv := newDriver(ctx, driverName)
	conn := driver.ConnInfo{Base: base}
	_ = driverExtraInfo // reserved for future use
	if err := drv.Open(context.Background(), conn); err != nil {
		return wrapDriverError(ctx, "Unable to open database for applyChangeset", err)
	}
	defer drv.Close()

	reader, err := changeset.NewReader(changesetPath)
	if err != nil {
		return wrapError(ctx, "Unable to open changeset file for reading", err)
	}
	defer reader.Close()

	if reader.IsEmpty() {
		ctx.Logger().Debug("--- no changes ---")
		return nil
	}

	err = drv.ApplyChangeset(context.Background(), reader)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, ErrConflict) || strings.Contains(msg, "constraint") {
			return &GeoDiffError{Code: Conflicts, Msg: msg}
		}
		return wrapError(ctx, "Failed to apply changeset", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// InvertChangeset
// ---------------------------------------------------------------------------

// InvertChangeset creates an inverse changeset (applying changesetInv to MODIFIED produces BASE).
func InvertChangeset(changesetPath, changesetInv string) error {
	ctx := NewContext()
	return invertChangesetByPath(ctx, changesetPath, changesetInv)
}

func invertChangesetByPath(ctx *Context, changesetPath, changesetInv string) error {
	if changesetPath == "" {
		return NewGeoDiffError("NULL arguments to invertChangeset")
	}
	if !FileExists(changesetPath) {
		return NewGeoDiffError("Missing input file in invertChangeset: " + changesetPath)
	}

	reader, err := changeset.NewReader(changesetPath)
	if err != nil {
		return wrapError(ctx, "Could not open changeset", err)
	}
	defer reader.Close()

	writer, err := changeset.NewWriter(changesetInv)
	if err != nil {
		return wrapError(ctx, "Could not create inverse changeset file", err)
	}
	defer writer.Close()

	// Delegate to the canonical implementation in the changeset package.
	return changeset.InvertChangeset(reader, writer)
}

// ---------------------------------------------------------------------------
// HasChanges / ChangesCount
// ---------------------------------------------------------------------------

// HasChanges returns true if the changeset contains any changes.
func HasChanges(changesetPath string) (bool, error) {
	reader, err := changeset.NewReader(changesetPath)
	if err != nil {
		return false, fmt.Errorf("could not open changeset: %s: %w", changesetPath, err)
	}
	defer reader.Close()
	return !reader.IsEmpty(), nil
}

// ChangesCount returns the number of changes in a changeset, or -1 on error.
func ChangesCount(changesetPath string) (int, error) {
	reader, err := changeset.NewReader(changesetPath)
	if err != nil {
		return -1, fmt.Errorf("could not open changeset: %s: %w", changesetPath, err)
	}
	defer reader.Close()

	count := 0
	for {
		entry, err := reader.NextEntry()
		if err != nil {
			return -1, fmt.Errorf("error reading changeset: %w", err)
		}
		if entry == nil {
			break
		}
		count++
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// ListChanges / ListChangesSummary
// ---------------------------------------------------------------------------

// ListChanges exports a changeset to a JSON file with full detail.
func ListChanges(changesetPath, jsonfile string) error {
	return listChangesJSON(changesetPath, jsonfile, false)
}

// ListChangesSummary exports a changeset summary to a JSON file.
func ListChangesSummary(changesetPath, jsonfile string) error {
	return listChangesJSON(changesetPath, jsonfile, true)
}

func listChangesJSON(changesetPath, jsonfile string, onlySummary bool) error {
	reader, err := changeset.NewReader(changesetPath)
	if err != nil {
		return fmt.Errorf("could not open changeset: %s: %w", changesetPath, err)
	}
	defer reader.Close()

	var res []byte
	if onlySummary {
		j, err := changesetToJSONSummary(reader)
		if err != nil {
			return fmt.Errorf("failed to convert changeset to summary JSON: %w", err)
		}
		res = j
	} else {
		j, err := changesetToJSON(reader)
		if err != nil {
			return fmt.Errorf("failed to convert changeset to JSON: %w", err)
		}
		res = j
	}

	if jsonfile == "" {
		return NewGeoDiffError("output file is required for listChanges")
	}
	return FlushString(jsonfile, string(res))
}

// jsonChange represents a single column change in JSON output.
type jsonChange struct {
	Column int             `json:"column"`
	Name   string          `json:"name,omitempty"`
	Old    json.RawMessage `json:"old,omitempty"`
	New    json.RawMessage `json:"new,omitempty"`
}

// jsonEntry is a single changeset entry in JSON output.
type jsonEntry struct {
	Table   string       `json:"table"`
	Type    string       `json:"type"`
	Changes []jsonChange `json:"changes"`
}

// jsonSummary is the summary JSON structure.
type jsonSummary struct {
	Table  string         `json:"table"`
	Insert int            `json:"insert"`
	Update int            `json:"update"`
	Delete int            `json:"delete"`
	Types  map[string]int `json:"types,omitempty"`
}

// valueToJSON converts a changeset.Value to a JSON-marshallable representation.
func valueToJSON(v changeset.Value) json.RawMessage {
	switch v.Type() {
	case changeset.TypeUndefined:
		return json.RawMessage(`null`)
	case changeset.TypeNull:
		return json.RawMessage(`null`)
	case changeset.TypeInt:
		n, _ := v.AsInt()
		b, _ := json.Marshal(n)
		return b
	case changeset.TypeDouble:
		f, _ := v.AsDouble()
		b, _ := json.Marshal(f)
		return b
	case changeset.TypeText:
		s, _ := v.AsText()
		b, _ := json.Marshal(s)
		return b
	case changeset.TypeBlob:
		blb, _ := v.AsBlob()
		b, _ := json.Marshal(base64.StdEncoding.EncodeToString(blb))
		return b
	default:
		return json.RawMessage(`null`)
	}
}

// valueToJSON converts a changeset.Value to a JSON-marshallable representation.
// changesetToJSON reads all entries and produces a full JSON representation.
func changesetToJSON(reader *changeset.Reader) ([]byte, error) {
	var entries []jsonEntry
	for {
		entry, err := reader.NextEntry()
		if err != nil {
			return nil, err
		}
		if entry == nil {
			break
		}

		je := jsonEntry{
			Table: entry.Table.Name,
			Type:  entry.Op.String(),
		}

		switch entry.Op {
		case changeset.OpInsert:
			for i, v := range entry.NewValues {
				if v.Type() == changeset.TypeUndefined {
					continue
				}
				je.Changes = append(je.Changes, jsonChange{
					Column: i,
					New:    valueToJSON(v),
				})
			}
		case changeset.OpDelete:
			for i, v := range entry.OldValues {
				if v.Type() == changeset.TypeUndefined {
					continue
				}
				je.Changes = append(je.Changes, jsonChange{
					Column: i,
					Old:    valueToJSON(v),
				})
			}
		case changeset.OpUpdate:
			// Like C++ geodiff, list every column with an old or new value:
			// primary keys carry only an old value, and without them an
			// update does not say which row it changed.
			for i := range entry.NewValues {
				oldV := entry.OldValues[i]
				newV := entry.NewValues[i]
				if newV.Type() == changeset.TypeUndefined && oldV.Type() == changeset.TypeUndefined {
					continue
				}
				ch := jsonChange{Column: i}
				if newV.Type() != changeset.TypeUndefined {
					ch.New = valueToJSON(newV)
				}
				if oldV.Type() != changeset.TypeUndefined {
					ch.Old = valueToJSON(oldV)
				}
				je.Changes = append(je.Changes, ch)
			}
		}
		entries = append(entries, je)
	}

	// Wrap in {"geodiff": [...]} like the C++ version.
	output := map[string]any{
		"geodiff": entries,
	}
	return json.MarshalIndent(output, "", "  ")
}

// changesetToJSONSummary reads all entries and produces a summary JSON.
func changesetToJSONSummary(reader *changeset.Reader) ([]byte, error) {
	type tableStats struct {
		insert int
		update int
		delete int
	}
	summaries := make(map[string]*tableStats)
	var tableOrder []string

	for {
		entry, err := reader.NextEntry()
		if err != nil {
			return nil, err
		}
		if entry == nil {
			break
		}

		name := entry.Table.Name
		if _, ok := summaries[name]; !ok {
			summaries[name] = &tableStats{}
			tableOrder = append(tableOrder, name)
		}
		switch entry.Op {
		case changeset.OpInsert:
			summaries[name].insert++
		case changeset.OpUpdate:
			summaries[name].update++
		case changeset.OpDelete:
			summaries[name].delete++
		}
	}

	var out []jsonSummary
	for _, name := range tableOrder {
		s := summaries[name]
		out = append(out, jsonSummary{
			Table:  name,
			Insert: s.insert,
			Update: s.update,
			Delete: s.delete,
		})
	}

	output := map[string]any{
		"geodiff_summary": out,
	}
	return json.MarshalIndent(output, "", "  ")
}

// ---------------------------------------------------------------------------
// ConcatChanges
// ---------------------------------------------------------------------------

// ConcatChanges combines multiple changeset files into one.
func ConcatChanges(inputFiles []string, outputFile string) error {
	ctx := NewContext()

	if len(inputFiles) < 2 {
		return NewGeoDiffError("Need at least two input changesets in concatChanges")
	}
	for _, f := range inputFiles {
		if !FileExists(f) {
			return NewGeoDiffError("Input file in concatChanges does not exist: " + f)
		}
	}

	writer, err := changeset.NewWriter(outputFile)
	if err != nil {
		return wrapError(ctx, "Could not create output changeset", err)
	}
	defer writer.Close()

	lastTableName := ""
	for _, inputFile := range inputFiles {
		reader, err := changeset.NewReader(inputFile)
		if err != nil {
			return wrapError(ctx, "Could not open input changeset: "+inputFile, err)
		}

		for {
			entry, err := reader.NextEntry()
			if err != nil {
				reader.Close()
				return wrapError(ctx, "Error reading changeset: "+inputFile, err)
			}
			if entry == nil {
				break
			}

			tableName := entry.Table.Name
			if tableName != lastTableName {
				if err := writer.BeginTable(entry.Table); err != nil {
					reader.Close()
					return wrapError(ctx, "Error writing table header in concat", err)
				}
				lastTableName = tableName
			}
			if err := writer.WriteEntry(*entry); err != nil {
				reader.Close()
				return wrapError(ctx, "Error writing entry in concat", err)
			}
		}
		reader.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// MakeCopy / MakeCopySqlite
// ---------------------------------------------------------------------------

// MakeCopy copies a dataset from src to dst driver.
// Currently only supports the sqlite driver.
func MakeCopy(driverSrc, extraInfo, src, driverDst, extraInfoDst, dst string) error {
	ctx := NewContext()

	if driverSrc == "" || driverDst == "" || src == "" || dst == "" {
		return NewGeoDiffError("NULL arguments to makeCopy")
	}

	// If both sides are sqlite, use the backup API.
	if driverSrc == "sqlite" && driverDst == "sqlite" {
		return MakeCopySqlite(src, dst)
	}

	// Cross-driver copy: dump src to changeset, create dst, apply.
	srcDriver := newDriver(ctx, driverSrc)
	srcConn := driver.ConnInfo{Base: src}
	_ = extraInfo // reserved for future use
	if err := srcDriver.Open(context.Background(), srcConn); err != nil {
		return wrapDriverError(ctx, "Cannot open source database", err)
	}
	defer srcDriver.Close()

	tableNames, err := srcDriver.ListTables(context.Background(), driver.BaseSide)
	if err != nil {
		return wrapDriverError(ctx, "Failed to list source tables", err)
	}
	tableNames = filterTableNames(ctx, tableNames)

	var tables []*schema.TableSchema
	for _, name := range tableNames {
		tbl, err := srcDriver.TableSchema(context.Background(), name, driver.BaseSide)
		if err != nil {
			return wrapDriverError(ctx, "Failed to read table schema: "+name, err)
		}
		tables = append(tables, tbl)
	}

	tmpChangeset := RandomTmpFilename()
	defer os.Remove(tmpChangeset)

	w, err := changeset.NewWriter(tmpChangeset)
	if err != nil {
		return wrapError(ctx, "Failed to create temporary changeset", err)
	}
	if err := srcDriver.DumpData(context.Background(), w, driver.BaseSide); err != nil {
		w.Close()
		return wrapDriverError(ctx, "Failed to dump source data", err)
	}
	w.Close()

	dstDriver := newDriver(ctx, driverDst)
	dstConn := driver.ConnInfo{Base: dst}
	_ = extraInfoDst // reserved for future use
	if err := dstDriver.Create(context.Background(), dstConn, true); err != nil {
		return wrapDriverError(ctx, "Cannot create destination database", err)
	}
	defer dstDriver.Close()

	if err := dstDriver.CreateTables(context.Background(), tables); err != nil {
		return wrapDriverError(ctx, "Failed to create tables in destination", err)
	}

	reader, err := changeset.NewReader(tmpChangeset)
	if err != nil {
		return wrapError(ctx, "Failed to open temporary changeset", err)
	}
	defer reader.Close()

	if err := dstDriver.ApplyChangeset(context.Background(), reader); err != nil {
		return wrapDriverError(ctx, "Failed to apply data to destination", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// DumpData
// ---------------------------------------------------------------------------

// DumpData dumps all data from a source as INSERT statements in changeset format.
func DumpData(driverName, extraInfo, src, changesetPath string) error {
	ctx := NewContext()

	if driverName == "" || src == "" || changesetPath == "" {
		return NewGeoDiffError("NULL arguments to dumpData")
	}

	drv := newDriver(ctx, driverName)
	conn := driver.ConnInfo{Base: src}
	_ = extraInfo // reserved for future use
	if err := drv.Open(context.Background(), conn); err != nil {
		return wrapDriverError(ctx, "Cannot open source database", err)
	}
	defer drv.Close()

	w, err := changeset.NewWriter(changesetPath)
	if err != nil {
		return wrapError(ctx, "Failed to create changeset file", err)
	}
	defer w.Close()

	if err := drv.DumpData(context.Background(), w, driver.BaseSide); err != nil {
		return wrapDriverError(ctx, "Failed to dump data", err)
	}
	return nil
}

// CreateInitialDiff creates a changeset that, when applied to a new empty database,
// produces the full state of src (schema + all data). This is the correct way to
// produce an "initial sync" diff for first-time push.
//
// Unlike a simple DumpData, this handles GPKG metadata tables properly and
// guarantees the resulting changeset can seed a fresh database from scratch.
func CreateInitialDiff(src, changesetPath string) error {
	ctx := NewContext()

	if src == "" || changesetPath == "" {
		return NewGeoDiffError("NULL arguments to createInitialDiff")
	}

	drv := newDriver(ctx, "sqlite")
	if err := drv.Open(context.Background(), driver.ConnInfo{Base: src}); err != nil {
		return wrapDriverError(ctx, "Cannot open source database", err)
	}
	defer drv.Close()

	// Collect table schemas.
	tableNames, err := drv.ListTables(context.Background(), driver.BaseSide)
	if err != nil {
		return wrapDriverError(ctx, "Failed to list tables", err)
	}

	var schemas []*schema.TableSchema
	for _, name := range tableNames {
		tbl, err := drv.TableSchema(context.Background(), name, driver.BaseSide)
		if err != nil {
			return wrapDriverError(ctx, "Failed to read schema: "+name, err)
		}
		schemas = append(schemas, tbl)
	}

	// Create an empty database with matching schema (including GPKG metadata).
	emptyDrv := driver.NewSqliteDriver()
	emptyPath := changesetPath + ".empty.gpkg"
	defer os.Remove(emptyPath)

	if err := emptyDrv.Create(context.Background(), driver.ConnInfo{Base: emptyPath}, true); err != nil {
		return wrapError(ctx, "Failed to create empty database", err)
	}
	if err := emptyDrv.CreateTables(context.Background(), schemas); err != nil {
		emptyDrv.Close()
		return wrapError(ctx, "Failed to create schema in empty database", err)
	}
	emptyDrv.Close()

	// Diff empty → canonical (this produces INSERTs for all existing rows).
	return createChangesetEx(ctx, "sqlite", nil, emptyPath, src, changesetPath)
}

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

// Schema writes the database schema as JSON to a file.
func Schema(driverName, extraInfo, src, jsonfile string) error {
	ctx := NewContext()

	if driverName == "" || src == "" || jsonfile == "" {
		return NewGeoDiffError("NULL arguments to schema")
	}

	drv := newDriver(ctx, driverName)
	conn := driver.ConnInfo{Base: src}
	_ = extraInfo // reserved for future use
	if err := drv.Open(context.Background(), conn); err != nil {
		return wrapDriverError(ctx, "Cannot open source database", err)
	}
	defer drv.Close()

	tableNames, err := drv.ListTables(context.Background(), driver.BaseSide)
	if err != nil {
		return wrapDriverError(ctx, "Failed to list tables", err)
	}

	type columnJSON struct {
		Name          string `json:"name"`
		Type          string `json:"type"`
		DBType        string `json:"type_db"`
		IsPrimaryKey  bool   `json:"primary_key,omitempty"`
		IsNotNull     bool   `json:"not_null,omitempty"`
		AutoIncrement bool   `json:"auto_increment,omitempty"`
		Geometry      *struct {
			Type  string `json:"type"`
			SrsID string `json:"srs_id"`
			HasZ  bool   `json:"has_z,omitempty"`
			HasM  bool   `json:"has_m,omitempty"`
		} `json:"geometry,omitempty"`
	}

	type tableJSON struct {
		Table   string       `json:"table"`
		Columns []columnJSON `json:"columns"`
		CRS     *struct {
			SrsID    int    `json:"srs_id"`
			AuthName string `json:"auth_name"`
			AuthCode int    `json:"auth_code"`
			WKT      string `json:"wkt"`
		} `json:"crs,omitempty"`
	}

	var tables []tableJSON
	for _, name := range tableNames {
		tbl, err := drv.TableSchema(context.Background(), name, driver.BaseSide)
		if err != nil {
			return wrapDriverError(ctx, "Failed to read schema for table: "+name, err)
		}

		tj := tableJSON{Table: name}
		for _, col := range tbl.Columns {
			cj := columnJSON{
				Name:          col.Name,
				Type:          schema.BaseTypeString(col.Type.BaseType),
				DBType:        col.Type.DBType,
				IsPrimaryKey:  col.IsPrimaryKey,
				IsNotNull:     col.IsNotNull,
				AutoIncrement: col.IsAutoIncrement,
			}
			if col.IsGeometry {
				cj.Geometry = &struct {
					Type  string `json:"type"`
					SrsID string `json:"srs_id"`
					HasZ  bool   `json:"has_z,omitempty"`
					HasM  bool   `json:"has_m,omitempty"`
				}{
					Type:  col.GeomType,
					SrsID: fmt.Sprintf("%d", col.GeomSrsId),
					HasZ:  col.GeomHasZ,
					HasM:  col.GeomHasM,
				}
			}
			tj.Columns = append(tj.Columns, cj)
		}
		if tbl.Crs.SrsId != 0 {
			tj.CRS = &struct {
				SrsID    int    `json:"srs_id"`
				AuthName string `json:"auth_name"`
				AuthCode int    `json:"auth_code"`
				WKT      string `json:"wkt"`
			}{
				SrsID:    tbl.Crs.SrsId,
				AuthName: tbl.Crs.AuthName,
				AuthCode: tbl.Crs.AuthCode,
				WKT:      tbl.Crs.Wkt,
			}
		}
		tables = append(tables, tj)
	}

	output := map[string]any{
		"geodiff_schema": tables,
	}
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return wrapError(ctx, "Failed to marshal schema JSON", err)
	}
	return FlushString(jsonfile, string(data))
}

// ConflictFeature is a row-level rebase conflict. The server stores these;
// the review UI paints them. They are not written into the changeset blob.
type ConflictFeature = changeset.ConflictFeature

// ConflictItem is one column inside a ConflictFeature.
type ConflictItem = changeset.ConflictItem

// ConflictsToJSON encodes typed conflicts as {"geodiff":[...]} using
// base / theirs / ours (product form, not the C++ old/new sidecar).
func ConflictsToJSON(conflicts []ConflictFeature) ([]byte, error) {
	return changeset.ConflictsToJSON(conflicts)
}

// ---------------------------------------------------------------------------
// CreateRebasedChangeset
// ---------------------------------------------------------------------------

// CreateRebasedChangeset rebases BASE→MODIFIED onto BASE→THEIRS,
// writing THEIRS→MERGED to rebased. Returns typed conflicts (empty if none).
// conflictFile is an optional C++-compat sidecar; pass "" to skip.
func CreateRebasedChangeset(base, modified, base2their, rebased, conflictFile string) ([]ConflictFeature, error) {
	ctx := NewContext()

	if conflictFile != "" {
		_ = os.Remove(conflictFile)
	}

	// Verify we can open the database.
	drv := driver.NewSqliteDriver()
	if err := drv.Open(context.Background(), driver.ConnInfo{Base: modified}); err != nil {
		drv.Close()
		return nil, wrapDriverError(ctx, "Unable to open database for rebase validation", err)
	}
	drv.Close()

	// Create BASE→MODIFIED changeset.
	base2modified := rebased + "_BASE_MODIFIED"
	defer os.Remove(base2modified)

	if err := CreateChangeset(base, modified, base2modified); err != nil {
		return nil, err
	}

	return createRebasedChangesetEx(ctx, "sqlite", nil, base, base2modified, base2their, rebased, conflictFile)
}

// RebaseChangesets rebases BASE→OURS onto BASE→THEIRS, writing THEIRS→MERGED.
// This is the only merge path when two changesets share a base but not a tip.
// Envelope (base_commit + blob) is product metadata — not this function.
func RebaseChangesets(baseTheirs, baseOurs, theirsMerged string) ([]ConflictFeature, error) {
	if baseTheirs == "" || baseOurs == "" || theirsMerged == "" {
		return nil, NewGeoDiffError("NULL arguments to rebaseChangesets")
	}
	return driver.Rebase(baseTheirs, baseOurs, theirsMerged)
}

// createRebasedChangesetEx performs the actual rebase of two changesets.
func createRebasedChangesetEx(ctx *Context, driverName string, driverExtraInfo map[string]string,
	base, base2modified, base2their, rebased, conflictFile string) ([]ConflictFeature, error) {

	if driverName == "" || base == "" || base2modified == "" || base2their == "" || rebased == "" {
		return nil, NewGeoDiffError("NULL arguments to createRebasedChangesetEx")
	}

	// Delegate to driver.Rebase which has proper value-level rebasing.
	// base2their = base→theirs (their changes)
	// base2modified = base→ours (our changes)
	// rebased = output (theirs→merged, our changes rebased on theirs)
	conflicts, err := driver.Rebase(base2their, base2modified, rebased)
	if err != nil {
		return nil, err
	}

	if len(conflicts) == 0 {
		ctx.Logger().Debug("No conflicts present")
	} else if conflictFile != "" {
		data, _ := json.MarshalIndent(conflictsJSON{Geodiff: convertConflicts(conflicts)}, "", "  ")
		_ = FlushString(conflictFile, string(data))
	}
	return conflicts, nil
}

// convertConflicts converts ConflictFeature to the C++ sidecar shape (old/new).
func convertConflicts(src []ConflictFeature) []conflictFeature {
	result := make([]conflictFeature, 0, len(src))
	for _, dcf := range src {
		if !dcf.IsValid() {
			continue
		}
		cf := conflictFeature{
			Table: dcf.TableName,
			Type:  "conflict",
			FID:   dcf.FID(),
		}
		for _, dci := range dcf.Items {
			cf.Changes = append(cf.Changes, conflictItem{
				Column: dci.Column,
				Base:   valueToJSON(dci.Base),
				Old:    valueToJSON(dci.Theirs),
				New:    valueToJSON(dci.Ours),
			})
		}
		result = append(result, cf)
	}
	return result
}

// ---------------------------------------------------------------------------
// Rebase
// ---------------------------------------------------------------------------

// MarshalConflicts is the C++ sidecar JSON form (old=theirs, new=ours).
func MarshalConflicts(conflicts []ConflictFeature) ([]byte, error) {
	return driver.MarshalConflicts(conflicts)
}

// Rebase rebases local modifications on top of remote changes.
// base: original, modifiedTheir: remote version, modified: local copy (modified in place).
// Returns typed conflicts. conflictFile is an optional C++-compat sidecar; pass "" to skip.
func Rebase(base, modifiedTheir, modified, conflictFile string) ([]ConflictFeature, error) {
	ctx := NewContext()

	if base == "" || modifiedTheir == "" || modified == "" {
		return nil, NewGeoDiffError("NULL arguments to rebase")
	}
	if !FileExists(base) {
		return nil, NewGeoDiffError("Missing 'base' file in rebase: " + base)
	}
	if !FileExists(modifiedTheir) {
		return nil, NewGeoDiffError("Missing 'modified_their' file in rebase: " + modifiedTheir)
	}
	if !FileExists(modified) {
		return nil, NewGeoDiffError("Missing 'modified' file in rebase: " + modified)
	}

	base2theirs := modified + "_base2theirs.bin"
	defer os.Remove(base2theirs)

	if err := createChangesetEx(ctx, "sqlite", nil, base, modifiedTheir, base2theirs); err != nil {
		return nil, NewGeoDiffError("Unable to perform createChangeset base2theirs: " + err.Error())
	}

	return rebaseEx(ctx, "sqlite", nil, base, modified, base2theirs, conflictFile)
}

// rebaseEx performs the full rebase operation.
func rebaseEx(ctx *Context, driverName string, driverExtraInfo map[string]string,
	base, modified, base2their, conflictFile string) ([]ConflictFeature, error) {

	if base == "" || modified == "" || base2their == "" {
		return nil, NewGeoDiffError("NULL arguments to rebaseEx")
	}

	root := filepath.Join(TmpDir(), "geodiff_"+RandomString(6))

	// Situation 1: base2theirs has no changes, nothing to do.
	hasTheir, err := HasChanges(base2their)
	if err != nil {
		return nil, NewGeoDiffError("Failed to check base2their changes: " + err.Error())
	}
	if !hasTheir {
		return nil, nil
	}

	// Situation 2: base2modified has no changes → just apply base2theirs.
	base2modified := root + "_base2modified.bin"
	defer os.Remove(base2modified)

	if err := createChangesetEx(ctx, driverName, driverExtraInfo, base, modified, base2modified); err != nil {
		return nil, NewGeoDiffError("Unable to perform createChangeset base2modified: " + err.Error())
	}

	hasOurs, err := HasChanges(base2modified)
	if err != nil {
		return nil, NewGeoDiffError("Failed to check base2modified changes: " + err.Error())
	}
	if !hasOurs {
		// modified == base, so just apply their changes.
		if err := applyChangesetEx(ctx, driverName, driverExtraInfo, modified, base2their); err != nil {
			return nil, NewGeoDiffError("Unable to perform applyChangeset base2theirs: " + err.Error())
		}
		return nil, nil
	}

	// Situation 3: both sides have changes.
	// Undo our changes → apply theirs → apply rebased.
	theirs2final := root + "_theirs2final.bin"
	defer os.Remove(theirs2final)

	conflicts, err := createRebasedChangesetEx(ctx, driverName, driverExtraInfo, base,
		base2modified, base2their, theirs2final, conflictFile)
	if err != nil {
		return nil, NewGeoDiffError("Unable to perform createRebasedChangeset theirs2final: " + err.Error())
	}

	// Undo our local changes: invert base→modified → modified→base.
	modified2base := root + "_modified2base.bin"
	defer os.Remove(modified2base)

	if err := invertChangesetByPath(ctx, base2modified, modified2base); err != nil {
		return nil, NewGeoDiffError("Unable to perform invertChangeset modified2base: " + err.Error())
	}

	// Concat: modified→base + base→their + their→final → modified→final.
	modified2final := root + "_modified2final.bin"
	defer os.Remove(modified2final)

	if err := ConcatChanges([]string{modified2base, base2their, theirs2final}, modified2final); err != nil {
		return nil, NewGeoDiffError("Unable to concat changesets: " + err.Error())
	}

	// Apply.
	if err := applyChangesetEx(ctx, driverName, driverExtraInfo, modified, modified2final); err != nil {
		return nil, NewGeoDiffError("Unable to perform applyChangeset modified2final: " + err.Error())
	}

	return conflicts, nil
}

// ---------------------------------------------------------------------------
// Rebase logic (changeset-level)
// ---------------------------------------------------------------------------

// conflictFeature represents a conflict on a single feature row (C++-compatible format).
type conflictFeature struct {
	Table   string         `json:"table"`
	Type    string         `json:"type"`
	FID     string         `json:"fid"`
	Changes []conflictItem `json:"changes"`
}

type conflictItem struct {
	Column int             `json:"column"`
	Base   json.RawMessage `json:"base,omitempty"`
	Old    json.RawMessage `json:"old,omitempty"`
	New    json.RawMessage `json:"new,omitempty"`
}

type conflictsJSON struct {
	Geodiff []conflictFeature `json:"geodiff"`
}

// ---------------------------------------------------------------------------
// CreateWkbFromGpkgHeader
// ---------------------------------------------------------------------------

// CreateWkbFromGpkgHeader strips the GeoPackage binary header from a GPKG WKB blob,
// returning the raw WKB geometry bytes.
func CreateWkbFromGpkgHeader(gpkgWkb []byte) ([]byte, error) {
	if len(gpkgWkb) < 8 {
		return nil, NewGeoDiffError("GPKG WKB too short, expected at least 8 bytes")
	}

	// Validate magic bytes.
	if gpkgWkb[0] != 'G' || gpkgWkb[1] != 'P' {
		return nil, NewGeoDiffError("Invalid GPKG WKB header: missing 'GP' magic bytes")
	}

	// Byte 3: flags.
	flags := gpkgWkb[3]
	envelopeIndicator := (flags >> 1) & 0x07 // bits 1-3

	var headerSize int
	switch envelopeIndicator {
	case 0:
		headerSize = 8 // No envelope
	case 1:
		headerSize = 40 // minX, maxX, minY, maxY (4×8 = 32) + 8
	case 2:
		headerSize = 56 // + minZ, maxZ (2×8 = 16) → total 48+8=56
	case 3:
		headerSize = 72 // + minZ, maxZ, minM, maxM (4×8 = 32) → total 64+8=72
	case 4:
		headerSize = 56 // minX, maxX, minY, maxY, minM, maxM (4×8 + 2×8?) — wait, let me re-check.
		// Actually per the spec: envelope 4 is minX, maxX, minY, maxY, minM, maxM = 6 doubles = 48 bytes + 8 = 56.
	default:
		return nil, NewGeoDiffError(fmt.Sprintf("Unknown GPKG envelope indicator: %d", envelopeIndicator))
	}

	if len(gpkgWkb) < headerSize {
		return nil, NewGeoDiffError(fmt.Sprintf("GPKG WKB too short for header size %d: only %d bytes", headerSize, len(gpkgWkb)))
	}

	return gpkgWkb[headerSize:], nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// newDriver creates a driver instance for the given driver name.
// Currently only "sqlite" is supported.
func newDriver(ctx *Context, driverName string) driver.Driver {
	switch driverName {
	case "sqlite":
		return driver.NewSqliteDriver()
	default:
		ctx.Logger().Error("Unknown driver: " + driverName)
		return nil
	}
}

// wrapError logs and wraps an error.
func wrapError(ctx *Context, contextMsg string, err error) error {
	if err == nil {
		return nil
	}
	msg := contextMsg + ": " + err.Error()
	return NewGeoDiffError(msg)
}

// wrapDriverError handles driver-level errors and converts them to GeoDiffError.
func wrapDriverError(ctx *Context, contextMsg string, err error) error {
	if err == nil {
		return nil
	}
	msg := contextMsg + ": " + err.Error()

	if ge, ok := err.(*GeoDiffError); ok {
		return ge
	}
	return NewGeoDiffError(msg)
}
