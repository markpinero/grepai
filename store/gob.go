package store

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yoanbernabeu/grepai/internal/fileutil"
)

type GOBStore struct {
	indexPath     string
	lockPath      string
	chunks        map[string]Chunk    // id -> chunk
	documents     map[string]Document // path -> document
	byContentHash map[string]string   // content hash -> a chunk id carrying it (not persisted; rebuilt on load)
	mu            sync.RWMutex
}

type gobData struct {
	Chunks    map[string]Chunk
	Documents map[string]Document
}

func NewGOBStore(indexPath string) *GOBStore {
	return &GOBStore{
		indexPath:     indexPath,
		lockPath:      indexPath + ".lock",
		chunks:        make(map[string]Chunk),
		documents:     make(map[string]Document),
		byContentHash: make(map[string]string),
	}
}

func (s *GOBStore) SaveChunks(ctx context.Context, chunks []Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, chunk := range chunks {
		s.chunks[chunk.ID] = chunk
		if chunk.ContentHash != "" && len(chunk.Vector) > 0 {
			s.byContentHash[chunk.ContentHash] = chunk.ID
		}
	}

	return nil
}

func (s *GOBStore) DeleteByFile(ctx context.Context, filePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, ok := s.documents[filePath]
	if !ok {
		return nil
	}

	for _, chunkID := range doc.ChunkIDs {
		if chunk, ok := s.chunks[chunkID]; ok && s.byContentHash[chunk.ContentHash] == chunkID {
			delete(s.byContentHash, chunk.ContentHash)
		}
		delete(s.chunks, chunkID)
	}

	return nil
}

// rebuildContentHashIndex recreates the content-hash lookup from s.chunks.
// Caller must hold s.mu.
func (s *GOBStore) rebuildContentHashIndex() {
	s.byContentHash = make(map[string]string, len(s.chunks))
	for id, chunk := range s.chunks {
		if chunk.ContentHash != "" && len(chunk.Vector) > 0 {
			s.byContentHash[chunk.ContentHash] = id
		}
	}
}

func (s *GOBStore) Search(ctx context.Context, queryVector []float32, limit int, opts SearchOptions) ([]SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	results := make([]SearchResult, 0, len(s.chunks))

	for _, chunk := range s.chunks {
		// Filter by path prefix if provided
		if opts.PathPrefix != "" && !strings.HasPrefix(chunk.FilePath, opts.PathPrefix) {
			continue
		}
		score := cosineSimilarity(queryVector, chunk.Vector)
		results = append(results, SearchResult{
			Chunk: chunk,
			Score: score,
		})
	}

	// Sort by score descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

func (s *GOBStore) GetDocument(ctx context.Context, filePath string) (*Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	doc, ok := s.documents[filePath]
	if !ok {
		return nil, nil
	}

	return &doc, nil
}

func (s *GOBStore) SaveDocument(ctx context.Context, doc Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.documents[doc.Path] = doc
	return nil
}

func (s *GOBStore) DeleteDocument(ctx context.Context, filePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.documents, filePath)
	return nil
}

func (s *GOBStore) ListDocuments(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	paths := make([]string, 0, len(s.documents))
	for path := range s.documents {
		paths = append(paths, path)
	}

	return paths, nil
}

func (s *GOBStore) Load(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Acquire shared (read) file lock for cross-process safety
	lockFile, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		// If we can't create lock file, proceed without locking (backward compat)
		return s.loadUnlocked()
	}
	defer lockFile.Close()

	if err := fileutil.FlockShared(lockFile, false); err != nil {
		// If locking fails, proceed without locking (backward compat)
		return s.loadUnlocked()
	}
	defer func() {
		_ = fileutil.Funlock(lockFile)
	}()

	return s.loadUnlocked()
}

// loadUnlocked performs the actual load without any locking.
// Corrupt / truncated indexes are quarantined to index.gob.corrupt and the
// store starts empty so the next watch scan can rebuild. A failed decode used
// to hard-error forever (unexpected EOF crash loop).
func (s *GOBStore) loadUnlocked() error {
	s.chunks = make(map[string]Chunk)
	s.documents = make(map[string]Document)
	s.byContentHash = make(map[string]string)

	file, err := os.Open(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to open index file: %w", err)
	}
	defer file.Close()

	var data gobData
	decoder := gob.NewDecoder(file)
	if err := decoder.Decode(&data); err != nil {
		// Empty file and torn writes both surface as EOF / unexpected EOF.
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !isGobDecodeEOF(err) {
			// Still recover — any decode failure means the on-disk snapshot is unusable.
			log.Printf("Warning: index decode failed (%v); quarantining and rebuilding", err)
		} else {
			log.Printf("Warning: index truncated or empty (%v); quarantining and rebuilding", err)
		}
		_ = file.Close()
		if qerr := quarantineCorruptIndex(s.indexPath); qerr != nil {
			// If quarantine lost a race (another process already moved it), adopt empty.
			if !os.IsNotExist(qerr) {
				return fmt.Errorf("failed to decode index: %w (quarantine also failed: %v)", err, qerr)
			}
		}
		return nil
	}

	s.chunks = data.Chunks
	s.documents = data.Documents

	if s.chunks == nil {
		s.chunks = make(map[string]Chunk)
	}
	if s.documents == nil {
		s.documents = make(map[string]Document)
	}
	s.rebuildContentHashIndex()

	return nil
}

