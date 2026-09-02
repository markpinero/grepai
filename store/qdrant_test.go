package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
)

func mustCreateValue(t *testing.T, value interface{}) *qdrant.Value {
	t.Helper()
	val, err := qdrant.NewValue(value)
	if err != nil {
		t.Fatalf("failed to create qdrant value: %v", err)
	}
	return val
}

// TestSanitizeCollectionName tests the exported SanitizeCollectionName function
func TestSanitizeCollectionName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple path", "/Users/test/project", "_Users_test_project"},
		{"nested path", "/home/user/src/github.com/repo", "_home_user_src_github.com_repo"},
		{"root path", "/", "_"},
		{"multiple slashes", "///", "___"},
		{"no slashes", "myproject", "myproject"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeCollectionName(tt.input)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

// TestParseHost tests the parseHost function with various endpoint formats
func TestParseHost(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"http scheme", "http://localhost", "localhost"},
		{"https scheme", "https://qdrant.io", "qdrant.io"},
		{"with port", "localhost:6334", "localhost"},
		{"http with port", "http://localhost:6334", "localhost"},
		{"https with port", "https://qdrant.io:443", "qdrant.io"},
		{"with path", "http://localhost/v1", "localhost"},
		{"with port and path", "http://localhost:6334/v1", "localhost"},
		{"IP address", "192.168.1.1", "192.168.1.1"},
		{"complex URL", "https://qdrant-cluster.qdrant.io:6334/v1/collections", "qdrant-cluster.qdrant.io"},
		{"just hostname", "localhost", "localhost"},
		{"just IP", "127.0.0.1", "127.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseHost(tt.input)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

// TestGetUUIDForChunk tests that UUID generation is deterministic for the same chunk ID
func TestGetUUIDForChunk(t *testing.T) {
	store := &QdrantStore{}

	chunkID := "test-file.go:10-20"
	uuid1 := store.getUUIDForChunk(chunkID)
	uuid2 := store.getUUIDForChunk(chunkID)

	if uuid1 != uuid2 {
		t.Errorf("expected same UUID for same chunk ID, got %v and %v", uuid1, uuid2)
	}

	// Different chunk IDs should produce different UUIDs
	uuid3 := store.getUUIDForChunk("other-file.go:5-15")
	if uuid1 == uuid3 {
		t.Errorf("expected different UUIDs for different chunk IDs")
	}

	// Verify it's a valid UUID
	if _, err := uuid.Parse(uuid1.String()); err != nil {
		t.Errorf("generated invalid UUID: %v", err)
	}
}

func TestGetUUIDForChunk_IncludesProjectNamespace(t *testing.T) {
	chunkID := "README.md_0"
	victimStore := &QdrantStore{projectNamespace: "/repos/victim"}
	attackerStore := &QdrantStore{projectNamespace: "/repos/attacker"}

	victimUUID := victimStore.getUUIDForChunk(chunkID)
	attackerUUID := attackerStore.getUUIDForChunk(chunkID)

	if victimUUID == attackerUUID {
		t.Fatalf("expected different UUIDs for same chunk ID in different namespaces, got %s", victimUUID)
	}
}

// TestParseChunkPayload tests parsing of Qdrant point payloads
func TestParseChunkPayload(t *testing.T) {
	store := &QdrantStore{}

	now := time.Now().UTC()

	tests := []struct {
		name     string
		payload  map[string]*qdrant.Value
		expected *Chunk
	}{
		{
			name: "complete payload",
			payload: map[string]*qdrant.Value{
				"file_path":  mustCreateValue(t, "test.go"),
				"start_line": mustCreateValue(t, int64(10)),
				"end_line":   mustCreateValue(t, int64(20)),
				"content":    mustCreateValue(t, "test content"),
				"hash":       mustCreateValue(t, "abc123"),
				"updated_at": mustCreateValue(t, now.Format(time.RFC3339)),
			},
			expected: &Chunk{
				FilePath:  "test.go",
				StartLine: 10,
				EndLine:   20,
				Content:   "test content",
				Hash:      "abc123",
				UpdatedAt: now,
			},
		},
		{
			name: "minimal payload",
			payload: map[string]*qdrant.Value{
				"file_path":  mustCreateValue(t, "test.go"),
				"start_line": mustCreateValue(t, int64(1)),
				"end_line":   mustCreateValue(t, int64(10)),
			},
			expected: &Chunk{
				FilePath:  "test.go",
				StartLine: 1,
				EndLine:   10,
				Content:   "",
				Hash:      "",
				UpdatedAt: time.Time{},
			},
		},
		{
			name:    "empty payload",
			payload: map[string]*qdrant.Value{},
			expected: &Chunk{
				FilePath:  "",
				StartLine: 0,
				EndLine:   0,
				Content:   "",
				Hash:      "",
				UpdatedAt: time.Time{},
			},
		},
		{
			name: "partial payload with missing fields",
			payload: map[string]*qdrant.Value{
				"file_path": mustCreateValue(t, "test.go"),
				"content":   mustCreateValue(t, "some content"),
			},
			expected: &Chunk{
				FilePath:  "test.go",
				StartLine: 0,
				EndLine:   0,
				Content:   "some content",
				Hash:      "",
				UpdatedAt: time.Time{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := store.parseChunkPayload(tt.payload)

			if result.FilePath != tt.expected.FilePath {
				t.Errorf("expected FilePath %s, got %s", tt.expected.FilePath, result.FilePath)
			}
			if result.StartLine != tt.expected.StartLine {
				t.Errorf("expected StartLine %d, got %d", tt.expected.StartLine, result.StartLine)
			}
			if result.EndLine != tt.expected.EndLine {
				t.Errorf("expected EndLine %d, got %d", tt.expected.EndLine, result.EndLine)
			}
			if result.Content != tt.expected.Content {
				t.Errorf("expected Content %s, got %s", tt.expected.Content, result.Content)
			}
			if result.Hash != tt.expected.Hash {
				t.Errorf("expected Hash %s, got %s", tt.expected.Hash, result.Hash)
			}
			// Allow small time difference due to parsing
			if !result.UpdatedAt.IsZero() && !tt.expected.UpdatedAt.IsZero() {
				if result.UpdatedAt.Sub(tt.expected.UpdatedAt).Abs() > time.Second {
					t.Errorf("expected UpdatedAt %v, got %v", tt.expected.UpdatedAt, result.UpdatedAt)
				}
			}
		})
	}
}

// TestBuildChunkPayload tests building of Qdrant payloads from chunks
func TestBuildChunkPayload(t *testing.T) {
	store := &QdrantStore{projectNamespace: "/repos/test"}

	now := time.Now().UTC()

	tests := []struct {
		name    string
		chunk   Chunk
		wantErr bool
	}{
		{
			name: "valid chunk",
			chunk: Chunk{
				ID:        "test-id",
				FilePath:  "test.go",
				StartLine: 10,
				EndLine:   20,
				Content:   "test content",
				Hash:      "abc123",
				UpdatedAt: now,
			},
			wantErr: false,
		},
		{
			name: "minimal chunk",
			chunk: Chunk{
				ID:        "test-id",
				FilePath:  "test.go",
				StartLine: 0,
				EndLine:   0,
				Content:   "",
				Hash:      "",
				UpdatedAt: time.Time{},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := store.buildChunkPayload(tt.chunk)

			if (err != nil) != tt.wantErr {
				t.Errorf("buildChunkPayload() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if val, ok := payload["file_path"]; !ok {
					t.Error("expected file_path in payload")
				} else if val.GetStringValue() != tt.chunk.FilePath {
					t.Errorf("expected file_path %s, got %s", tt.chunk.FilePath, val.GetStringValue())
				}

				if val, ok := payload["start_line"]; !ok {
					t.Error("expected start_line in payload")
				} else if val.GetIntegerValue() != int64(tt.chunk.StartLine) {
					t.Errorf("expected start_line %d, got %d", tt.chunk.StartLine, val.GetIntegerValue())
				}

				if val, ok := payload["end_line"]; !ok {
					t.Error("expected end_line in payload")
				} else if val.GetIntegerValue() != int64(tt.chunk.EndLine) {
					t.Errorf("expected end_line %d, got %d", tt.chunk.EndLine, val.GetIntegerValue())
				}

				if val, ok := payload["content"]; !ok {
					t.Error("expected content in payload")
				} else if val.GetStringValue() != tt.chunk.Content {
					t.Errorf("expected content %s, got %s", tt.chunk.Content, val.GetStringValue())
				}

				if val, ok := payload["hash"]; !ok {
					t.Error("expected hash in payload")
				} else if val.GetStringValue() != tt.chunk.Hash {
					t.Errorf("expected hash %s, got %s", tt.chunk.Hash, val.GetStringValue())
				}

				if val, ok := payload["project_namespace"]; !ok {
					t.Error("expected project_namespace in payload")
				} else if val.GetStringValue() != store.projectNamespace {
					t.Errorf("expected project_namespace %s, got %s", store.projectNamespace, val.GetStringValue())
				}
			}
		})
	}
}

// TestSearch_InvalidLimit tests that Search returns error for invalid limits
func TestSearch_InvalidLimit(t *testing.T) {
	store := &QdrantStore{
		client:         nil, // Not used for validation
		collectionName: "test",
		dimensions:     768,
	}

	tests := []struct {
		name  string
		limit int
		want  string
	}{
		{"zero limit", 0, "limit must be positive"},
		{"negative limit", -1, "limit must be positive"},
		{"negative large limit", -100, "limit must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.Search(context.Background(), []float32{0.1, 0.2}, tt.limit, SearchOptions{})
			if err == nil {
				t.Error("expected error, got nil")
			} else if err.Error()[:22] != tt.want {
				t.Errorf("expected error message to start with %s, got %s", tt.want, err.Error())
			}
		})
	}
}

// TestSaveChunks_EmptySlice tests that SaveChunks handles empty chunks slice
func TestSaveChunks_EmptySlice(t *testing.T) {
	store := &QdrantStore{
		client:         nil, // Not used for empty slice
		collectionName: "test",
		dimensions:     768,
	}

	err := store.SaveChunks(context.Background(), []Chunk{})
	if err != nil {
		t.Errorf("expected no error for empty chunks, got %v", err)
	}
}

// TestQdrantStore_StructFields verifies struct has all expected fields
func TestQdrantStore_StructFields(t *testing.T) {
	store := &QdrantStore{
		collectionName:   "test-collection",
		projectNamespace: "/repos/test",
		dimensions:       768,
		apiKey:           "test-key",
	}

	// Use all fields to avoid unused variable warnings
	if store.collectionName != "test-collection" || store.projectNamespace != "/repos/test" || store.dimensions != 768 || store.apiKey != "test-key" {
		t.Errorf("expected collectionName 'test-collection', projectNamespace '/repos/test', dimensions 768, and apiKey 'test-key'")
	}
}

// TestQdrantStore_CollectionNameSanitization verifies / replacement with _
func TestQdrantStore_CollectionNameSanitization(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple path", "/Users/test/project", "_Users_test_project"},
		{"nested path", "/home/user/src/github.com/repo", "_home_user_src_github.com_repo"},
		{"root path", "/", "_"},
		{"multiple slashes", "///", "___"},
		{"no slashes", "myproject", "myproject"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeCollectionName(tt.input)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

// TestQdrantStore_DimensionsField verifies dimension field values
func TestQdrantStore_DimensionsField(t *testing.T) {
	tests := []struct {
		name       string
		dimensions int
	}{
		{"nomic-embed-text", 768},
		{"text-embedding-3-small", 1536},
		{"text-embedding-3-large", 3072},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &QdrantStore{
				collectionName: "test",
				dimensions:     tt.dimensions,
			}

			// Verify dimensions is accessible and correct
			if store.dimensions != tt.dimensions {
				t.Errorf("expected dimensions %d, got %d", tt.dimensions, store.dimensions)
			}
			if store.collectionName != "test" {
				t.Errorf("expected collectionName 'test', got %s", store.collectionName)
			}
		})
	}
}

// TestQdrantStore_ConfigurationVariants tests different config combinations
func TestQdrantStore_ConfigurationVariants(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		collection string
		apiKey     string
		dimensions int
	}{
		{
			name:       "local qdrant",
			endpoint:   "http://localhost:6333",
			collection: "myproject",
			apiKey:     "",
			dimensions: 768,
		},
		{
			name:       "qdrant cloud",
			endpoint:   "https://cloud.qdrant.io",
			collection: "project",
			apiKey:     "secret-key",
			dimensions: 1536,
		},
		{
			name:       "custom endpoint",
			endpoint:   "http://192.168.1.100:6333",
			collection: "test_repo",
			apiKey:     "",
			dimensions: 768,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &QdrantStore{
				collectionName: tt.collection,
				dimensions:     tt.dimensions,
				apiKey:         tt.apiKey,
			}

			// Verify all fields are accessible and correct
			if store.collectionName != tt.collection {
				t.Errorf("expected collectionName %s, got %s", tt.collection, store.collectionName)
			}
			if store.dimensions != tt.dimensions {
				t.Errorf("expected dimensions %d, got %d", tt.dimensions, store.dimensions)
			}
			if store.apiKey != tt.apiKey {
				t.Errorf("expected apiKey %s, got %s", tt.apiKey, store.apiKey)
			}
		})
	}
}

