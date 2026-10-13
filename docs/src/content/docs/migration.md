---
title: Migrating an Existing Project
description: Move a project indexed by an earlier grepai release to the current layout, and optionally to Qdrant
---

## Who this is for

You already have `.grepai/` directories created by an earlier grepai release and want them on the current on-disk layout without re-indexing from scratch. Three things changed:

| Earlier release | Now |
|---|---|
| Symbol index in `.grepai/symbols.gob`, decoded in full on every `trace` call | `.grepai/symbols.db` (SQLite), opened in milliseconds |
| Default ignore list without lockfiles, tests or nested worktrees | Lockfiles, `*.test.*`, `*.spec.*`, mocks, fixtures and `.worktrees` ignored by default |
| GOB vector index by default | GOB still works; Qdrant recommended for large repositories |

Only the first is required. The symbol index is imported automatically the first time a newer binary opens a project, but that happens silently inside the first `trace` or `watch` call and takes about 30 seconds per 400 MB of index. `grepai migrate` does the same import with progress output, verifies the row counts, and removes the old file.

## 1. Stop the watcher

A watcher started from an earlier release keeps rewriting `symbols.gob` every 30 seconds, which would undo the migration.

```bash
grepai watch --stop
# or, for a workspace
grepai watch --workspace <name> --stop
```

## 2. Migrate the symbol index

```bash
cd <project>
grepai migrate --apply-default-ignores
```

Output for a large repository:

```
kentro (/Users/me/Projects/kentro)
  symbol index: symbols.gob (433.2 MB) -> symbols.db
  importing symbols      102289 / 102289
  importing references  1394974 / 1394974
  importing call edges   682471 / 682471
  importing files         13633 / 13633
  imported 102289 symbols, 1394974 references, 682471 call edges across 13633 files in 29.2s; counts verified
  removed symbols.gob.migrated (433.2 MB)
  reclaimed 433.2 MB
  added ignore patterns: .worktrees, pnpm-lock.yaml, package-lock.json, yarn.lock, **/*.test.ts, ...
  the next watch run removes newly ignored files from the index
```

Flags:

| Flag | Effect |
|---|---|
| `--workspace <name>` | Migrate every project in the workspace |
| `--apply-default-ignores` | Append ignore patterns newer releases ship by default to this project's `config.yaml` |
| `--keep-legacy` | Keep the old index as `symbols.gob.migrated` instead of deleting it |
| `--dry-run` | Report what would change without changing anything |

The command is safe to run again: a project that has already been migrated reports "nothing to import".

If the project root contains checked-out git worktrees under a name other than `.worktrees`, add that directory to `ignore` as well. Every file under it is otherwise indexed once per worktree.

## 3. Optional: move the vector store to Qdrant

The GOB vector index is loaded into memory in full by every `search` and `watch`. Past a few tens of thousands of chunks, Qdrant is faster to open and search, and it is the backend workspaces use. There is no in-place conversion; the vectors are re-embedded, which takes about as long as the original index did.

### Run Qdrant in Docker

The repository `compose.yaml` ships a pinned Qdrant service bound to loopback with a named volume:

```bash
docker compose --profile qdrant up -d
```

To run it from anywhere, save this as `~/.grepai/compose.yaml` and run `docker compose -f ~/.grepai/compose.yaml up -d`:

```yaml
services:
  qdrant:
    image: qdrant/qdrant:v1.17.1
    container_name: grepai-qdrant
    restart: unless-stopped
    ports:
      - "127.0.0.1:6333:6333"   # HTTP API and dashboard
      - "127.0.0.1:6334:6334"   # gRPC API (grepai uses this one)
    volumes:
      - grepai-qdrant:/qdrant/storage

volumes:
  grepai-qdrant:
```

Or without Compose:

```bash
docker run -d --name grepai-qdrant --restart unless-stopped \
  -p 127.0.0.1:6333:6333 -p 127.0.0.1:6334:6334 \
  -v grepai-qdrant:/qdrant/storage qdrant/qdrant:v1.17.1
```

The image is pinned to the version the Go client targets. Qdrant has no authentication by default, so keep the ports on loopback unless you set an API key. The dashboard is at `http://localhost:6333/dashboard`.

### Point the project at it

Edit `.grepai/config.yaml`:

```yaml
store:
  backend: qdrant
  qdrant:
    endpoint: http://localhost
    port: 6334
    collection: myproject   # optional; defaults to the sanitized project path
```

Then rebuild the vector index:

```bash
rm -f .grepai/index.gob
grepai watch
```

The symbol index is independent of the vector backend, so `symbols.db` is kept and `trace` keeps working while the re-index runs. For a workspace, set the same `store` block in `~/.grepai/workspace.yaml` and run `grepai watch --workspace <name>`.

## 4. Restart the watcher

```bash
grepai watch --background
```

The first scan after `--apply-default-ignores` removes the newly ignored files from the index and reports them as "files removed".

## Checklist

- `grepai version` shows the new release
- `.grepai/` contains `symbols.db` and no `symbols.gob`
- `grepai trace callers <symbol>` returns in well under a second
- `grepai status` shows the expected backend and file count
