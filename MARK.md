# Mark / hi-high grepai fork

**GitHub:** https://github.com/hi-high/grepai  
**Upstream:** https://github.com/yoanbernabeu/grepai  

Local checkout: `~/Code/Labs/grepai-src`.

## Branch / version

- `main` and `mark/local-harden` track the same tip
- Version string: `0.35.0-mark.6`
- Remotes: `origin` → hi-high/grepai, `upstream` → yoanbernabeu/grepai

## Base harden (mark.1)

1. **Atomic GOB persist** (`store/gob.go`) — temp + fsync + rename
2. **Corrupt index recovery** — undecodable `index.gob` → `index.gob.corrupt`
3. **Shutdown timeout** 2s → 90s
4. **`--no-worktrees`** CLI flag

## Cherry-picked from upstream PRs (mark.2)

| Upstream | Title |
|---|---|
| #248 | Qdrant chunk identity / project namespace |
| #263 | Workspace `ListDocuments` scoped to project |
| #207 | Qdrant gRPC 64MB max + drop unused vectors from GetAllChunks |
| #257 | Configurable embedder timeout + max retries |
| #277 | Parallel file-change detection + WalkDir watcher scan |
| #270 | `watch.discover_worktrees` config (works with `--no-worktrees`) |
| #203 | `.gitignore` must not ignore root `.` |
| #264 | Invalidate symbol cache when extractor version changes |

## mark.3 (Qdrant restart skip)

Workspace watch no longer re-embeds the whole tree on boot.

- `GetDocument` reconstructs non-empty `ChunkIDs` from existing chunk points so the `lastIndexTime` skip gate fires
- `SaveDocument` writes whole-file `file_hash` / `file_mod_time` via `SetPayload` (does not reuse per-chunk `hash`)
- Keyword index on `file_path`

## mark.4 (cwd infers workspace)

`search` / `trace` / `refs` / `mcp-serve` infer `--workspace` and `--project` from cwd when the flags are omitted and cwd sits inside a registered workspace project.

- Explicit `--workspace hum` still searches the whole workspace (does not infer project)
- Residual local `.grepai` on SoT primaries no longer wins over Qdrant workspace

## mark.5 (Ollama batching, cache lookups, defaults)

- `OllamaEmbedder` implements `BatchEmbedder` via `/api/embed` with a worker pool; `embedder.parallelism` now applies to Ollama. `truncate: false` so oversized chunks surface as context-length errors instead of silent prefix embeds.
- `embedder.BatchSizer` lets providers set their own per-request limits (Ollama: 4 inputs).
- `store.BatchEmbeddingCache`: content-hash lookups in one call per 500 hashes for GOB (hash index replaces linear scan), Qdrant (one filtered scroll) and Postgres (one query).
- Qdrant `ListDocuments` / `GetAllChunks` / `ListFilesWithStats` paginate; the old single-page scroll capped at 1000 points and never removed deleted files on larger collections.
- Default ignore list adds lockfiles, `*.test.ts(x)`, `*.spec.ts(x)`, `*.testData.ts`, `__mocks__`, `_fixtures`.
- Measured: Ollama + nomic-embed-text on M5 is ~21 chunks/s regardless of batching, parallelism or server (llama-server matched it). Cutting embedded tokens is the lever, not the pipeline.

## mark.6 (SQLite symbol store)

- `trace.SQLiteSymbolStore` replaces the GOB symbol index for all CLI, MCP and watcher paths. `.grepai/symbols.db` (WAL, pure-Go `modernc.org/sqlite`, no cgo). The GOB store remains in the tree and is still used by its own tests.
- First open imports an existing `symbols.gob` and renames it `symbols.gob.migrated`; delete that file once you trust the new index. Kentro: 433 MB gob, 13.6k files, imported in 21s.
- `trace`/`refs`/MCP calls no longer decode the whole index: kentro trace went from 1.9s / 2.67 GB RSS to 0.05s / 31 MB. Per-file saves are one transaction instead of a scan over every reference; the 30s persist tick is a no-op.
- `trace.FileSymbolStore` interface replaces `*trace.GOBSymbolStore` in `cli/watch.go` signatures.
- Default ignore additions from mark.5 plus `**/*.d.ts` are applied per-project in kentro's config, not in defaults.

## Install

```bash
~/Code/Labs/grepai-src/scripts/install-local.sh
# installs ~/.local/bin/grepai
```

## Always-on big projects

- SoT: `~/.grepai/big-projects.yaml`
- Ensure: `grepai-ensure` (LaunchAgent `com.markpinero.grepai-watchdog`)
- Doctor: `grepai-doctor`
- Add: `grepai-big-add <workspace> /abs/path`
- Stop: `grepai-stop-big`

Big projects use **Qdrant workspaces**, not GOB.
