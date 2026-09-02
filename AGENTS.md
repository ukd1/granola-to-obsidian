# Granola to Obsidian Sync — AGENTS.md

## Quick Start

```bash
just build          # Build binary
just install        # Build + install to $INSTALL_DIR (default ~/.local/bin)
go build -o granola-sync .
go test ./...
./granola-sync /path/to/obsidian/vault/folder
```

## Project Type

Go CLI tool (module `granola-sync`, Go 1.25). `main.go` holds everything except
the macOS Keychain shim, which lives in `keychain_darwin.go` (cgo, Security
framework) with a stub in `keychain_fallback.go` for non-darwin or `CGO_ENABLED=0`
builds. Unit tests are in `main_test.go`. Binary name: `granola-sync`.

## Commands

| Command | Description |
|---|---|
| `just build` | `go build -o granola-sync .` |
| `just install` | Build + copy to `$INSTALL_DIR` (default `~/.local/bin`) |
| `go test ./...` | Run unit tests |
| `./granola-sync OUTPUT_DIR` | Run sync |
| `--dry-run` | Preview changes |
| `--full-sync` | Force re-sync all docs |
| `--auth-provider=google\|microsoft` | Provider for first-time browser sign-in (default `google`, or `GRANOLA_AUTH_PROVIDER`) |
| `LOG_LEVEL=debug ./granola-sync ...` | Verbose logging |

Building on macOS needs the Xcode command line tools for cgo. The Keychain shim
uses `SecKeychain*` APIs that clang flags as deprecated; set
`CGO_CFLAGS=-Wno-deprecated-declarations` to silence the warnings. `go.sum` is
committed — no `go mod tidy` needed unless deps change.

## Architecture (~2300 Lines)

**Data flow:**

1. **Authentication** (`loadOrAuthenticateAccessToken`):
   - Primary: a refreshable WorkOS session stored in the macOS Keychain under
     service `ai.granola.sync`, account `session`, as JSON (`GranolaTokens`).
     Access tokens are refreshed via `POST /v1/refresh-access-token` when inside a
     2-minute expiry window.
   - First run: PKCE browser sign-in through `https://api.granola.ai/v1/auth`
     with the desktop app's account email as `login_hint`. Granola's web callback
     opens the desktop app, so the one-time code is recovered by polling the
     desktop app's Sentry breadcrumb file (`sentry/scope_v3.json`) for the
     matching `login-complete` URL, or from a pasted `app-redirect` URL on stdin.
     The code is exchanged at `POST /v1/workos-auth-complete`.
   - Account guard: the session must belong to the desktop app's active account
     (email compared first, then `external_id` vs the desktop user ID).
   - Legacy fallback: `supabase.json.enc` / `supabase.json` in
     `~/Library/Application Support/Granola/`. Dead on current Granola releases
     because `storage.dek` moved into an app-only Keychain access group.

2. **Document fetch** — paginates `POST https://api.granola.ai/v2/get-documents`
   (100 per page, up to 1000 pages), deduplicates by ID. `decodeDocumentsPage`
   accepts both the `{docs:[...]}` object and a bare array. An empty result
   triggers `diagnoseEmptyDocuments`, which logs the account and probes the
   alternate endpoint and client version.

3. **Local index** — walks output dir for `*.md` files, parses frontmatter
   (`granola_id`, `updated_at`/`created_at`), builds `map[string]LocalNote`

4. **Transcript sidecar reconciliation** — renames `*.transcript.json` files to
   match their associated note's filename

5. **Sync loop** — for each doc:
   - `needsSync()` compares Granola's `updated_at` vs local frontmatter `updated_at`
   - Body via `resolveNoteBody`: ProseMirror JSON in `last_viewed_panel`, else
     `POST /v1/get-document-panels` (HTML or ProseMirror object; `PanelContent`
     accepts both), else transcript-only
   - Writes frontmatter + note body + optional transcript section
   - Writes transcript JSON sidecar `{note}.transcript.json`
   - A document with no body and no transcript segments is skipped. The
     transcript endpoint returns `[]` (not 404) for unrecorded meetings;
     `fetchDocumentTranscript` maps that to "no transcript" so the real run and
     the dry run agree.

