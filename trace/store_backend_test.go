package trace

import (
	"os"
	"strings"
	"testing"
)

// symbolStoreBackend selects which SymbolStore implementation the shared
// store tests exercise. TestMain runs the package suite once per backend.
var symbolStoreBackend = "gob"

func newTestSymbolStore(path string) FileSymbolStore {
	if symbolStoreBackend == "sqlite" {
		return NewSQLiteSymbolStore(strings.TrimSuffix(path, ".gob") + ".db")
	}
	return NewGOBSymbolStore(path)
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