func TestDocumentFromRetrievedPoints_EmptyIsMissing(t *testing.T) {
	if doc := documentFromRetrievedPoints("src/main.go", nil); doc != nil {
		t.Fatalf("expected nil for no points, got %+v", doc)
	}
	if doc := documentFromRetrievedPoints("src/main.go", []*qdrant.RetrievedPoint{}); doc != nil {
		t.Fatalf("expected nil for empty points, got %+v", doc)
	}
}

func TestDocumentFromRetrievedPoints_ExistingChunksEnableSkip(t *testing.T) {
	pointID := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	doc := documentFromRetrievedPoints("src/main.go", []*qdrant.RetrievedPoint{{
		Id: qdrant.NewID(pointID),
		Payload: map[string]*qdrant.Value{
			"file_path": mustCreateValue(t, "src/main.go"),
			"hash":      mustCreateValue(t, "chunk-hash-not-file-hash"),
		},
	}})
	if doc == nil {
		t.Fatal("expected a document when chunks exist")
	}
	if doc.Path != "src/main.go" {
		t.Errorf("Path = %q, want src/main.go", doc.Path)
	}
	if len(doc.ChunkIDs) == 0 {
		t.Fatal("ChunkIDs must be non-empty so lastIndexTime skip can fire")
	}
	if doc.ChunkIDs[0] != pointID {
		t.Errorf("ChunkIDs[0] = %q, want point uuid %q", doc.ChunkIDs[0], pointID)
	}
	if doc.Hash != "" {
		t.Errorf("Hash = %q, want empty when file_hash payload is absent (do not use chunk hash)", doc.Hash)
	}
}