**Output path format** (relative to OUTPUT_DIR):
```
{year}/{month_num - Month_name}/{date} - {sanitized_title}.md
e.g. 2024/01 - Jan/2024-01-15 - Team Meeting.md
```
Existing notes are updated in place at whatever path they currently have.

## Key Patterns & Gotchas

### Authentication (macOS-only)
- Keychain access goes through Keychain Services in `keychain_darwin.go`, not
  the `security` CLI. Rebuilding the binary changes its code signature, so the
  next run may show one "granola-sync wants to access ai.granola.sync" prompt.
- To force a fresh sign-in (for example after signing in with the wrong account):
  `security delete-generic-password -s ai.granola.sync`
- The desktop app also receives the `granola://login-complete` callback. That has
  been harmless in practice but is a race the tool does not control.
- Sentry breadcrumbs are a 200-entry ring buffer; the callback is only findable
  shortly after sign-in, which is why the tool polls every second.

### Frontmatter-Based Tracking (No State File)
- No `.granola_sync_state.json` or DB — tracking is purely frontmatter-based
- Local notes are indexed by `granola_id` from frontmatter
- If multiple files share the same `granola_id`, the most recently modified one wins (duplicate warning logged)
- Comparison uses string comparison of ISO 8601 timestamps — assumes lexicographic ordering works because RFC 3339 timestamps sort correctly
- **Hand-edited frontmatter survives updates**: `buildFrontmatter` takes the
  existing note's raw `with:` and `tags:` blocks (via `readFrontmatterBlocks`) and
  keeps them when non-empty. Everything else is regenerated from Granola.

### Transcript Sidecars
- Files are `{note}.transcript.json` alongside each `.md` note
- During sync, stale sidecars are deleted if transcript is no longer available
- During reconciliation, sidecars are renamed to match note filename changes
- Sidecar matching is heuristic: checks path derived from both `localNotes` index and generated path from doc metadata

### ProseMirror → Markdown
- Only handles: `heading`, `paragraph`, `bulletList`/`listItem`, `text` nodes
- All other node types recurse into `Content` children (no error for unknown types)
- No inline formatting (bold, italic) conversion despite Granola likely producing it
- `parseProseMirrorContent` uses safe type assertion (`typeStr, _ := data["type"].(string)`) — missing `type` key results in empty string, not a panic

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
- User-Agent/`X-Client-Version` mimic the Granola desktop app; the version string
  is the `granolaClientVersion` constant
- Pagination safety limit: 1000 pages (100k docs)
- Stops pagination when: empty response, partial page, or zero new unique IDs in a full page
- No 401 handling mid-run: a rejected token exits the process rather than refreshing

## Style & Conventions

- Single `main` package, no exported symbols
- Logging via `charmbracelet/log` — level set from `LOG_LEVEL` env var
- `teeWriter` strips ANSI codes for file output while preserving terminal color detection
- Error handling: `log.Fatal` for startup failures, `log.Error` + `continue` for per-doc errors, `os.Exit(1)` for fatal API/auth errors
- No `defer` cleanup for HTTP response bodies in the pagination loop (closed explicitly)
- Variable naming: descriptive (`outputPath`, `displayDocID`, `transcriptFilesByID`)
- JSON tags: `json:"name"` style, `any` for optional/loosely-typed fields

## Tests

`go test ./...` covers the pure functions: HTML/ProseMirror conversion, panel
selection and decoding, auth URL construction, callback parsing and breadcrumb
detection, token refresh windows, account matching, legacy credential loading,
documents page decoding, and frontmatter preservation. Anything touching the
network, Keychain, or browser is exercised manually with `--dry-run`.

## Dependencies

Direct: `github.com/charmbracelet/log` (logging) and `golang.org/x/net` (HTML
parsing). Transitive deps are lipgloss/colorprofile/term libraries for styled
terminal output. macOS builds link CoreFoundation and Security via cgo.
