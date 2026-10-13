package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
)

const (
	payloadKeyFileHash    = "file_hash"
	payloadKeyFileModTime = "file_mod_time"
	indexedChunkIDMarker  = "indexed"
)

// sanitizeUTF8 ensures the string contains only valid UTF-8 characters.
// Invalid sequences are replaced with the Unicode replacement character.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

type QdrantStore struct {
	client           *qdrant.Client
	collectionName   string
	projectNamespace string
	dimensions       int
	apiKey           string
}

func parseHost(endpoint string) string {
	host := strings.TrimPrefix(endpoint, "http://")
	host = strings.TrimPrefix(host, "https://")

	if idx := strings.Index(host, ":"); idx != -1 {
		host = host[:idx]
	}

	if idx := strings.Index(host, "/"); idx != -1 {
		host = host[:idx]
	}

	return host
}

func NewQdrantStore(ctx context.Context, endpoint string, port int, useTLS bool, collection, apiKey string, dimensions int) (*QdrantStore, error) {
	return NewQdrantStoreWithNamespace(ctx, endpoint, port, useTLS, collection, collection, apiKey, dimensions)
}

func NewQdrantStoreWithNamespace(ctx context.Context, endpoint string, port int, useTLS bool, collection, projectNamespace, apiKey string, dimensions int) (*QdrantStore, error) {
	host := parseHost(endpoint)

	if port <= 0 {
		port = 6334
	}
	if projectNamespace == "" {
		projectNamespace = collection
	}

	client, err := qdrant.NewClient(&qdrant.Config{
		Host:   host,
		Port:   port,
		UseTLS: useTLS,
		APIKey: apiKey,
		GrpcOptions: []grpc.DialOption{
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64 * 1024 * 1024)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Qdrant client: %w", err)
	}

	store := &QdrantStore{
		client:           client,
		collectionName:   collection,
		projectNamespace: projectNamespace,
		dimensions:       dimensions,
		apiKey:           apiKey,
	}

	if err := store.ensureCollection(ctx); err != nil {
		return nil, err
	}

	return store, nil
}

func (s *QdrantStore) ensureCollection(ctx context.Context) error {
	exists, err := s.client.CollectionExists(ctx, s.collectionName)
	if err != nil {
		return fmt.Errorf("failed to check collection existence: %w", err)
	}

	if !exists {
		if s.dimensions <= 0 {
			return fmt.Errorf("dimensions must be positive, got: %d", s.dimensions)
		}
		err = s.client.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: s.collectionName,
			VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
				Size:     uint64(s.dimensions),
				Distance: qdrant.Distance_Cosine,
			}),
		})
		if err != nil {
			return fmt.Errorf("failed to create collection: %w", err)
		}
	}

	// Create field index for content_hash to enable efficient lookups.
	// Error is intentionally ignored because the index may already exist.
	_, _ = s.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
		CollectionName: s.collectionName,
		FieldName:      "content_hash",
		FieldType:      qdrant.PtrOf(qdrant.FieldType_FieldTypeKeyword),
	})
	_, _ = s.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
		CollectionName: s.collectionName,
		FieldName:      "project_namespace",
		FieldType:      qdrant.PtrOf(qdrant.FieldType_FieldTypeKeyword),
	})
	_, _ = s.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
		CollectionName: s.collectionName,
		FieldName:      "file_path",
		FieldType:      qdrant.PtrOf(qdrant.FieldType_FieldTypeKeyword),
	})

	return nil
}

func sanitizeCollectionName(path string) string {
	return strings.ReplaceAll(path, "/", "_")
}

func SanitizeCollectionName(path string) string {
	return sanitizeCollectionName(path)
}

func (s *QdrantStore) getUUIDForChunk(chunkID string) uuid.UUID {
	namespace := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	return uuid.NewSHA1(namespace, []byte(s.namespacedChunkID(chunkID)))
}

func (s *QdrantStore) namespacedChunkID(chunkID string) string {
	return s.projectNamespace + "\x00" + chunkID
}

func (s *QdrantStore) namespaceCondition() *qdrant.Condition {
	return qdrant.NewMatch("project_namespace", s.projectNamespace)
}

func (s *QdrantStore) filterWithNamespace(conditions ...*qdrant.Condition) *qdrant.Filter {
	must := make([]*qdrant.Condition, 0, len(conditions)+1)
	must = append(must, s.namespaceCondition())
	must = append(must, conditions...)
	return &qdrant.Filter{Must: must}
}