// isGobDecodeEOF matches gob's wrapped unexpected-EOF strings.
func isGobDecodeEOF(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "EOF")
}

// quarantineCorruptIndex renames index.gob -> index.gob.corrupt (replacing any
// prior evidence). Losers of a rename race with another reader get IsNotExist.
func quarantineCorruptIndex(indexPath string) error {
	corruptPath := indexPath + ".corrupt"
	_ = os.Remove(corruptPath) // best-effort replace of stale evidence
	if err := os.Rename(indexPath, corruptPath); err != nil {
		return err
	}
	log.Printf("Warning: quarantined corrupt index to %s — run 'grepai watch' to rebuild", corruptPath)
	return nil
}

func (s *GOBStore) Persist(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := fileutil.EnsureParentDir(s.indexPath); err != nil {
		return fmt.Errorf("failed to prepare index directory: %w", err)
	}

	// Acquire exclusive (write) file lock for cross-process safety
	lockFile, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		// If we can't create lock file, proceed without locking (backward compat)
		return s.persistUnlocked()
	}
	defer lockFile.Close()

	if err := fileutil.FlockExclusive(lockFile, false); err != nil {
		// If locking fails, proceed without locking (backward compat)
		return s.persistUnlocked()
	}
	defer func() {
		_ = fileutil.Funlock(lockFile)
	}()

	return s.persistUnlocked()
}

// persistUnlocked performs the actual persist without any locking.
// Writes to a sibling temp file, fsyncs, then atomically renames over the
// target. A crash mid-encode can no longer leave a truncated index.gob —
// the previous good snapshot survives.
func (s *GOBStore) persistUnlocked() error {
	data := gobData{
		Chunks:    s.chunks,
		Documents: s.documents,
	}

	dir := filepath.Dir(s.indexPath)
	tmpFile, err := os.CreateTemp(dir, filepath.Base(s.indexPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create index temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := gob.NewEncoder(tmpFile).Encode(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to encode index: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to sync index temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close index temp file: %w", err)
	}
	if err := fileutil.ReplaceFileAtomically(tmpPath, s.indexPath); err != nil {
		return fmt.Errorf("failed to replace index file: %w", err)
	}
	cleanupTemp = false
	return nil
}

func (s *GOBStore) Close() error {
	return s.Persist(context.Background())
}

func (s *GOBStore) Stats() (numDocs int, numChunks int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.documents), len(s.chunks)
}

func (s *GOBStore) GetStats(ctx context.Context) (*IndexStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var lastUpdated time.Time
	for _, chunk := range s.chunks {
		if chunk.UpdatedAt.After(lastUpdated) {
			lastUpdated = chunk.UpdatedAt
		}
	}

	// Get file size
	var size int64
	if info, err := os.Stat(s.indexPath); err == nil {
		size = info.Size()
	}

	return &IndexStats{
		TotalFiles:  len(s.documents),
		TotalChunks: len(s.chunks),
		IndexSize:   size,
		LastUpdated: lastUpdated,
	}, nil
}

func (s *GOBStore) ListFilesWithStats(ctx context.Context) ([]FileStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := make([]FileStats, 0, len(s.documents))
	for _, doc := range s.documents {
		stats = append(stats, FileStats{
			Path:       doc.Path,
			ChunkCount: len(doc.ChunkIDs),
			ModTime:    doc.ModTime,
		})
	}
	return stats, nil
}

func (s *GOBStore) GetChunksForFile(ctx context.Context, filePath string) ([]Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	doc, ok := s.documents[filePath]
	if !ok {
		return nil, nil
	}

	chunks := make([]Chunk, 0, len(doc.ChunkIDs))
	for _, id := range doc.ChunkIDs {
		if chunk, ok := s.chunks[id]; ok {
			chunks = append(chunks, chunk)
		}
	}
	return chunks, nil
}

func (s *GOBStore) GetAllChunks(ctx context.Context) ([]Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	chunks := make([]Chunk, 0, len(s.chunks))
	for _, chunk := range s.chunks {
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

// LookupByContentHash returns the cached vector for a content hash.
func (s *GOBStore) LookupByContentHash(ctx context.Context, contentHash string) ([]float32, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	vec, ok := s.lookupVectorUnlocked(contentHash)
	return vec, ok, nil
}

// LookupByContentHashes implements BatchEmbeddingCache.
func (s *GOBStore) LookupByContentHashes(ctx context.Context, contentHashes []string) (map[string][]float32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	found := make(map[string][]float32)
	for _, h := range contentHashes {
		if vec, ok := s.lookupVectorUnlocked(h); ok {
			found[h] = vec
		}
	}
	return found, nil
}

// lookupVectorUnlocked returns a copy of the vector for a content hash.
// Caller must hold s.mu.
func (s *GOBStore) lookupVectorUnlocked(contentHash string) ([]float32, bool) {
	id, ok := s.byContentHash[contentHash]
	if !ok {
		return nil, false
	}
	chunk, ok := s.chunks[id]
	if !ok || len(chunk.Vector) == 0 {
		return nil, false
	}
	vec := make([]float32, len(chunk.Vector))
	copy(vec, chunk.Vector)
	return vec, true
}

// cosineSimilarity calculates the cosine similarity between two vectors
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}

	var dotProduct, normA, normB float64

	for i := range a {
		dotProduct += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return float32(dotProduct / (math.Sqrt(normA) * math.Sqrt(normB)))
}
