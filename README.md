# Granola to Obsidian Sync

A Go tool that syncs notes and transcripts from Granola to your Obsidian vault. Supports incremental sync, only processing new or updated documents on subsequent runs.

## Features

- **Incremental Sync**: Only syncs new or updated documents
- **Full History Pagination**: Fetches all available documents, not just the first page
- **Full Transcript Support**: Includes meeting transcripts in formatted sections
- **Date Prefixed Filenames**: Files named as "YYYY-MM-DD - [title].md"
- **Frontmatter-Based Tracking**: Uses each note's `granola_id` frontmatter, no extra state file
- **Dry Run Mode**: Preview changes before syncing
- **Detailed Logging**: Clear reporting of what was synced, updated, or skipped

## Prerequisites

- Go 1.25+
- macOS (for browser callback and Keychain session storage)
- Active Granola account with documents

## Setup

### 1. Clone or Download

```bash
git clone <repository-url>
cd granola-to-obsidian
```

### 2. Build

```bash
go build -o granola-sync .
```

## Usage

### Basic Sync

```bash
./granola-sync /path/to/your/obsidian/folder
```

### Command Line Options

```bash
./granola-sync [OPTIONS] OUTPUT_DIR
```

**Positional Arguments:**
- `OUTPUT_DIR`: Full path to your Obsidian subfolder where notes should be saved

**Options:**
- `--dry-run`: Preview what would be synced without making changes
- `--full-sync`: Force sync all documents (ignore timestamps)
- `--auth-provider=google|microsoft`: Provider for first-time browser sign-in

**Environment Variables:**
- `LOG_LEVEL`: Logging verbosity (`debug`, `info`, `warn`, `error`, `fatal`). Defaults to `info`.
- `GRANOLA_AUTH_PROVIDER`: Default browser sign-in provider (`google` or `microsoft`). Defaults to `google`.

### Examples

```bash
# Daily incremental sync (recommended)
./granola-sync ~/Documents/MyVault/Granola_Notes

# Preview changes before syncing
./granola-sync --dry-run ~/Documents/MyVault/Granola_Notes

# Force sync all documents
./granola-sync --full-sync ~/Documents/MyVault/Granola_Notes

# Verbose logging
LOG_LEVEL=debug ./granola-sync ~/Documents/MyVault/Granola_Notes
```

## Frontmatter Indexing

The tool scans existing markdown files in your output folder and builds an in-memory index from frontmatter:

- `granola_id` (required for matching)
- `updated_at` (or `created_at` fallback for incremental comparison)

### Sync Logic

- **New documents**: Synced when no local note has matching `granola_id`
- **Updated documents**: Synced when Granola's `updated_at` is newer than local frontmatter `updated_at`
- **Unchanged documents**: Skipped entirely (no API calls made)
- **Renamed local files**: Preserved; updates are written back to the file matched by `granola_id`
- **Transcript sidecars**: Indexed by document ID and renamed to match note filename changes when possible

## Output Format

### File Naming

New notes are saved in date-ordered folders under `OUTPUT_DIR`:
`YYYY/MM - Mon/YYYY-MM-DD - [sanitized_title].md`

If transcript data is available, a sidecar JSON file is also saved as:
`YYYY/MM - Mon/YYYY-MM-DD - [sanitized_title].transcript.json`

Examples:
- `2024/01 - Jan/2024-01-15 - Team Meeting Notes.md`
- `2024/02 - Feb/2024-02-16 - Project Review.md`
- `2024/01 - Jan/2024-01-15 - Team Meeting Notes.transcript.json`

Existing notes are updated in place at whatever path they currently have, so you
can move or rename them freely. When a note is updated, any `with:` and `tags:`
values you have added to its frontmatter are kept; the other frontmatter fields
and the body are regenerated from Granola.

### Document Structure

Each synced document includes:

