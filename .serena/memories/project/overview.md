# Granola to Obsidian - Project Overview

## Purpose
Go CLI tool that syncs meeting notes and transcripts from Granola to an Obsidian vault. Uses incremental sync with frontmatter-based tracking (no external state files).

## Architecture
- Single-file Go tool: `main.go` (~1020 lines)
- One external dependency: `github.com/charmbracelet/log v0.4.2`
- Go 1.25.0

## Key Design Decisions
- **LocalNote struct** over `map[string]map[string]string` for type safety
- **Single `walkOutputDir`** collects both note index and transcript sidecar paths in one pass
- **`teeWriter`** with `Fd()` method preserves charmbracelet/log terminal color detection while stripping ANSI for log file output
- **Signal-aware context** (`signal.NotifyContext`) threaded to all HTTP functions for cancellability
- **Single `http.Client`** reused across all API calls
- **`setAPIHeaders` helper** deduplicates header setup for both API endpoints
- **Compiled regex** at package level (`invalidFilenameChars`, `ansiEscape`)
- **`bufio.Scanner`** for frontmatter parsing (reads only needed lines, not entire file)

## API Endpoints
- Documents: `POST https://api.granola.ai/v2/get-documents`
- Transcript: `POST https://api.granola.ai/v1/get-document-transcript`
- Auth: Bearer token from `~/Library/Application Support/Granola/supabase.json`
- User-Agent: `Granola/5.354.0`

## Build & Run
```bash
go build -o granola-sync .
./granola-sync [--dry-run] [--full-sync] OUTPUT_DIR
LOG_LEVEL=debug ./granola-sync ~/vault/Granola
```

## Sync Flow
1. Walk output dir → index notes by `granola_id` frontmatter + collect transcript paths
2. Load credentials from Granola's supabase.json
3. Fetch all documents with pagination (100/page, dedup by ID)
4. Build transcript file index, reconcile sidecar renames
5. For each doc: check timestamps → convert ProseMirror → fetch transcript → write .md + .transcript.json

## Important Gotchas
- `docID` guards: transcript fetches and local index updates skip when ID is empty
- `normalizeTitle` returns "Untitled Granola Note" for empty/nil/whitespace titles
- `sanitizeFilename` falls back to "Untitled Granola Note" if all chars stripped
- `granola_updated_at` falls back to `created_at` in both index building and sync updates
- Credential extraction handles both string and already-parsed map token payloads
- Timestamp comparison uses string comparison (ISO format strings sort correctly)
