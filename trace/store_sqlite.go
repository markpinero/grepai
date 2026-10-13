package trace

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/yoanbernabeu/grepai/internal/fileutil"
)

// legacyGOBSymbolFileName is the pre-SQLite symbol index that Load imports
// once when found next to the database.
const legacyGOBSymbolFileName = "symbols.gob"

const sqliteSymbolSchema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS files (
	path              TEXT PRIMARY KEY,
	content_hash      TEXT,
	extractor_version TEXT
);
CREATE TABLE IF NOT EXISTS symbols (
	id           INTEGER PRIMARY KEY,
	name         TEXT    NOT NULL,
	kind         TEXT    NOT NULL,
	file         TEXT    NOT NULL,
	line         INTEGER NOT NULL,
	end_line     INTEGER NOT NULL,
	signature    TEXT    NOT NULL,
	receiver     TEXT    NOT NULL,
	package      TEXT    NOT NULL,
	exported     INTEGER NOT NULL,
	language     TEXT    NOT NULL,
	docstring    TEXT    NOT NULL,
	feature_path TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name);
CREATE INDEX IF NOT EXISTS symbols_file ON symbols(file);
CREATE TABLE IF NOT EXISTS refs (
	id          INTEGER PRIMARY KEY,
	symbol_name TEXT    NOT NULL,
	kind        TEXT    NOT NULL,
	file        TEXT    NOT NULL,
	line        INTEGER NOT NULL,
	col         INTEGER NOT NULL,
	context     TEXT    NOT NULL,
	caller_name TEXT    NOT NULL,
	caller_file TEXT    NOT NULL,
	caller_line INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS refs_symbol ON refs(symbol_name, file, line);
CREATE INDEX IF NOT EXISTS refs_file ON refs(file);
CREATE TABLE IF NOT EXISTS call_edges (
	id        INTEGER PRIMARY KEY,
	caller    TEXT    NOT NULL,
	callee    TEXT    NOT NULL,
	file      TEXT    NOT NULL,
	line      INTEGER NOT NULL,
	call_type TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS call_edges_caller ON call_edges(caller);
CREATE INDEX IF NOT EXISTS call_edges_callee ON call_edges(callee);
CREATE INDEX IF NOT EXISTS call_edges_file ON call_edges(file);
`

const (
	symbolColumns = "name, kind, file, line, end_line, signature, receiver, package, exported, language, docstring, feature_path"
	refColumns    = "symbol_name, kind, file, line, col, context, caller_name, caller_file, caller_line"
	edgeColumns   = "caller, callee, file, line, call_type"
)

// SQLiteSymbolStore implements SymbolStore on a SQLite database. Queries are
// served by indexes instead of decoding the whole index into memory, so open
// cost does not grow with repository size and per-file updates touch only
// that file's rows.
type SQLiteSymbolStore struct {
	dbPath string
	mu     sync.Mutex
	db     *sql.DB
}

// NewSQLiteSymbolStore creates a store backed by the database at dbPath.
// Call Load before use.
func NewSQLiteSymbolStore(dbPath string) *SQLiteSymbolStore {
	return &SQLiteSymbolStore{dbPath: dbPath}
}

// Load opens the database, creates the schema, and imports a legacy GOB
// index found next to it when the database is still empty. Other methods
// open the database on first use, so calling Load is optional.
func (s *SQLiteSymbolStore) Load(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked(ctx)
}

func (s *SQLiteSymbolStore) openLocked(ctx context.Context) error {
	if s.db != nil {
		return nil
	}
	if err := fileutil.EnsureParentDir(s.dbPath); err != nil {
		return fmt.Errorf("failed to prepare symbol index directory: %w", err)
	}

	dsn := "file:" + s.dbPath + "?" + url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to open symbol index: %w", err)
	}
	// A single connection keeps writers and readers serialized in-process;
	// cross-process access is handled by WAL plus busy_timeout.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, sqliteSymbolSchema); err != nil {
		_ = db.Close()
		return fmt.Errorf("failed to create symbol index schema: %w", err)
	}
	s.db = db

	if err := s.migrateLegacyGOB(ctx); err != nil {
		log.Printf("Warning: failed to import legacy symbol index: %v", err)
	}
	return nil
}

func (s *SQLiteSymbolStore) migrateLegacyGOB(ctx context.Context) error {
	gobPath := filepath.Join(filepath.Dir(s.dbPath), legacyGOBSymbolFileName)
	if _, err := os.Stat(gobPath); err != nil {
		return nil
	}
	var fileCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&fileCount); err != nil {
		return err
	}
	if fileCount > 0 {
		return nil
	}

	legacy := NewGOBSymbolStore(gobPath)
	if err := legacy.loadUnlocked(); err != nil {
		return err
	}
	start := time.Now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	symStmt, err := tx.PrepareContext(ctx, `INSERT INTO symbols (`+symbolColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer symStmt.Close()
	refStmt, err := tx.PrepareContext(ctx, `INSERT INTO refs (`+refColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer refStmt.Close()
	edgeStmt, err := tx.PrepareContext(ctx, `INSERT INTO call_edges (`+edgeColumns+`) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer edgeStmt.Close()
	fileStmt, err := tx.PrepareContext(ctx, `INSERT INTO files (path, content_hash, extractor_version) VALUES (?,?,?)`)
	if err != nil {
		return err
	}
	defer fileStmt.Close()

	for _, syms := range legacy.index.Symbols {
		for _, sym := range syms {
			if _, err := symStmt.ExecContext(ctx, symbolArgs(sym)...); err != nil {
				return err
			}
		}
	}
	for _, refs := range legacy.index.References {
		for _, ref := range refs {
			if _, err := refStmt.ExecContext(ctx, refArgs(ref)...); err != nil {
				return err
			}
		}
	}
	for _, edge := range legacy.index.CallGraph {
		if _, err := edgeStmt.ExecContext(ctx, edge.Caller, edge.Callee, edge.File, edge.Line, edge.CallType); err != nil {
			return err
		}
	}
	for path := range legacy.fileIndex {
		if _, err := fileStmt.ExecContext(ctx, path, nullIfEmpty(legacy.fileContentHashes[path]), nullIfEmpty(legacy.fileExtractorVersions[path])); err != nil {
			return err
		}
	}
	if err := setMetaTx(ctx, tx, "updated_at", legacy.index.UpdatedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	migrated := gobPath + ".migrated"
	if err := os.Rename(gobPath, migrated); err != nil {
		return fmt.Errorf("imported symbols but could not rename %s: %w", gobPath, err)
	}
	log.Printf("Imported legacy symbol index (%d files) into %s in %s; old index kept at %s",
		len(legacy.fileIndex), filepath.Base(s.dbPath), time.Since(start).Round(time.Millisecond), filepath.Base(migrated))
	return nil
}

// Persist only ensures the database exists: every save is committed immediately.
func (s *SQLiteSymbolStore) Persist(ctx context.Context) error {
	_, err := s.conn()
	return err
}

// Close checkpoints the WAL and closes the database.
func (s *SQLiteSymbolStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *SQLiteSymbolStore) conn() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(context.Background()); err != nil {
		return nil, err
	}
	return s.db, nil
}

// SaveFile persists symbols and references for a file.
func (s *SQLiteSymbolStore) SaveFile(ctx context.Context, filePath string, symbols []Symbol, refs []Reference) error {
	return s.saveFile(ctx, filePath, "", nil, symbols, refs)
}

// SaveFileWithContentHash persists symbols/references and records the file's
// content hash. The stored extractor version is left unchanged.
func (s *SQLiteSymbolStore) SaveFileWithContentHash(ctx context.Context, filePath string, contentHash string, symbols []Symbol, refs []Reference) error {
	return s.saveFile(ctx, filePath, contentHash, nil, symbols, refs)
}

// SaveFileWithSignature persists symbols/references and records both the
// content hash and the extractor version that produced them.
func (s *SQLiteSymbolStore) SaveFileWithSignature(ctx context.Context, filePath string, contentHash, extractorVersion string, symbols []Symbol, refs []Reference) error {
	return s.saveFile(ctx, filePath, contentHash, &extractorVersion, symbols, refs)
}

func (s *SQLiteSymbolStore) saveFile(ctx context.Context, filePath, contentHash string, extractorVersion *string, symbols []Symbol, refs []Reference) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteFileRowsTx(ctx, tx, filePath); err != nil {
		return err
	}

	if len(symbols) > 0 {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO symbols (`+symbolColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		for _, sym := range symbols {
			if _, err := stmt.ExecContext(ctx, symbolArgs(sym)...); err != nil {
				stmt.Close()
				return err
			}
		}
		stmt.Close()
	}

	if len(refs) > 0 {
		refStmt, err := tx.PrepareContext(ctx, `INSERT INTO refs (`+refColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		edgeStmt, err := tx.PrepareContext(ctx, `INSERT INTO call_edges (`+edgeColumns+`) VALUES (?,?,?,?,'direct')`)
		if err != nil {
			refStmt.Close()
			return err
		}
		for _, ref := range refs {
			if _, err := refStmt.ExecContext(ctx, refArgs(ref)...); err != nil {
				refStmt.Close()
				edgeStmt.Close()
				return err
			}
		}
		for _, ref := range refs {
			if ref.CallerName == "" || ref.CallerName == "<top-level>" {
				continue
			}
			if _, err := edgeStmt.ExecContext(ctx, ref.CallerName, ref.SymbolName, ref.File, ref.Line); err != nil {
				refStmt.Close()
				edgeStmt.Close()
				return err
			}
		}
		refStmt.Close()
		edgeStmt.Close()
	}

	if extractorVersion != nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO files (path, content_hash, extractor_version) VALUES (?,?,?)
			ON CONFLICT(path) DO UPDATE SET content_hash = excluded.content_hash, extractor_version = excluded.extractor_version`,
			filePath, nullIfEmpty(contentHash), nullIfEmpty(*extractorVersion))
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO files (path, content_hash) VALUES (?,?)
			ON CONFLICT(path) DO UPDATE SET content_hash = excluded.content_hash`,
			filePath, nullIfEmpty(contentHash))
	}
	if err != nil {
		return err
	}
	if err := setMetaTx(ctx, tx, "updated_at", time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteFile removes all symbols and references for a file.
func (s *SQLiteSymbolStore) DeleteFile(ctx context.Context, filePath string) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteFileRowsTx(ctx, tx, filePath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, filePath); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteFileRowsTx(ctx context.Context, tx *sql.Tx, filePath string) error {
	for _, q := range []string{
		`DELETE FROM symbols WHERE file = ?`,
		`DELETE FROM refs WHERE file = ?`,
		`DELETE FROM call_edges WHERE file = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, filePath); err != nil {
			return err
		}
	}
	return nil
}

// IsFileIndexed checks if a file has been indexed.
func (s *SQLiteSymbolStore) IsFileIndexed(filePath string) bool {
	db, err := s.conn()
	if err != nil {
		return false
	}
	var one int
	return db.QueryRow(`SELECT 1 FROM files WHERE path = ?`, filePath).Scan(&one) == nil
}

// GetFileContentHash returns the stored content hash for a file when available.
func (s *SQLiteSymbolStore) GetFileContentHash(filePath string) (string, bool) {
	return s.fileColumn(filePath, "content_hash")
}

// GetFileExtractorVersion returns the extractor version recorded for a file when available.
func (s *SQLiteSymbolStore) GetFileExtractorVersion(filePath string) (string, bool) {
	return s.fileColumn(filePath, "extractor_version")
}

func (s *SQLiteSymbolStore) fileColumn(filePath, column string) (string, bool) {
	db, err := s.conn()
	if err != nil {
		return "", false
	}
	var v sql.NullString
	if err := db.QueryRow(`SELECT `+column+` FROM files WHERE path = ?`, filePath).Scan(&v); err != nil || !v.Valid {
		return "", false
	}
	return v.String, true
}

// LookupSymbol finds symbol definitions by name.
func (s *SQLiteSymbolStore) LookupSymbol(ctx context.Context, name string) ([]Symbol, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	syms, err := querySymbols(ctx, db, `WHERE name = ?`, name)
	if err != nil {
		return nil, err
	}
	if syms == nil {
		syms = []Symbol{}
	}
	return syms, nil
}

// LookupCallers finds all call references to a symbol.
func (s *SQLiteSymbolStore) LookupCallers(ctx context.Context, symbolName string) ([]Reference, error) {
	refs, err := s.refsForSymbol(ctx, symbolName)
	if err != nil {
		return nil, err
	}
	return filterByReferenceKinds(refs, RefKindCall, ""), nil
}

// LookupReaders finds property/data readers for a symbol name.
func (s *SQLiteSymbolStore) LookupReaders(ctx context.Context, symbolName string) ([]Reference, error) {
	refs, err := s.refsForSymbol(ctx, symbolName)
	if err != nil {
		return nil, err
	}
	return filterByReferenceKinds(refs, RefKindRead), nil
}

// LookupWriters finds property/data writers for a symbol name.
func (s *SQLiteSymbolStore) LookupWriters(ctx context.Context, symbolName string) ([]Reference, error) {
	refs, err := s.refsForSymbol(ctx, symbolName)
	if err != nil {
		return nil, err
	}
	return filterByReferenceKinds(refs, RefKindWrite), nil
}

func (s *SQLiteSymbolStore) refsForSymbol(ctx context.Context, symbolName string) ([]Reference, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	return queryRefs(ctx, db, `WHERE symbol_name = ?`, symbolName)
}

// LookupCallees finds all symbols called by a function.
func (s *SQLiteSymbolStore) LookupCallees(ctx context.Context, symbolName string, file string) ([]Reference, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	edges, err := queryEdges(ctx, db, `WHERE caller = ?`, symbolName)
	if err != nil {
		return nil, err
	}

	var callees []Reference
	seen := make(map[string]bool)
	for _, edge := range edges {
		key := fmt.Sprintf("%s:%d", edge.File, edge.Line)
		if seen[key] {
			continue
		}
		seen[key] = true

		matches, err := queryRefs(ctx, db,
			`WHERE symbol_name = ? AND file = ? AND line = ? AND caller_name = ? AND kind IN ('', ?)`,
			edge.Callee, edge.File, edge.Line, symbolName, RefKindCall)
		if err != nil {
			return nil, err
		}
		if len(matches) > 0 {
			callees = append(callees, matches[0])
		} else if len(callees) == 0 {
			callees = append(callees, Reference{
				SymbolName: edge.Callee,
				File:       edge.File,
				Line:       edge.Line,
				CallerName: symbolName,
			})
		}
	}
	return callees, nil
}

// GetCallGraph builds a call graph from a starting symbol.
func (s *SQLiteSymbolStore) GetCallGraph(ctx context.Context, symbolName string, depth int) (*CallGraph, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}

	graph := &CallGraph{
		Root:  symbolName,
		Nodes: make(map[string]Symbol),
		Edges: []CallEdge{},
		Depth: depth,
	}

	symbolCache := make(map[string][]Symbol)
	symbolsNamed := func(name string) ([]Symbol, error) {
		if syms, ok := symbolCache[name]; ok {
			return syms, nil
		}
		syms, err := querySymbols(ctx, db, `WHERE name = ?`, name)
		if err != nil {
			return nil, err
		}
		symbolCache[name] = syms
		return syms, nil
	}
	shouldTraverse := func(name string, isRoot bool) (bool, error) {
		syms, err := symbolsNamed(name)
		if err != nil {
			return false, err
		}
		if len(syms) == 0 {
			return false, nil
		}
		if isRoot {
			return true, nil
		}
		return len(syms) == 1, nil
	}
	isDeclarationSelfEdge := func(edge CallEdge) (bool, error) {
		if edge.Caller != edge.Callee {
			return false, nil
		}
		syms, err := symbolsNamed(edge.Caller)
		if err != nil {
			return false, err
		}
		for _, sym := range syms {
			if sym.File == edge.File && sym.Line == edge.Line {
				return true, nil
			}
		}
		return false, nil
	}

	visited := make(map[string]bool)
	type queueItem struct {
		name  string
		depth int
	}
	queue := []queueItem{{symbolName, 0}}
	edgeSeen := make(map[string]bool)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current.name] || current.depth > depth {
			continue
		}
		visited[current.name] = true

		if syms, err := symbolsNamed(current.name); err != nil {
			return nil, err
		} else if len(syms) > 0 {
			graph.Nodes[current.name] = syms[0]
		}

		var edges []CallEdge
		if current.depth == 0 {
			edges, err = queryEdges(ctx, db, `WHERE caller = ? OR callee = ?`, current.name, current.name)
		} else {
			edges, err = queryEdges(ctx, db, `WHERE caller = ?`, current.name)
		}
		if err != nil {
			return nil, err
		}

		for _, edge := range edges {
			if edge.Caller == current.name {
				selfEdge, err := isDeclarationSelfEdge(edge)
				if err != nil {
					return nil, err
				}
				if selfEdge {
					continue
				}
				edgeKey := fmt.Sprintf("%s->%s", edge.Caller, edge.Callee)
				if !edgeSeen[edgeKey] {
					graph.Edges = append(graph.Edges, edge)
					edgeSeen[edgeKey] = true
				}
				if !visited[edge.Callee] {
					traverse, err := shouldTraverse(edge.Callee, false)
					if err != nil {
						return nil, err
					}
					if traverse {
						queue = append(queue, queueItem{edge.Callee, current.depth + 1})
					}
				}
			}
			if current.depth == 0 && edge.Callee == current.name {
				selfEdge, err := isDeclarationSelfEdge(edge)
				if err != nil {
					return nil, err
				}
				if selfEdge {
					continue
				}
				edgeKey := fmt.Sprintf("%s->%s", edge.Caller, edge.Callee)
				if !edgeSeen[edgeKey] {
					graph.Edges = append(graph.Edges, edge)
					edgeSeen[edgeKey] = true
				}
				if _, exists := graph.Nodes[edge.Caller]; !exists {
					syms, err := symbolsNamed(edge.Caller)
					if err != nil {
						return nil, err
					}
					if len(syms) > 0 {
						graph.Nodes[edge.Caller] = syms[0]
					}
				}
			}
		}
	}

	return graph, nil
}

