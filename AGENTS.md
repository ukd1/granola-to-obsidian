# Granola to Obsidian Sync — AGENTS.md

## Quick Start

```bash
just build          # Build binary
just install        # Build + install to $INSTALL_DIR (default ~/.local/bin)
go build -o granola-sync .
./granola-sync /path/to/obsidian/vault/folder
```

## Project Type

Single-file Go CLI tool (module `granola-sync`, Go 1.25). No internal packages, no tests. Binary name: `granola-sync`.

## Commands

| Command | Description |
|---|---|
| `just build` | `go build -o granola-sync .` |
| `just install` | Build + copy to `$INSTALL_DIR` (default `~/.local/bin`) |
| `./granola-sync OUTPUT_DIR` | Run sync |
| `--dry-run` | Preview changes |
| `--full-sync` | Force re-sync all docs |
| `LOG_LEVEL=debug ./granola-sync ...` | Verbose logging |

No test runner, no linter config. `go.sum` is committed — no `go mod tidy` needed unless deps change.

## Architecture (Single-File, ~1200 Lines)

**Data flow:**

1. **Credential loading** — reads Granola's encrypted Electron safeStorage on macOS:
   - Primary: `~/Library/Application Support/Granola/supabase.json.enc` (AES-256-GCM, DEK wrapped via macOS Keychain + Chromium OSCrypt format)
   - Fallback: `~/Library/Application Support/Granola/supabase.json` (plaintext JSON)
   - Extracts `workos_tokens` or `cognito_tokens` → JWT access token

2. **Document fetch** — paginates `POST https://api.granola.ai/v2/get-documents` (100 per page, up to 1000 pages), deduplicates by ID

3. **Local index** — walks output dir for `*.md` files, parses frontmatter (`granola_id`, `updated_at`/`created_at`), builds `map[string]LocalNote`

4. **Transcript sidecar reconciliation** — renames `*.transcript.json` files to match their associated note's filename

5. **Sync loop** — for each doc:
   - `needsSync()` compares Granola's `updated_at` vs local frontmatter `updated_at`
   - If stale/new: fetches transcript via `POST /v1/get-document-transcript`
   - Converts ProseMirror JSON content to Markdown (headings, paragraphs, bullet lists, text)
   - Writes `---\ngranola_id: ...\ntitle: "..."\ncreated_at: ...\nupdated_at: ...\nhas_transcript: true/false\nwith: []\ntags:\n  - meeting\n---\n\n` + note body + optional transcript section
   - Writes transcript JSON sidecar `{note}.transcript.json`

**Output path format:**
```
granola/{year}/{month_num - Month_name}/{date} - {sanitized_title}.md
e.g. granola/2024/01 - Jan/2024-01-15 - Team Meeting.md
```

## Key Patterns & Gotchas

### Credential Decryption (macOS-only)
- Uses `security find-generic-password -s "Granola Safe Storage" -w` from the macOS Keychain
- Electron safeStorage uses Chromium's OSCrypt format: optional `v10`/`v11` prefix, AES-128-CBC with PBKDF2-HMAC-SHA1 (salt `saltysalt`, 1003 iterations), IV is 16 spaces
- The DEK (data-encryption key) is stored in `storage.dek`, wrapped with safeStorage + base64-encoded
- First run will prompt for Keychain access — user must click "Always Allow"

### Frontmatter-Based Tracking (No State File)
- No `.granola_sync_state.json` or DB — tracking is purely frontmatter-based
- Local notes are indexed by `granola_id` from frontmatter
- If multiple files share the same `granola_id`, the most recently modified one wins (duplicate warning logged)
- Comparison uses string comparison of ISO 8601 timestamps — assumes lexicographic ordering works because RFC 3339 timestamps sort correctly

### Transcript Sidecars
- Files are `{note}.transcript.json` alongside each `.md` note
- During sync, stale sidecars are deleted if transcript is no longer available
- During reconciliation, sidecars are renamed to match note filename changes
- Sidecar matching is heuristic: checks path derived from both `localNotes` index and generated path from doc metadata

### ProseMirror → Markdown
- Only handles: `heading`, `paragraph`, `bulletList`/`listItem`, `text` nodes
- All other node types recurse into `Content` children (no error for unknown types)
- No inline formatting (bold, italic) conversion despite Granola likely producing it
- If `last_viewed_panel` has no `doc`-type content, falls back to transcript-only export
- `parseProseMirrorContent` uses safe type assertion (`typeStr, _ := data["type"].(string)`) — missing `type` key results in empty string, not a panic

### Token Expiry Handling
- `tokenExpiryFromJWT` checks JWT `exp` claim but there's no refresh logic — expired tokens require the user to open the Granola desktop app to re-write `supabase.json.enc`
- Token extraction handles two payload formats: direct `map[string]any` or JSON-string that needs parsing (`extractTokenMap`)

### Signal Handling
- `signal.NotifyContext` catches SIGINT but only cancels the HTTP context — no graceful shutdown for in-progress writes

### Timestamp Formatting
- `formatTime` converts float64 seconds → `MM:SS`, but string timestamps pass through verbatim

### Filename Sanitization
- Characters `<>:"/\\|?*` are stripped via regex
- Empty result after sanitization → `"Untitled Granola Note"`
- Date prefix uses `CreatedAt`, falls back to `UpdatedAt`, then current date

### HTTP Client
- 60s timeout, no retry logic
- User-Agent/headers mimic Granola desktop app (`Granola/5.354.0`)
- Pagination safety limit: 1000 pages (100k docs)
- Stops pagination when: empty response, partial page, or zero new unique IDs in a full page

## Style & Conventions

- Single `main` package, no exported symbols
- Logging via `charmbracelet/log` — level set from `LOG_LEVEL` env var
- `teeWriter` strips ANSI codes for file output while preserving terminal color detection
- Error handling: `log.Fatal` for startup failures, `log.Error` + `continue` for per-doc errors, `os.Exit(1)` for fatal API/auth errors
- No `defer` cleanup for HTTP response bodies in the pagination loop (closed explicitly)
- Variable naming: descriptive (`outputPath`, `displayDocID`, `transcriptFilesByID`)
- JSON tags: `json:"name"` style, `any` for optional/loosely-typed fields

## No Tests

No `*_test.go` files exist. Test manually by running with `--dry-run` to verify behavior before `--full-sync`.

## Dependencies

Only direct dependency: `github.com/charmbracelet/log`. Transitive deps are lipgloss/colorprofile/term libraries for styled terminal output.
