<div align="center">

# grepai

### grep for the AI era

[![Product Hunt](https://api.producthunt.com/widgets/embed-image/v1/featured.svg?post_id=1067661&theme=light)](https://www.producthunt.com/products/grepai)

[![GitHub stars](https://img.shields.io/github/stars/yoanbernabeu/grepai?style=flat&logo=github)](https://github.com/yoanbernabeu/grepai/stargazers)
[![Downloads](https://img.shields.io/github/downloads/yoanbernabeu/grepai/total?style=flat&logo=github)](https://github.com/yoanbernabeu/grepai/releases)
[![Go](https://github.com/yoanbernabeu/grepai/actions/workflows/ci.yml/badge.svg)](https://github.com/yoanbernabeu/grepai/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/yoanbernabeu/grepai)](https://goreportcard.com/report/github.com/yoanbernabeu/grepai)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**Search code by meaning, not just text.**

[Documentation](https://yoanbernabeu.github.io/grepai/) · [Installation](#installation) · [Quick Start](#quick-start)

</div>

---

`grepai` is a privacy-first CLI for semantic code search. It uses vector embeddings to understand code meaning, enabling natural language queries that find relevant code—even when naming conventions vary.

**Drastically reduces AI agent input tokens** by providing relevant context instead of raw search results.

## About this fork

This is a fork of [yoanbernabeu/grepai](https://github.com/yoanbernabeu/grepai) tuned for large monorepos indexed locally with Ollama and Qdrant. Everything upstream works the same; the differences are in what happens at scale.

**Symbol index in SQLite.** Upstream keeps the `trace` and `refs` index in a GOB file that is decoded in full on every call. On a 433 MB index (102k symbols, 1.4M references) that meant every trace call, including every MCP request from an agent, paid the full load:

| Query on the same index | Upstream | This fork |
|---|---|---|
| `trace callers` (2,754 callers) | 1.5 to 2.2s, 2.3 to 2.9 GB RAM | 0.17s, 80 MB |
| `trace callees` | 1.4s, 2.7 GB | 0.07s, 31 MB |
| `trace graph --depth 3` | 1.5s, 2.7 GB | 0.10s, 54 MB |
| Watcher resident memory | 1.4 GB | about 150 MB |
| Watcher persist every 30s | re-encode and fsync the whole file | no-op |

Output is identical. Existing projects are imported on first open, or explicitly with `grepai migrate`.

**Other changes**

- Ollama embeds through `/api/embed` with a worker pool and honors `embedder.parallelism`; oversized chunks are re-chunked instead of silently truncated.
- Embedding-cache lookups are batched (one store call per 500 chunks instead of one per chunk) for GOB, Qdrant and Postgres.
- Qdrant `ListDocuments` paginates. Upstream caps it at 1000 points, so on larger collections deleted files were never removed from the index.
- Lockfiles, test scaffolding and nested `.worktrees` checkouts are ignored by default. `grepai migrate --apply-default-ignores` adds them to an older config.
- Atomic index writes, corrupt-index recovery, `--no-worktrees`, and the upstream fixes listed in [MARK.md](MARK.md).

A note on what did not help: on Apple Silicon, Ollama with `nomic-embed-text` embeds about 20 chunks per second regardless of batch size, parallelism or inference server. Indexing speed on a fixed machine comes from embedding fewer tokens, not from the pipeline. See [Migrating an Existing Project](docs/src/content/docs/migration.md) for the Qdrant Docker setup and the upgrade path.

## Features

- **Search by intent** — Ask "authentication logic" and find `handleUserSession`
- **Trace call graphs** — Know who calls a function before you change it
- **100% local** — Your code never leaves your machine
- **Always up-to-date** — File watcher keeps the index fresh automatically
- **AI agent ready** — Works with Claude Code, Cursor, Windsurf out of the box
- **MCP server** — Your AI agent can call grepai directly as a tool

## Installation

**Homebrew (macOS):**
```bash
brew install yoanbernabeu/tap/grepai
```

**Linux/macOS:**
```bash
curl -sSL https://raw.githubusercontent.com/yoanbernabeu/grepai/main/install.sh | sh
```

**Windows (PowerShell):**
```powershell
irm https://raw.githubusercontent.com/yoanbernabeu/grepai/main/install.ps1 | iex
```

Requires an embedding provider — [Ollama](https://ollama.ai) (default), [LM Studio](https://lmstudio.ai), or OpenAI.

**Ollama (recommended):**
```bash
ollama pull embeddinggemma
```

## Quick Start

```bash
grepai init                        # Initialize in your project
grepai watch                       # Start indexing daemon
grepai search "error handling"     # Search semantically
grepai trace callers "Login"       # Find who calls a function
```

## Shell Completion

grepai supports autocompletion for commands, flags, and dynamic values (workspace names, project names, providers, backends).

**Zsh (add to `~/.zshrc`):**
```bash
eval "$(grepai completion zsh)"
```

**Oh-My-Zsh plugin:**
```bash
mkdir -p ${ZSH_CUSTOM:-~/.oh-my-zsh/custom}/plugins/grepai
grepai completion zsh > ${ZSH_CUSTOM:-~/.oh-my-zsh/custom}/plugins/grepai/_grepai
# Then add "grepai" to plugins=(...) in ~/.zshrc
```

**Bash:**
```bash
# Linux
grepai completion bash > /etc/bash_completion.d/grepai

# macOS (requires bash-completion@2)
grepai completion bash > $(brew --prefix)/etc/bash_completion.d/grepai
```

**Fish:**
```bash
grepai completion fish > ~/.config/fish/completions/grepai.fish
```

**PowerShell:**
```powershell
grepai completion powershell | Out-String | Invoke-Expression
```

## What developers say

> *"I just hit my limit and it took 13% of my max5 plan just to read my codebase. I am very, very excited about your new tool."*
> — u/911pleasehold on [r/ClaudeAI](https://www.reddit.com/r/ClaudeAI/comments/1qiv0d3/open_source_i_reduced_claude_code_input_tokens_by/) (280K+ views)

> *"It works great! Takes 5 minutes to install. Crazy!"*
> — [@LesSaleGeek](https://x.com/LesSaleGeek/status/2010335803604611124) on X

> *"The results are incredible!"*
> — [Kenny Nguyen](https://www.linkedin.com/feed/update/urn:li:activity:7419451883293061120?commentUrn=urn%3Ali%3Acomment%3A%28activity%3A7419451883293061120%2C7419464457388453888%29) on LinkedIn

## Why grepai?

`grep` was built in 1973 for exact text matching. Modern codebases need semantic understanding.

| | `grep` / `ripgrep` | `grepai` |
|---|---|---|
| **Search** | Exact text / regex | Semantic understanding |
| **Query** | `"func.*Login"` | `"user authentication flow"` |
| **Finds** | Pattern matches | Conceptually related code |

## Documentation

Full docs, guides, and blog:

- **[Documentation](https://yoanbernabeu.github.io/grepai/)** — Configuration, AI agents, MCP setup
- **[Blog](https://yoanbernabeu.github.io/grepai/blog/)** — Benchmarks, tutorials, release notes

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## License

[MIT License](LICENSE) - Yoan Bernabeu 2026