func TestDocumentFromRetrievedPoints_FileHashAndModTime(t *testing.T) {
	modTime := time.Date(2026, 8, 31, 15, 41, 19, 0, time.UTC)
	doc := documentFromRetrievedPoints("pkg/foo.go", []*qdrant.RetrievedPoint{{
		Id: qdrant.NewIDNum(42),
		Payload: map[string]*qdrant.Value{
			"file_path":     mustCreateValue(t, "pkg/foo.go"),
			"file_hash":     mustCreateValue(t, "abc123file"),
			"file_mod_time": mustCreateValue(t, modTime.Format(time.RFC3339)),
			"hash":          mustCreateValue(t, "chunk-hash"),
		},
	}})
	if doc == nil {
		t.Fatal("expected a document")
	}
	if doc.Hash != "abc123file" {
		t.Errorf("Hash = %q, want abc123file", doc.Hash)
	}
	if !doc.ModTime.Equal(modTime) {
		t.Errorf("ModTime = %v, want %v", doc.ModTime, modTime)
	}
	if len(doc.ChunkIDs) != 1 || doc.ChunkIDs[0] != "42" {
		t.Errorf("ChunkIDs = %v, want [42]", doc.ChunkIDs)
	}
}

func TestDocumentFromRetrievedPoints_MissingPointIDStillIndexed(t *testing.T) {
	doc := documentFromRetrievedPoints("a.ts", []*qdrant.RetrievedPoint{{
		Payload: map[string]*qdrant.Value{
			"file_path": mustCreateValue(t, "a.ts"),
		},
	}})
	if doc == nil || len(doc.ChunkIDs) == 0 {
		t.Fatalf("expected existence marker ChunkIDs, got %+v", doc)
	}
}

func TestDocumentMetadataPayload(t *testing.T) {
	modTime := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	payload, err := documentMetadataPayload(Document{
		Path:    "src/main.go",
		Hash:    "filehash",
		ModTime: modTime,
	})
	if err != nil {
		t.Fatalf("documentMetadataPayload: %v", err)
	}
	if got := payload["file_hash"].GetStringValue(); got != "filehash" {
		t.Errorf("file_hash = %q, want filehash", got)
	}
	if got := payload["file_mod_time"].GetStringValue(); got != modTime.Format(time.RFC3339) {
		t.Errorf("file_mod_time = %q, want %s", got, modTime.Format(time.RFC3339))
	}
	if _, ok := payload["hash"]; ok {
		t.Fatal("must not overwrite chunk hash payload")
	}
}
