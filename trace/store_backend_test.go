package trace

import (
	"os"
	"strings"
	"testing"
)

// symbolStoreBackend selects which SymbolStore implementation the shared
// store tests exercise. TestMain runs the package suite once per backend.
var symbolStoreBackend = "gob"

// newTestSymbolStore returns a store for the active backend and closes it
// when the test ends. Windows cannot delete an open SQLite file, so an
// unclosed store makes t.TempDir cleanup fail.
func newTestSymbolStore(t *testing.T, path string) FileSymbolStore {
	t.Helper()
	var store FileSymbolStore
	if symbolStoreBackend == "sqlite" {
		store = NewSQLiteSymbolStore(strings.TrimSuffix(path, ".gob") + ".db")
	} else {
		store = NewGOBSymbolStore(path)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMain(m *testing.M) {
	for _, backend := range []string{"gob", "sqlite"} {
		symbolStoreBackend = backend
		if code := m.Run(); code != 0 {
			os.Exit(code)
		}
	}
	os.Exit(0)
}
