package indexer

import (
	"context"
	"fmt"
	"log"
	"maps"
	"runtime"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yoanbernabeu/grepai/embedder"
	"github.com/yoanbernabeu/grepai/framework"
	"github.com/yoanbernabeu/grepai/store"
)

type Indexer struct {
	root          string
	store         store.VectorStore
	embedder      embedder.Embedder
	chunker       *Chunker
	scanner       *Scanner
	processor     *framework.ProcessorRegistry
	lastIndexTime time.Time
}

type IndexStats struct {
	FilesIndexed  int
	FilesSkipped  int
	ChunksCreated int
	FilesRemoved  int
	Duration      time.Duration
	ScannedFiles  []FileMeta // All files found during scan (for reuse by callers)
}

// ProgressInfo contains progress information for indexing
type ProgressInfo struct {
	Current     int    // Current file number (1-indexed)
	Total       int    // Total number of files
	CurrentFile string // Path of current file being processed
}

// ProgressCallback is called for each file during indexing
type ProgressCallback func(info ProgressInfo)

// BatchProgressInfo contains progress information for batch embedding
type BatchProgressInfo struct {
	BatchIndex      int  // Current batch index (0-indexed)
	TotalBatches    int  // Total number of batches
	CompletedChunks int  // Number of chunks completed so far
	TotalChunks     int  // Total number of chunks to embed
	Retrying        bool // True if this is a retry attempt
	Attempt         int  // Retry attempt number (1-indexed, 0 if not retrying)
	StatusCode      int  // HTTP status code when retrying (429 = rate limited, 5xx = server error)
}

// BatchProgressCallback is called for batch embedding progress and retry visibility
type BatchProgressCallback func(info BatchProgressInfo)

func NewIndexer(
	root string,
	st store.VectorStore,
	emb embedder.Embedder,
	chunker *Chunker,
	scanner *Scanner,
	lastIndexTime time.Time,
	processors ...*framework.ProcessorRegistry,
) *Indexer {
	var processor *framework.ProcessorRegistry
	if len(processors) > 0 {
		processor = processors[0]
	}

	return &Indexer{
		root:          root,
		store:         st,
		embedder:      emb,
		chunker:       chunker,
		scanner:       scanner,
		processor:     processor,
		lastIndexTime: lastIndexTime,
	}
}

// IndexAll performs a full index of the project (no progress reporting)
func (idx *Indexer) IndexAll(ctx context.Context) (*IndexStats, error) {
	return idx.IndexAllWithProgress(ctx, nil)
}

// IndexAllWithProgress performs a full index with progress reporting
func (idx *Indexer) IndexAllWithProgress(ctx context.Context, onProgress ProgressCallback) (*IndexStats, error) {
	return idx.IndexAllWithBatchProgress(ctx, onProgress, nil)
}