func (s *QdrantStore) SaveChunks(ctx context.Context, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	points := make([]*qdrant.PointStruct, 0, len(chunks))
	for _, chunk := range chunks {
		payload, err := s.buildChunkPayload(chunk)
		if err != nil {
			return fmt.Errorf("failed to build payload: %w", err)
		}

		pointID := s.getUUIDForChunk(chunk.ID)

		points = append(points, &qdrant.PointStruct{
			Id:      qdrant.NewID(pointID.String()),
			Vectors: qdrant.NewVectors(chunk.Vector...),
			Payload: payload,
		})
	}

	_, err := s.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: s.collectionName,
		Points:         points,
		Wait:           qdrant.PtrOf(true),
	})
	if err != nil {
		return fmt.Errorf("failed to upsert points: %w", err)
	}

	return nil
}

func (s *QdrantStore) buildChunkPayload(chunk Chunk) (map[string]*qdrant.Value, error) {
	payload := make(map[string]*qdrant.Value)

	filePathVal, err := qdrant.NewValue(chunk.FilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create file_path value: %w", err)
	}

	startLineVal, err := qdrant.NewValue(int64(chunk.StartLine))
	if err != nil {
		return nil, fmt.Errorf("failed to create start_line value: %w", err)
	}

	endLineVal, err := qdrant.NewValue(int64(chunk.EndLine))
	if err != nil {
		return nil, fmt.Errorf("failed to create end_line value: %w", err)
	}

	contentVal, err := qdrant.NewValue(sanitizeUTF8(chunk.Content))
	if err != nil {
		return nil, fmt.Errorf("failed to create content value: %w", err)
	}

	hashVal, err := qdrant.NewValue(chunk.Hash)
	if err != nil {
		return nil, fmt.Errorf("failed to create hash value: %w", err)
	}

	updatedAtVal, err := qdrant.NewValue(chunk.UpdatedAt.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("failed to create updated_at value: %w", err)
	}
	projectNamespaceVal, err := qdrant.NewValue(s.projectNamespace)
	if err != nil {
		return nil, fmt.Errorf("failed to create project_namespace value: %w", err)
	}

	payload["file_path"] = filePathVal
	payload["start_line"] = startLineVal
	payload["end_line"] = endLineVal
	payload["content"] = contentVal
	payload["hash"] = hashVal
	payload["updated_at"] = updatedAtVal
	payload["project_namespace"] = projectNamespaceVal

	if chunk.ContentHash != "" {
		contentHashVal, err := qdrant.NewValue(chunk.ContentHash)
		if err != nil {
			return nil, fmt.Errorf("failed to create content_hash value: %w", err)
		}
		payload["content_hash"] = contentHashVal
	}

	return payload, nil
}

func (s *QdrantStore) DeleteByFile(ctx context.Context, filePath string) error {
	filter := s.filterWithNamespace(qdrant.NewMatch("file_path", filePath))

	_, err := s.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: s.collectionName,
		Points:         qdrant.NewPointsSelectorFilter(filter),
	})
	if err != nil {
		return fmt.Errorf("failed to delete points: %w", err)
	}

	return nil
}

func (s *QdrantStore) Search(ctx context.Context, queryVector []float32, limit int, opts SearchOptions) ([]SearchResult, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive, got: %d", limit)
	}

	// Fetch more results to account for filtering by path prefix
	fetchLimit := limit
	if opts.PathPrefix != "" {
		// Fetch 2x the limit to allow for filtering
		maxInt := int(^uint(0) >> 1)
		if limit > maxInt/2 {
			fetchLimit = maxInt
		} else {
			fetchLimit = limit * 2
		}
	}
	if fetchLimit < 0 {
		return nil, fmt.Errorf("invalid fetch limit: %d", fetchLimit)
	}
	fetchLimitU64 := uint64(fetchLimit)

	searchResult, err := s.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: s.collectionName,
		Query:          qdrant.NewQuery(queryVector...),
		Filter:         s.filterWithNamespace(),
		Limit:          qdrant.PtrOf(fetchLimitU64),
		WithPayload:    qdrant.NewWithPayloadInclude("file_path", "start_line", "end_line", "content", "hash", "updated_at"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search: %w", err)
	}

	results := make([]SearchResult, 0, len(searchResult))
	for _, point := range searchResult {
		chunk := s.parseChunkPayload(point.Payload)

		// Filter by path prefix if provided
		if opts.PathPrefix != "" && !strings.HasPrefix(chunk.FilePath, opts.PathPrefix) {
			continue
		}

		results = append(results, SearchResult{
			Chunk: *chunk,
			Score: point.Score,
		})

		// Stop once we have enough results
		if len(results) >= limit {
			break
		}
	}

	return results, nil
}

