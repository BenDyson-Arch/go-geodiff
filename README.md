# go-geodiff

Go port of [geodiff](https://github.com/MerginMaps/geodiff) — a library for creating, applying, and merging diffs of GeoPackage and SQLite databases.

Originally by [Lutra Consulting](https://www.lutraconsulting.co.uk/) (MIT). This port preserves the binary changeset format used by the [Mergin Maps](https://merginmaps.com/) / QField ecosystem.

## Usage

```go
import "github.com/BenDyson-Arch/go-geodiff/geodiff"

// Create a binary diff between two GPKG files
err := geodiff.CreateChangeset("base.gpkg", "modified.gpkg", "changes.diff")

// Apply a diff to a GPKG (in-place)
err = geodiff.ApplyChangeset("target.gpkg", "changes.diff")

// Rebase local changes on top of remote. Conflicts are typed values
// (sidecar path is optional C++ compat).
conflicts, err := geodiff.Rebase("base.gpkg", "remote.gpkg", "local.gpkg", "")

// Two changesets that share a base: the only diverge path. No Branch type.
conflicts, err = geodiff.RebaseChangesets("base2theirs.diff", "base2ours.diff", "theirs2merged.diff")

// Export changeset as JSON
err = geodiff.ListChanges("changes.diff", "changes.json")

// Count changes
count, err := geodiff.ChangesCount("changes.diff")

// Invert a changeset
err = geodiff.InvertChangeset("changes.diff", "changes_inv.diff")

// Create a full initial diff for first-time sync (schema + all data)
err = geodiff.CreateInitialDiff("canonical.gpkg", "initial.diff")
```

## Relationship to upstream

This is a careful, function-by-function port of the C++ geodiff library (v2.3.0), targeting only the SQLite/GeoPackage driver. The Postgres driver and CLI tool are excluded.

Binary changeset files produced by go-geodiff are **byte-identical** to those produced by the C++ `geodiff` CLI — verified by cross-validation tests in `crossval/`.

Applying a changeset to a GeoPackage with a spatial index runs its `rtree_*` triggers. Like upstream (through libgpkg), go-geodiff provides the functions they call: `ST_IsEmpty`, `ST_MinX`, `ST_MaxX`, `ST_MinY` and `ST_MaxY`. `modernc.org/sqlite` registers functions for the whole process, when the first driver opens; a function of the same name your program registered before that is kept.

Rebase identifies rows by their whole primary key, whatever its type: text, blob and composite keys come through exactly as written. When both sides insert the same key, ours is renumbered only if the table has a single integer key; otherwise theirs' row stands and any value ours inserted differently is reported as a conflict. `ConflictFeature.PKValues` carries the key's values and `FID()` gives it as a string.

## What's included

| Package | Purpose |
|---------|---------|
| `geodiff` | Public API — `CreateChangeset`, `ApplyChangeset`, `Rebase` / `RebaseChangesets` (typed `ConflictFeature`), `CreateInitialDiff`, `InvertChangeset`, `ListChanges`, `MakeCopy`, `Schema`, `DumpData`, WKB header stripping |
| `driver` | `SqliteDriver` — ATTACH-based SQL diffing. `Rebase` — 3-way merge engine |
| `changeset` | Binary changeset format — reader, writer, types, invert, concat, JSON export |
| `schema` | `TableSchema`, `TableColumnInfo`, `CrsDefinition` — database introspection |
| `varint` | SQLite variable-length integer encoding |
| `crossval` | Cross-validation against C++ geodiff binary (CI-only) |

## Dependencies

Zero beyond Go's standard library plus two well-established packages already in the TinyOwl ecosystem:

- [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — pure Go SQLite (no CGo)
- [`github.com/twpayne/go-geom`](https://pkg.go.dev/github.com/twpayne/go-geom) — WKB geometry parsing

No system dependencies. Single binary.

## Testing

```bash
# Run all tests
go test ./...

# With race detector
go test -race ./...

# Cross-validate against C++ geodiff binary
GEODIFF_CPP_BIN=/path/to/geodiff go test ./crossval/ -v
```

## Performance

Pure Go, zero CGo. Benchmarked on Intel i7-12700KF (20 threads):

### High-level operations

| Operation | ns/op | allocs |
|-----------|------:|-------:|
| `CreateChangeset` (identical, 1 table) | 876,000 | 601 |
| `Schema` export | 493,000 | 283 |
| `DumpData` | 503,000 | 317 |
| `MakeCopy` (SQLite backup) | 1,525,000 | 961 |
| `Rebase` (no conflict, 1 table) | 1,023,000 | 623 |

### Changeset format

| Operation | ns/op | allocs |
|-----------|------:|-------:|
| `Reader` — 10,000 INSERTs | 870,000 | 30,002 |
| `Writer` — 1,000 INSERTs | 1,460,000 | 10 |
| `Concat` — 500 + 500 entries | 1,097,000 | 6,838 |
| `Varint` encode | 23 | 0 |
| `Varint` decode | 17 | 0 |

### Go vs C++

go-geodiff is **2.8× faster** than the Qt-based C++ `geodiff` CLI for changeset creation:

```bash
GEODIFF_CPP_BIN=/path/to/geodiff go test ./crossval/ -bench=GoVsCPP -benchtime=3s
```

| | Go | C++ |
|---|---:|---:|
| `CreateChangeset` | **869 μs** | 2,452 μs |

To run all benchmarks:

```bash
go test ./crossval/ -bench=. -benchtime=3s
go test ./changeset/ -bench=. -benchtime=1s
go test ./varint/ -bench=. -benchtime=1s
```

## License

MIT — same as the upstream [geodiff](https://github.com/MerginMaps/geodiff) library.

Copyright (c) 2019 Lutra Consulting Ltd.
Copyright (c) 2026 TinyOwl Contributors