// IndexAllWithBatchProgress performs a full index with both file and batch progress reporting.
// When the embedder implements BatchEmbedder, files are processed in parallel using cross-file batching.
func (idx *Indexer) IndexAllWithBatchProgress(ctx context.Context, onProgress ProgressCallback, onBatchProgress BatchProgressCallback) (*IndexStats, error) {
	start := time.Now()
	stats := &IndexStats{}

	// Scan all files (metadata-only first pass)
	fileMetas, skipped, err := idx.scanner.ScanMetadata()
	if err != nil {
		return nil, fmt.Errorf("failed to scan files: %w", err)
	}
	stats.FilesSkipped = len(skipped)
	stats.ScannedFiles = fileMetas

	// Get existing documents
	existingDocs, err := idx.store.ListDocuments(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list documents: %w", err)
	}

	existingMap := make(map[string]bool)
	for _, doc := range existingDocs {
		existingMap[doc] = true
	}

	// Every scanned file is accounted for exactly once: it either still exists
	// (and is removed from existingMap below) or it was deleted from disk and
	// stays in existingMap so it gets removed from the index further down.
	for _, fileMeta := range fileMetas {
		delete(existingMap, fileMeta.Path)
	}

	// Decide which files need (re)indexing. Each file's decision only depends
	// on that file's own document lookup + optional content hash, so this is
	// done concurrently across a bounded worker pool. On large repositories
	// (100k+ files) this phase — not embedding — is often the bottleneck,
	// especially right after a watcher restart or branch switch where every
	// file's mtime looks "new" and must be hashed to check for real changes.
	decisions := make([]fileScanDecision, len(fileMetas))
	var completed atomic.Int64

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(scanWorkerLimit())

	for i := range fileMetas {
		fileMeta := fileMetas[i]
		g.Go(func() error {
			decision, err := idx.decideFileScan(gctx, fileMeta)
			if err != nil {
				return fmt.Errorf("failed to get document %s: %w", fileMeta.Path, err)
			}
			decisions[i] = decision

			if onProgress != nil {
				n := completed.Add(1)
				onProgress(ProgressInfo{
					Current:     int(n),
					Total:       len(fileMetas),
					CurrentFile: fileMeta.Path,
				})
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Collect results in original scan order for deterministic output.
	filesToIndex := make([]FileInfo, 0, len(fileMetas))
	for _, decision := range decisions {
		if decision.countAsSkipped {
			stats.FilesSkipped++
		}
		if decision.file != nil {
			filesToIndex = append(filesToIndex, *decision.file)
		}
	}

	if len(filesToIndex) > 0 {
		indexed, chunks, err := idx.indexFilesBatched(ctx, filesToIndex, embedder.Batched(idx.embedder), onBatchProgress)
		if err != nil {
			return nil, err
		}
		stats.FilesIndexed = indexed
		stats.ChunksCreated = chunks
	}

	// Remove deleted files
	for path := range existingMap {
		if err := idx.RemoveFile(ctx, path); err != nil {
			log.Printf("Failed to remove %s: %v", path, err)
			continue
		}
		stats.FilesRemoved++
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

// fileScanDecision is the outcome of deciding whether a single scanned file
// needs (re)indexing.
type fileScanDecision struct {
	// file is non-nil when the file needs (re)indexing.
	file *FileInfo
	// countAsSkipped mirrors the original sequential bookkeeping: mtime-gated
	// skips and unreadable/binary/oversized files count toward
	// stats.FilesSkipped, but files that are unchanged (same content hash)
	// do not -- matching the pre-existing (sequential) behavior exactly.
	countAsSkipped bool
}

// decideFileScan fetches a file's existing document (if any) and determines
// whether it needs (re)indexing, following the same rules as the original
// sequential loop: an mtime fast-path gate, then a content-hash comparison
// for files that need a closer look. It is safe to call concurrently for
// different files -- it only reads from the store and the filesystem.
func (idx *Indexer) decideFileScan(ctx context.Context, fileMeta FileMeta) (fileScanDecision, error) {
	// Fetch the document once -- used by both the mod-time gate and hash check.
	doc, err := idx.store.GetDocument(ctx, fileMeta.Path)
	if err != nil {
		return fileScanDecision{}, err
	}

	// Skip files modified before lastIndexTime -- but only if they have chunks.
	// Files with no chunks need re-indexing even if their mod_time is old
	// (e.g., a prior indexing run created the document but failed to embed).
	if !idx.lastIndexTime.IsZero() && doc != nil && len(doc.ChunkIDs) > 0 {
		fileModTime := time.Unix(fileMeta.ModTime, 0)
		if fileModTime.Before(idx.lastIndexTime) || fileModTime.Equal(idx.lastIndexTime) {
			return fileScanDecision{countAsSkipped: true}, nil
		}
	}

	// Load file content and hash only after metadata filtering.
	file, err := idx.scanner.ScanFile(fileMeta.Path)
	if err != nil {
		log.Printf("Failed to scan %s: %v", fileMeta.Path, err)
		return fileScanDecision{countAsSkipped: true}, nil
	}
	if file == nil {
		return fileScanDecision{countAsSkipped: true}, nil
	}

	if doc != nil && doc.Hash == file.Hash && len(doc.ChunkIDs) > 0 {
		return fileScanDecision{}, nil // File unchanged and has chunks
	}

	return fileScanDecision{file: file}, nil
}

// scanWorkerLimit returns the number of concurrent workers to use when
// deciding which files need (re)indexing (store lookups + optional hashing).
// This phase mixes store I/O (which may be a network round trip for remote
// backends like Postgres/Qdrant) with local file hashing, so it benefits from
// more concurrency than pure CPU-bound work would need.
func scanWorkerLimit() int {
	n := runtime.GOMAXPROCS(0) * 4
	if n < 8 {
		return 8
	}
	if n > 64 {
		return 64
	}
	return n
}

// fileChunkData holds chunking information for a single file during batch processing.
type fileChunkData struct {
	fileIndex  int // Index in the files slice (for result mapping)
	file       FileInfo
	chunkInfos []ChunkInfo
	lineMap    []int
	source     string
}

// prepareFileChunks processes files by deleting existing chunks and creating new chunks.
// Returns the file data for storage and the file chunks for embedding.
// preparedFileChunks holds the chunking outcome for a single file, produced
// concurrently by prepareFileChunks.
type preparedFileChunks struct {
	fd fileChunkData
	fc embedder.FileChunks
	ok bool // false if the file produced no chunks and should be dropped
}

// prepareFileChunks processes files by deleting existing chunks and creating new chunks.
// Returns the file data for storage and the file chunks for embedding.
//
// Each file is independent (its own store delete + chunk generation), so this
// runs concurrently across a bounded worker pool. On large repositories this
// overlaps per-file store round trips (relevant for remote backends like
// Postgres/Qdrant) with the CPU cost of chunking instead of paying both costs
// strictly one file at a time.
func (idx *Indexer) prepareFileChunks(
	ctx context.Context,
	files []FileInfo,
) ([]fileChunkData, []embedder.FileChunks, error) {
	results := make([]preparedFileChunks, len(files))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(scanWorkerLimit())

	for i := range files {
		file := files[i]
		g.Go(func() error {
			if err := idx.store.DeleteByFile(gctx, file.Path); err != nil {
				return fmt.Errorf("failed to delete existing chunks for %s: %w", file.Path, err)
			}

			embedContent, lineMap := idx.embeddingContent(gctx, file)
			chunkInfos := idx.chunker.ChunkWithContext(file.Path, embedContent)
			if len(chunkInfos) == 0 {
				return nil
			}

			contents := make([]string, len(chunkInfos))
			for j, c := range chunkInfos {
				contents[j] = c.EmbedContent
			}

			results[i] = preparedFileChunks{
				fd: fileChunkData{
					fileIndex:  i,
					file:       file,
					chunkInfos: chunkInfos,
					lineMap:    lineMap,
					source:     file.Content,
				},
				fc: embedder.FileChunks{
					FileIndex: i,
					Chunks:    contents,
				},
				ok: true,
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, err
	}

	// Collect results in original file order for deterministic output.
	fileData := make([]fileChunkData, 0, len(files))
	fileChunks := make([]embedder.FileChunks, 0, len(files))
	for _, r := range results {
		if !r.ok {
			continue
		}
		fileData = append(fileData, r.fd)
		fileChunks = append(fileChunks, r.fc)
	}

	return fileData, fileChunks, nil
}

// createStoreChunks creates store.Chunk objects from chunk info and embeddings.
func createStoreChunks(chunkInfos []ChunkInfo, embeddings [][]float32, now time.Time) ([]store.Chunk, []string) {
	chunks := make([]store.Chunk, len(chunkInfos))
	chunkIDs := make([]string, len(chunkInfos))

	for i, info := range chunkInfos {
		chunks[i] = store.Chunk{
			ID:          info.ID,
			FilePath:    info.FilePath,
			StartLine:   info.StartLine,
			EndLine:     info.EndLine,
			Content:     info.Content,
			Vector:      embeddings[i],
			Hash:        info.Hash,
			ContentHash: info.ContentHash,
			UpdatedAt:   now,
		}
		chunkIDs[i] = info.ID
	}

	return chunks, chunkIDs
}

// saveFileData saves chunks and document metadata for a single file.
func (idx *Indexer) saveFileData(ctx context.Context, fd fileChunkData, chunks []store.Chunk, chunkIDs []string) error {
	if err := idx.store.SaveChunks(ctx, chunks); err != nil {
		return fmt.Errorf("failed to save chunks for %s: %w", fd.file.Path, err)
	}

	doc := store.Document{
		Path:     fd.file.Path,
		Hash:     fd.file.Hash,
		ModTime:  time.Unix(fd.file.ModTime, 0),
		ChunkIDs: chunkIDs,
	}

	if err := idx.store.SaveDocument(ctx, doc); err != nil {
		return fmt.Errorf("failed to save document for %s: %w", fd.file.Path, err)
	}

	return nil
}

// wrapBatchProgress creates an embedder.BatchProgress callback from BatchProgressCallback.
func wrapBatchProgress(onProgress BatchProgressCallback) embedder.BatchProgress {
	if onProgress == nil {
		return nil
	}
	return func(batchIndex, totalBatches, completedChunks, totalChunks int, retrying bool, attempt int, statusCode int) {
		onProgress(BatchProgressInfo{
			BatchIndex:      batchIndex,
			TotalBatches:    totalBatches,
			CompletedChunks: completedChunks,
			TotalChunks:     totalChunks,
			Retrying:        retrying,
			Attempt:         attempt,
			StatusCode:      statusCode,
		})
	}
}

// indexFilesBatched indexes multiple files using cross-file batch embedding.
// It collects chunks from all files, forms batches, embeds them in parallel,
// then maps results back and stores them.
func (idx *Indexer) indexFilesBatched(
	ctx context.Context,
	files []FileInfo,
	batchEmb embedder.BatchEmbedder,
	onProgress BatchProgressCallback,
) (filesIndexed int, chunksCreated int, err error) {
	fileData, fileChunks, err := idx.prepareFileChunks(ctx, files)
	if err != nil {
		return 0, 0, err
	}

	if len(fileChunks) == 0 {
		return 0, 0, nil
	}

	// Check embedding cache for content-addressed deduplication
	cache, hasCache := idx.store.(store.EmbeddingCache)
	var totalCacheHits int

	// Resolve every chunk's content hash up front so remote stores answer in
	// a few batched calls instead of one round trip per chunk.
	var cachedVectors map[string][]float32
	if hasCache {
		hashes := make([]string, 0, len(fileChunks))
		for _, fd := range fileData {
			for _, chunk := range fd.chunkInfos {
				if chunk.ContentHash != "" {
					hashes = append(hashes, chunk.ContentHash)
				}
			}
		}
		cachedVectors = lookupCachedVectors(ctx, cache, hashes)
	}

	// Pre-fill cached embeddings and filter out fully-cached files
	type preFilled struct {
		fdIndex   int
		vectors   [][]float32
		allCached bool
	}

	var preFilledFiles []preFilled
	var remainingFileData []fileChunkData
	var remainingFileChunks []embedder.FileChunks

	for i, fd := range fileData {
		if !hasCache {
			remainingFileData = append(remainingFileData, fd)
			remainingFileChunks = append(remainingFileChunks, fileChunks[i])
			continue
		}

		vecs := make([][]float32, len(fd.chunkInfos))
		allCached := true
		for j, chunk := range fd.chunkInfos {
			vec, found := cachedVectors[chunk.ContentHash]
			if chunk.ContentHash == "" || !found {
				allCached = false
				continue
			}
			vecs[j] = vec
			totalCacheHits++
		}

		if allCached {
			preFilledFiles = append(preFilledFiles, preFilled{fdIndex: i, vectors: vecs, allCached: true})
		} else {
			remainingFileData = append(remainingFileData, fd)
			remainingFileChunks = append(remainingFileChunks, fileChunks[i])
		}
	}

	if totalCacheHits > 0 {
		log.Printf("Reused %d cached embeddings across %d files", totalCacheHits, len(preFilledFiles))
	}

	// Save fully-cached files immediately
	now := time.Now()
	for _, pf := range preFilledFiles {
		fd := fileData[pf.fdIndex]
		idx.remapChunksToSource(fd.chunkInfos, fd.file.Path, fd.source, fd.lineMap)
		chunks, chunkIDs := createStoreChunks(fd.chunkInfos, pf.vectors, now)
		if err := idx.saveFileData(ctx, fd, chunks, chunkIDs); err != nil {
			return filesIndexed, chunksCreated, err
		}
		filesIndexed++
		chunksCreated += len(chunks)
	}

	// Embed remaining (non-cached) files
	if len(remainingFileChunks) > 0 {
		fileEmbeddings, err := idx.embedWithReChunking(ctx, batchEmb, remainingFileData, remainingFileChunks, len(files), onProgress)
		if err != nil {
			return filesIndexed, chunksCreated, err
		}

		for _, fd := range remainingFileData {
			embeddings := fileEmbeddings[fd.fileIndex]
			if len(embeddings) != len(fd.chunkInfos) {
				log.Printf("Warning: embedding count mismatch for %s: got %d, expected %d",
					fd.file.Path, len(embeddings), len(fd.chunkInfos))
				continue
			}
			idx.remapChunksToSource(fd.chunkInfos, fd.file.Path, fd.source, fd.lineMap)
			chunks, chunkIDs := createStoreChunks(fd.chunkInfos, embeddings, now)
			if err := idx.saveFileData(ctx, fd, chunks, chunkIDs); err != nil {
				return filesIndexed, chunksCreated, err
			}
			filesIndexed++
			chunksCreated += len(chunks)
		}
	}

	return filesIndexed, chunksCreated, nil
}

// maxReChunkAttempts is the maximum number of times we'll try to re-chunk
// before giving up on a file.
const maxReChunkAttempts = 3

// IndexFile indexes a single file
// IndexFile indexes a single file through the same batched path as a full
// index run and returns the number of chunks stored.
func (idx *Indexer) IndexFile(ctx context.Context, file FileInfo) (int, error) {
	_, chunks, err := idx.indexFilesBatched(ctx, []FileInfo{file}, embedder.Batched(idx.embedder), nil)
	return chunks, err
}

// embedWithReChunking attempts to embed chunks, automatically re-chunking
// any chunks that exceed the embedder's context limit.
// embedWithReChunking embeds every file's chunks in cross-file batches and
// returns per-file embeddings indexed by fileIndex. When the embedder rejects
// an input for exceeding its context, the offending chunk is split with the
// chunker and the embed is retried; fileData and fileChunks are updated in
// place so callers see the final chunk list. A file is re-chunked at most
// maxReChunkAttempts times.
func (idx *Indexer) embedWithReChunking(
	ctx context.Context,
	batchEmb embedder.BatchEmbedder,
	fileData []fileChunkData,
	fileChunks []embedder.FileChunks,
	numFiles int,
	onProgress BatchProgressCallback,
) ([][][]float32, error) {
	maxSize, maxTokens := embedder.MaxBatchSize, embedder.MaxBatchTokens
	if sizer, ok := batchEmb.(embedder.BatchSizer); ok {
		maxSize, maxTokens = sizer.BatchLimits()
	}
	attempts := make(map[int]int)

	for {
		batches := embedder.FormBatchesWithLimits(fileChunks, maxSize, maxTokens)
		results, err := batchEmb.EmbedBatches(ctx, batches, wrapBatchProgress(onProgress))
		if err == nil {
			return embedder.MapResultsToFiles(batches, results, numFiles), nil
		}

		ctxErr := embedder.AsContextLengthError(err)
		if ctxErr == nil {
			return nil, fmt.Errorf("failed to embed batches: %w", err)
		}
		if ctxErr.BatchIndex < 0 || ctxErr.BatchIndex >= len(batches) ||
			ctxErr.ChunkIndex < 0 || ctxErr.ChunkIndex >= len(batches[ctxErr.BatchIndex].Entries) {
			return nil, fmt.Errorf("failed to embed batches: %w", err)
		}
		entry := batches[ctxErr.BatchIndex].Entries[ctxErr.ChunkIndex]

		k := -1
		for i := range fileData {
			if fileData[i].fileIndex == entry.FileIndex {
				k = i
				break
			}
		}
		if k < 0 || k >= len(fileChunks) || entry.ChunkIndex >= len(fileData[k].chunkInfos) {
			return nil, fmt.Errorf("failed to embed batches: %w", err)
		}
		fd := &fileData[k]
		attempts[fd.fileIndex]++
		if attempts[fd.fileIndex] > maxReChunkAttempts {
			return nil, fmt.Errorf("exceeded maximum re-chunk attempts (%d) for %s", maxReChunkAttempts, fd.file.Path)
		}

		failed := fd.chunkInfos[entry.ChunkIndex]
		subChunks := idx.chunker.ReChunk(failed, entry.ChunkIndex)
		if len(subChunks) == 0 {
			return nil, fmt.Errorf("re-chunking produced no chunks for %s", fd.file.Path)
		}
		log.Printf("Re-chunking %s chunk %d into %d sub-chunks (attempt %d/%d): context limit exceeded",
			fd.file.Path, entry.ChunkIndex, len(subChunks), attempts[fd.fileIndex], maxReChunkAttempts)

		rechunked := make([]ChunkInfo, 0, len(fd.chunkInfos)+len(subChunks)-1)
		rechunked = append(rechunked, fd.chunkInfos[:entry.ChunkIndex]...)
		rechunked = append(rechunked, subChunks...)
		rechunked = append(rechunked, fd.chunkInfos[entry.ChunkIndex+1:]...)
		fd.chunkInfos = rechunked

		contents := make([]string, len(rechunked))
		for j, c := range rechunked {
			contents[j] = c.EmbedContent
		}
		fileChunks[k].Chunks = contents //nolint:gosec // k < len(fileChunks) is checked above; gosec does not track it
	}
}

func (idx *Indexer) embeddingContent(ctx context.Context, file FileInfo) (string, []int) {
	if idx.processor == nil {
		return file.Content, nil
	}

	res, err := idx.processor.TransformForEmbedding(ctx, file.Path, file.Content)
	if err != nil {
		log.Printf("Warning: framework embedding transform failed for %s: %v", file.Path, err)
		return file.Content, nil
	}
	framework.LogWarningsOnce(res.Warnings)
	if res.Text == "" {
		return file.Content, nil
	}
	return res.Text, res.GeneratedToSourceLine
}

func (idx *Indexer) remapChunksToSource(chunks []ChunkInfo, filePath, source string, lineMap []int) {
	for i := range chunks {
		startLine, endLine := framework.RemapLineRange(lineMap, chunks[i].StartLine, chunks[i].EndLine)
		snippet := framework.SourceSnippet(source, startLine, endLine)
		if snippet == "" {
			continue
		}
		chunks[i].StartLine = startLine
		chunks[i].EndLine = endLine
		chunks[i].Content = fmt.Sprintf("File: %s\n\n%s", filePath, snippet)
	}
}

// cacheLookupBatchSize bounds the number of hashes sent in one
// LookupByContentHashes call so request payloads stay small.
const cacheLookupBatchSize = 500

// lookupCachedVectors resolves content hashes to cached embedding vectors.
// It uses the store's batched lookup when available and falls back to one
// call per hash otherwise. Lookup failures are logged and treated as misses.
func lookupCachedVectors(ctx context.Context, cache store.EmbeddingCache, hashes []string) map[string][]float32 {
	found := make(map[string][]float32, len(hashes))
	if len(hashes) == 0 {
		return found
	}

	unique := make([]string, 0, len(hashes))
	seen := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		unique = append(unique, h)
	}

	if batchCache, ok := cache.(store.BatchEmbeddingCache); ok {
		for start := 0; start < len(unique); start += cacheLookupBatchSize {
			end := min(start+cacheLookupBatchSize, len(unique))
			res, err := batchCache.LookupByContentHashes(ctx, unique[start:end])
			if err != nil {
				log.Printf("Warning: cache lookup failed: %v", err)
				continue
			}
			maps.Copy(found, res)
		}
		return found
	}

	for _, h := range unique {
		vec, ok, err := cache.LookupByContentHash(ctx, h)
		if err != nil {
			log.Printf("Warning: cache lookup failed for content hash %s: %v", h[:8], err)
			continue
		}
		if ok {
			found[h] = vec
		}
	}
	return found
}

// RemoveFile removes a file from the index
func (idx *Indexer) RemoveFile(ctx context.Context, path string) error {
	if err := idx.store.DeleteByFile(ctx, path); err != nil {
		return fmt.Errorf("failed to delete chunks: %w", err)
	}

	if err := idx.store.DeleteDocument(ctx, path); err != nil {
		return fmt.Errorf("failed to delete document: %w", err)
	}

	return nil
}

// NeedsReindex checks if a file needs reindexing
func (idx *Indexer) NeedsReindex(ctx context.Context, path string, hash string) (bool, error) {
	doc, err := idx.store.GetDocument(ctx, path)
	if err != nil {
		return false, err
	}

	if doc == nil {
		return true, nil
	}

	// Reindex if hash changed OR if document has no chunks (prior indexing failed)
	if doc.Hash != hash || len(doc.ChunkIDs) == 0 {
		return true, nil
	}

	return false, nil
}