func (s *QdrantStore) parseChunkPayload(payload map[string]*qdrant.Value) *Chunk {
	chunk := &Chunk{}
	if val, ok := payload["file_path"]; ok {
		chunk.FilePath = val.GetStringValue()
	}
	if val, ok := payload["start_line"]; ok {
		chunk.StartLine = int(val.GetIntegerValue())
	}
	if val, ok := payload["end_line"]; ok {
		chunk.EndLine = int(val.GetIntegerValue())
	}
	if val, ok := payload["content"]; ok {
		chunk.Content = val.GetStringValue()
	}
	if val, ok := payload["hash"]; ok {
		chunk.Hash = val.GetStringValue()
	}
	if val, ok := payload["updated_at"]; ok {
		t, err := time.Parse(time.RFC3339, val.GetStringValue())
		if err == nil {
			chunk.UpdatedAt = t
		}
	}
	if val, ok := payload["content_hash"]; ok {
		chunk.ContentHash = val.GetStringValue()
	}

	return chunk
}

func pointIDString(id *qdrant.PointId) string {
	if id == nil {
		return ""
	}
	if uuid := id.GetUuid(); uuid != "" {
		return uuid
	}
	if _, ok := id.GetPointIdOptions().(*qdrant.PointId_Num); ok {
		return strconv.FormatUint(id.GetNum(), 10)
	}
	return ""
}

// documentFromRetrievedPoints reconstructs indexer skip metadata from existing
// chunk points. ChunkIDs only need to be non-empty for the lastIndexTime gate;
// file_hash is the whole-file hash (not the per-chunk "hash" payload).
func documentFromRetrievedPoints(filePath string, points []*qdrant.RetrievedPoint) *Document {
	if len(points) == 0 {
		return nil
	}

	chunkIDs := make([]string, 0, len(points))
	for _, point := range points {
		if point == nil {
			continue
		}
		if id := pointIDString(point.GetId()); id != "" {
			chunkIDs = append(chunkIDs, id)
		}
	}
	if len(chunkIDs) == 0 {
		chunkIDs = []string{indexedChunkIDMarker}
	}

	doc := &Document{
		Path:     filePath,
		ChunkIDs: chunkIDs,
	}

	for _, point := range points {
		if point == nil {
			continue
		}
		payload := point.GetPayload()
		if doc.Hash == "" {
			if val, ok := payload[payloadKeyFileHash]; ok {
				doc.Hash = val.GetStringValue()
			}
		}
		if doc.ModTime.IsZero() {
			if val, ok := payload[payloadKeyFileModTime]; ok {
				if t, err := time.Parse(time.RFC3339, val.GetStringValue()); err == nil {
					doc.ModTime = t
				}
			}
		}
		if doc.Hash != "" && !doc.ModTime.IsZero() {
			break
		}
	}

	return doc
}

func documentMetadataPayload(doc Document) (map[string]*qdrant.Value, error) {
	hashVal, err := qdrant.NewValue(doc.Hash)
	if err != nil {
		return nil, fmt.Errorf("failed to create file_hash value: %w", err)
	}
	modTimeVal, err := qdrant.NewValue(doc.ModTime.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("failed to create file_mod_time value: %w", err)
	}
	return map[string]*qdrant.Value{
		payloadKeyFileHash:    hashVal,
		payloadKeyFileModTime: modTimeVal,
	}, nil
}

func (s *QdrantStore) GetDocument(ctx context.Context, filePath string) (*Document, error) {
	filter := s.filterWithNamespace(qdrant.NewMatch("file_path", filePath))

	scrollResult, err := s.client.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: s.collectionName,
		Filter:         filter,
		Limit:          qdrant.PtrOf(uint32(1)),
		WithPayload:    qdrant.NewWithPayloadInclude(payloadKeyFileHash, payloadKeyFileModTime),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get document: %w", err)
	}

	return documentFromRetrievedPoints(filePath, scrollResult), nil
}

