# Mark / hi-high grepai fork

**GitHub:** https://github.com/hi-high/grepai  
**Upstream:** https://github.com/yoanbernabeu/grepai  

Local checkout: `~/Code/Labs/grepai-src`.

## Branch / version

- `main` and `mark/local-harden` track the same tip
- Version string: `0.35.0-mark.4`
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