```markdown
---
granola_id: doc_123456
title: "Team Meeting Notes"
created_at: 2024-01-15T10:30:00Z
updated_at: 2024-01-15T11:00:00Z
has_transcript: true
with: []
tags:
  - meeting
---

# Team Meeting Notes

[Your formatted notes content here]

---

## Transcript

**John Smith** [01:23]: Let's start with the quarterly review.

**Sarah Johnson** [01:45]: The numbers look good for Q3.
```

## Credentials

The tool uses the same WorkOS browser sign-in and token-refresh endpoints as the
Granola desktop app. It does not use Granola's companion CLI.

- On first run, the tool opens Granola's sign-in page using PKCE. Granola's web
  callback is hard-wired to open the desktop app, so copy the full
  `https://www.granola.ai/app-redirect?...` URL from the browser, paste it into
  the waiting CLI, and press Return.
- The resulting refreshable session is stored in the macOS Keychain under
  `ai.granola.sync`; tokens are refreshed and rotated automatically.
- Older Granola releases can still use
  `~/Library/Application Support/Granola/supabase.json.enc` or the plaintext
  `supabase.json` as a legacy fallback.

Current Granola releases store their desktop encryption key in an app-only
Keychain access group. A separately signed CLI cannot read that key, so this
tool creates its own Granola session instead of attempting to bypass macOS's
access control or sharing the desktop app's rotating refresh token.

## Logging

The tool logs to both stderr and `granola_sync.log` including:
- Sync statistics (created, updated, skipped counts)
- Document processing details
- API response information
- Error messages and debugging info

## Troubleshooting

### Common Issues

1. **Browser sign-in does not finish**
	- It is normal for the final page to open Granola. The CLI reads the matching
	  `login-complete` callback from Granola's local app data automatically.
	- The active Granola account is supplied as a login hint and the returned
	  session is rejected if it belongs to a different account.
   - If automatic detection is unavailable, copy the page's full `app-redirect`
     URL and paste it into the waiting CLI.
   - Re-run with `--auth-provider=microsoft` if the account uses Microsoft.

2. **"No transcript available"**
   - Not all documents have transcripts (this is normal)
   - Only meeting recordings generate transcripts

3. **"No access token found" / "access token has expired"**
   - The tool should refresh its Keychain session automatically; if refresh was
     revoked, it starts browser sign-in again.
   - Older Granola releases may still show a Keychain prompt for `Granola Safe
     Storage` when the legacy desktop-file fallback is used.

4. **"Granola returned no documents for this session"**
   - The browser sign-in landed on a Granola account that has no notes (for
     example a second Google account). The log names the account it used and
     probes the alternate endpoint so you can rule out an API change.
   - Remove the stored session and re-run, choosing the account the desktop app
     is signed in as: `security delete-generic-password -s ai.granola.sync`
   - Rebuilding the binary changes its code signature, so the next run may show
     one Keychain prompt for `ai.granola.sync`; click "Always Allow".

5. **"Skipping document ... no suitable content found"**
   - Some docs may not have note-body content in the documents response
   - The tool falls back to transcript-only export when transcript data exists
   - This warning means neither note-body content nor transcript data was available

### Debug Mode

For detailed debugging:

```bash
LOG_LEVEL=debug ./granola-sync ~/path/to/vault/Granola_Notes
```

Or check the log file:

```bash
tail -f granola_sync.log
```

### Force Re-Sync

To force a complete re-sync of all documents:

```bash
./granola-sync --full-sync /path/to/your/obsidian/folder
```

## File Structure

```
granola-to-obsidian/
├── main.go              # Main sync tool
├── go.mod               # Go module definition
├── go.sum               # Dependency checksums
├── granola_sync.log     # Detailed logs (gitignored)
└── README.md            # This file
```

## History

Original version:
* forked from https://github.com/jeremysuriel/granola-to-obsidian
* script based on this article: https://josephthacker.com/hacking/2025/05/08/reverse-engineering-granola-notes.html.
* rewritten in Go with a focus on incremental syncing and robust error handling (last python version: https://github.com/ukd1/granola-to-obsidian/commit/866bd292bae094bbca111121500852ec0b0b2a07)