func (s *QdrantStore) SaveDocument(ctx context.Context, doc Document) error {
	payload, err := documentMetadataPayload(doc)
	if err != nil {
		return err
	}

	filter := s.filterWithNamespace(qdrant.NewMatch("file_path", doc.Path))
	_, err = s.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: s.collectionName,
		Payload:        payload,
		PointsSelector: qdrant.NewPointsSelectorFilter(filter),
		Wait:           qdrant.PtrOf(true),
	})
	if err != nil {
		return fmt.Errorf("failed to save document: %w", err)
	}
	return nil
}

func (s *QdrantStore) DeleteDocument(ctx context.Context, filePath string) error {
	return nil
}

// scrollPageSize is the page size used by scrollAll.
const scrollPageSize = 1000

// scrollAll pages through every point matching the request. The high-level
// client Scroll returns a single page and drops the next-page offset, which
// silently truncated ListDocuments to the first page on collections larger
// than the limit and left deleted files in the index forever.
func (s *QdrantStore) scrollAll(ctx context.Context, req *qdrant.ScrollPoints) ([]*qdrant.RetrievedPoint, error) {
	var all []*qdrant.RetrievedPoint
	req.Limit = qdrant.PtrOf(uint32(scrollPageSize))
	req.Offset = nil
	for {
		resp, err := s.client.GetPointsClient().Scroll(ctx, req)
		if err != nil {
			return nil, err
		}
		all = append(all, resp.GetResult()...)
		next := resp.GetNextPageOffset()
		if next == nil || len(resp.GetResult()) == 0 {
			return all, nil
		}
		req.Offset = next
	}
}