// GetSymbolsForFile returns all symbols defined in a specific file.
func (s *SQLiteSymbolStore) GetSymbolsForFile(ctx context.Context, filePath string) ([]Symbol, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	return querySymbols(ctx, db, `WHERE file = ?`, filePath)
}

// GetCallEdges returns all call graph edges.
func (s *SQLiteSymbolStore) GetCallEdges(ctx context.Context) ([]CallEdge, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	edges, err := queryEdges(ctx, db, ``)
	if err != nil {
		return nil, err
	}
	if edges == nil {
		edges = []CallEdge{}
	}
	return edges, nil
}

// GetStats returns statistics about the symbol index.
func (s *SQLiteSymbolStore) GetStats(ctx context.Context) (*SymbolStats, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	stats := &SymbolStats{}
	if err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM symbols),
		(SELECT COUNT(*) FROM refs),
		(SELECT COUNT(*) FROM files)`).Scan(&stats.TotalSymbols, &stats.TotalReferences, &stats.TotalFiles); err != nil {
		return nil, err
	}
	if info, err := os.Stat(s.dbPath); err == nil {
		stats.IndexSize = info.Size()
	}
	var updated string
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'updated_at'`).Scan(&updated); err == nil {
		if n, err := strconv.ParseInt(updated, 10, 64); err == nil {
			stats.LastUpdated = time.Unix(0, n)
		}
	}
	return stats, nil
}

