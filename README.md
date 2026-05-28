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
- Active Granola account with documents
- Granola desktop app installed (for credentials)

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

**Environment Variables:**
- `LOG_LEVEL`: Logging verbosity (`debug`, `info`, `warn`, `error`, `fatal`). Defaults to `info`.

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

Files are saved under date folders:
`YYYY/MM/DD/YYYY-MM-DD - [sanitized_title].md`

If transcript data is available, a sidecar JSON file is also saved as:
`YYYY/MM/DD/YYYY-MM-DD - [sanitized_title].transcript.json`

Examples:
- `2024/01/15/2024-01-15 - Team Meeting Notes.md`
- `2024/01/16/2024-01-16 - Project Review.md`
- `2024/01/15/2024-01-15 - Team Meeting Notes.transcript.json`

### Document Structure

Each synced document includes:

```markdown
---
granola_id: doc_123456
title: "Team Meeting Notes"
created_at: 2024-01-15T10:30:00Z
updated_at: 2024-01-15T11:00:00Z
has_transcript: true
---

# Team Meeting Notes

[Your formatted notes content here]

---

## Transcript

**John Smith** [01:23]: Let's start with the quarterly review.

**Sarah Johnson** [01:45]: The numbers look good for Q3.
```

## Credentials

The tool automatically loads credentials from Granola's configuration:
- **Encrypted (preferred)**: `~/Library/Application Support/Granola/supabase.json.enc`, decrypted using the `Granola Safe Storage` macOS Keychain entry. The first run will prompt for keychain access; click "Always Allow" to silence subsequent prompts.
- **Plaintext (legacy fallback)**: `~/Library/Application Support/Granola/supabase.json`, used only if the encrypted file is missing or unreadable.
- **Required**: Active Granola desktop app installation, signed in.
- **No manual configuration needed.**
- **Supported token formats**: `workos_tokens` (current) and `cognito_tokens` (legacy).

## Logging

The tool logs to both stderr and `granola_sync.log` including:
- Sync statistics (created, updated, skipped counts)
- Document processing details
- API response information
- Error messages and debugging info

## Troubleshooting

### Common Issues

1. **"Credentials file not found"**
   - Ensure Granola desktop app is installed
   - Check that you're logged into Granola

2. **"No transcript available"**
   - Not all documents have transcripts (this is normal)
   - Only meeting recordings generate transcripts

3. **"No access token found" / "access token has expired"**
   - Open the Granola desktop app — it will refresh credentials automatically and re-write `supabase.json.enc`.
   - If the keychain prompt was denied, re-run and click "Always Allow" so the tool can read the `Granola Safe Storage` entry.
   - Last resort: sign out and back in to Granola desktop.

4. **"Skipping document ... no suitable content found"**
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