func (s *QdrantStore) ListDocuments(ctx context.Context) ([]string, error) {
	scrollResult, err := s.scrollAll(ctx, &qdrant.ScrollPoints{
		CollectionName: s.collectionName,
		Filter:         s.filterWithNamespace(),
		WithPayload:    qdrant.NewWithPayloadInclude("file_path"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list documents: %w", err)
	}

	pathsMap := make(map[string]bool)
	for _, point := range scrollResult {
		if val, ok := point.Payload["file_path"]; ok {
			pathsMap[val.GetStringValue()] = true
		}
	}

	paths := make([]string, 0, len(pathsMap))
	for path := range pathsMap {
		paths = append(paths, path)
	}

	return paths, nil
}

func (s *QdrantStore) Load(ctx context.Context) error {
	return nil
}

func (s *QdrantStore) Persist(ctx context.Context) error {
	return nil
}

func (s *QdrantStore) Close() error {
	return nil
}

func (s *QdrantStore) GetStats(ctx context.Context) (*IndexStats, error) {
	count, err := s.client.Count(ctx, &qdrant.CountPoints{
		CollectionName: s.collectionName,
		Filter:         s.filterWithNamespace(),
		Exact:          qdrant.PtrOf(true),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to count points: %w", err)
	}
	if count > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("points count %d exceeds maximum int value", count)
	}

	stats := &IndexStats{
		TotalFiles:  0,
		TotalChunks: int(count),
		IndexSize:   0,
		LastUpdated: time.Now(),
	}

	return stats, nil
}

func (s *QdrantStore) ListFilesWithStats(ctx context.Context) ([]FileStats, error) {
	scrollResult, err := s.scrollAll(ctx, &qdrant.ScrollPoints{
		CollectionName: s.collectionName,
		Filter:         s.filterWithNamespace(),
		WithPayload:    qdrant.NewWithPayloadInclude("file_path", "start_line", "end_line"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	fileStats := make(map[string]*FileStats)
	for _, point := range scrollResult {
		filePath := ""
		if val, ok := point.Payload["file_path"]; ok {
			filePath = val.GetStringValue()
		}

		if filePath == "" {
			continue
		}

		if _, exists := fileStats[filePath]; !exists {
			fileStats[filePath] = &FileStats{
				Path:       filePath,
				ChunkCount: 0,
				ModTime:    time.Now(),
			}
		}
		fileStats[filePath].ChunkCount++
	}

	result := make([]FileStats, 0, len(fileStats))
	for _, stat := range fileStats {
		result = append(result, *stat)
	}

	return result, nil
}

func (s *QdrantStore) GetChunksForFile(ctx context.Context, filePath string) ([]Chunk, error) {
	filter := s.filterWithNamespace(qdrant.NewMatch("file_path", filePath))

	scrollResult, err := s.client.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: s.collectionName,
		Filter:         filter,
		Limit:          qdrant.PtrOf(uint32(10000)),
		WithPayload:    qdrant.NewWithPayloadInclude("file_path", "start_line", "end_line", "content", "hash", "updated_at"),
		WithVectors:    qdrant.NewWithVectors(true),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get chunks: %w", err)
	}

	chunks := make([]Chunk, 0, len(scrollResult))
	for _, point := range scrollResult {
		chunk := s.parseChunkPayload(point.Payload)
		if point.Vectors != nil && point.Vectors.GetVector() != nil {
			if dense := point.Vectors.GetVector().GetDense(); dense != nil {
				chunk.Vector = dense.GetData()
			}
		}
		chunks = append(chunks, *chunk)
	}

	return chunks, nil
}

func (s *QdrantStore) GetAllChunks(ctx context.Context) ([]Chunk, error) {
	scrollResult, err := s.scrollAll(ctx, &qdrant.ScrollPoints{
		CollectionName: s.collectionName,
		Filter:         s.filterWithNamespace(),
		WithPayload:    qdrant.NewWithPayloadInclude("file_path", "start_line", "end_line", "content", "hash", "updated_at"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get all chunks: %w", err)
	}

	chunks := make([]Chunk, 0, len(scrollResult))
	for _, point := range scrollResult {
		chunk := s.parseChunkPayload(point.Payload)
		if point.Vectors != nil && point.Vectors.GetVector() != nil {
			if dense := point.Vectors.GetVector().GetDense(); dense != nil {
				chunk.Vector = dense.GetData()
			}
		}
		chunks = append(chunks, *chunk)
	}

	return chunks, nil
}

// LookupByContentHash searches Qdrant for a point matching the content hash.
func (s *QdrantStore) LookupByContentHash(ctx context.Context, contentHash string) ([]float32, bool, error) {
	if contentHash == "" {
		return nil, false, nil
	}

	filter := s.filterWithNamespace(qdrant.NewMatch("content_hash", contentHash))

	scrollResult, err := s.client.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: s.collectionName,
		Filter:         filter,
		Limit:          qdrant.PtrOf(uint32(1)),
		WithVectors:    qdrant.NewWithVectors(true),
	})
	if err != nil {
		return nil, false, fmt.Errorf("failed to lookup by content hash: %w", err)
	}

	if len(scrollResult) == 0 {
		return nil, false, nil
	}

	point := scrollResult[0]
	if point.Vectors != nil && point.Vectors.GetVector() != nil {
		if dense := point.Vectors.GetVector().GetDense(); dense != nil {
			return dense.GetData(), true, nil
		}
	}

	return nil, false, nil
}

// LookupByContentHashes implements BatchEmbeddingCache with one filtered
// scroll for all hashes. Points sharing a content hash (worktrees, copied
// files) all match the filter, so the limit leaves headroom for duplicates;
// a hash crowded out past the limit is simply reported as a miss.
func (s *QdrantStore) LookupByContentHashes(ctx context.Context, contentHashes []string) (map[string][]float32, error) {
	found := make(map[string][]float32, len(contentHashes))
	if len(contentHashes) == 0 {
		return found, nil
	}

	filter := s.filterWithNamespace(qdrant.NewMatchKeywords("content_hash", contentHashes...))
	limit := len(contentHashes)*4 + 16
	if limit > 10000 {
		limit = 10000
	}

	scrollResult, err := s.client.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: s.collectionName,
		Filter:         filter,
		Limit:          qdrant.PtrOf(uint32(limit)),
		WithPayload:    qdrant.NewWithPayloadInclude("content_hash"),
		WithVectors:    qdrant.NewWithVectors(true),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to lookup by content hashes: %w", err)
	}

	for _, point := range scrollResult {
		hashVal, ok := point.Payload["content_hash"]
		if !ok {
			continue
		}
		hash := hashVal.GetStringValue()
		if _, done := found[hash]; done {
			continue
		}
		if point.Vectors != nil && point.Vectors.GetVector() != nil {
			if dense := point.Vectors.GetVector().GetDense(); dense != nil && len(dense.GetData()) > 0 {
				found[hash] = dense.GetData()
			}
		}
	}

	return found, nil
}