func setMetaTx(ctx context.Context, tx *sql.Tx, key string, t time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, strconv.FormatInt(t.UnixNano(), 10))
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func symbolArgs(sym Symbol) []any {
	return []any{sym.Name, string(sym.Kind), sym.File, sym.Line, sym.EndLine, sym.Signature, sym.Receiver, sym.Package, sym.Exported, sym.Language, sym.Docstring, sym.FeaturePath}
}

func refArgs(ref Reference) []any {
	return []any{ref.SymbolName, ref.Kind, ref.File, ref.Line, ref.Column, ref.Context, ref.CallerName, ref.CallerFile, ref.CallerLine}
}

func querySymbols(ctx context.Context, db *sql.DB, where string, args ...any) ([]Symbol, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+symbolColumns+` FROM symbols `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		var sym Symbol
		var kind string
		if err := rows.Scan(&sym.Name, &kind, &sym.File, &sym.Line, &sym.EndLine, &sym.Signature, &sym.Receiver, &sym.Package, &sym.Exported, &sym.Language, &sym.Docstring, &sym.FeaturePath); err != nil {
			return nil, err
		}
		sym.Kind = SymbolKind(kind)
		out = append(out, sym)
	}
	return out, rows.Err()
}

func queryRefs(ctx context.Context, db *sql.DB, where string, args ...any) ([]Reference, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+refColumns+` FROM refs `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reference
	for rows.Next() {
		var ref Reference
		if err := rows.Scan(&ref.SymbolName, &ref.Kind, &ref.File, &ref.Line, &ref.Column, &ref.Context, &ref.CallerName, &ref.CallerFile, &ref.CallerLine); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func queryEdges(ctx context.Context, db *sql.DB, where string, args ...any) ([]CallEdge, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+edgeColumns+` FROM call_edges `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallEdge
	for rows.Next() {
		var edge CallEdge
		if err := rows.Scan(&edge.Caller, &edge.Callee, &edge.File, &edge.Line, &edge.CallType); err != nil {
			return nil, err
		}
		out = append(out, edge)
	}
	return out, rows.Err()
}
