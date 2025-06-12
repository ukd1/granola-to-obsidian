# Granola to Obsidian Sync

A Python script that syncs notes and transcripts from Granola to your Obsidian vault. Supports incremental sync, only processing new or updated documents on subsequent runs.

Original script based on this article: https://josephthacker.com/hacking/2025/05/08/reverse-engineering-granola-notes.html


## Features

- 🔄 **Incremental Sync**: Only syncs new or updated documents
- 📝 **Full Transcript Support**: Includes meeting transcripts in formatted sections
- 📅 **Date Prefixed Filenames**: Files named as "YYYY-MM-DD - [title].md"
- 🎯 **Smart State Tracking**: Remembers what's been synced to avoid duplicates
- 🔍 **Dry Run Mode**: Preview changes before syncing
- 📊 **Detailed Logging**: Clear reporting of what was synced, updated, or skipped

## Prerequisites

- Python 3.7+
- Active Granola account with documents
- Granola desktop app installed (for credentials)

## Setup

### 1. Clone or Download

```bash
git clone <repository-url>
cd granola2obsidian
```

### 2. Automated Setup

Run the setup script to create a virtual environment and install dependencies:

```bash
./setup.sh
```

### 3. Manual Setup (Alternative)

```bash
# Create virtual environment
python3 -m venv venv

# Activate virtual environment
source venv/bin/activate

# Install dependencies
pip install -r requirements.txt
```

## Testing

### Test Transcript API

Use the included test script to verify your setup and see transcript data structure:

```bash
# Activate virtual environment
source venv/bin/activate

# Run transcript API test
python test_transcript.py
```

This will:
- Load your Granola credentials
- Fetch your recent documents
- Test the transcript API with sample data
- Show you the exact format of transcript segments

## Usage

### Basic Sync

```bash
# Activate virtual environment (if not already active)
source venv/bin/activate

# Sync to your Obsidian folder
python main.py /path/to/your/obsidian/folder
```

### Command Line Options

```bash
python main.py [OPTIONS] OUTPUT_DIR
```

**Positional Arguments:**
- `OUTPUT_DIR`: Full path to your Obsidian subfolder where notes should be saved

**Options:**
- `--dry-run`: Preview what would be synced without making changes
- `--full-sync`: Force sync all documents (ignore timestamps)
- `--clean`: Remove local files for documents deleted from Granola *(not implemented yet)*

### Examples

```bash
# Daily incremental sync (recommended)
python main.py ~/Documents/MyVault/Granola_Notes

# Preview changes before syncing
python main.py --dry-run ~/Documents/MyVault/Granola_Notes

# Force sync all documents
python main.py --full-sync ~/Documents/MyVault/Granola_Notes
```

## Sync State Management

The script maintains a `.granola_sync_state.json` file in the current working directory to track:

- Document IDs and their last update timestamps
- Local filename mappings
- Transcript availability status
- Last sync run timestamp

### Sync Logic

- **New documents**: Synced automatically
- **Updated documents**: Only synced when Granola's `updated_at` timestamp is newer than last sync
- **Unchanged documents**: Skipped entirely (no API calls made)
- **Renamed documents**: Old files are automatically cleaned up

### Sample Sync State File

```json
{
  "last_sync_run": "2024-01-15T10:30:00.123456",
  "documents": {
    "doc_123": {
      "granola_updated_at": "2024-01-15T09:45:00Z",
      "last_synced_at": "2024-01-15T10:30:00.123456",
      "local_filename": "2024-01-15 - Team_Meeting.md",
      "has_transcript": true,
      "title": "Team Meeting"
    }
  }
}
```

## Output Format

### File Naming

Files are saved with date prefixes: `YYYY-MM-DD - [sanitized_title].md`

Examples:
- `2024-01-15 - Team_Meeting_Notes.md`
- `2024-01-16 - Project_Review.md`

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

## 📝 Full Transcript

**John Smith** [01:23]: Let's start with the quarterly review.

**Sarah Johnson** [01:45]: The numbers look good for Q3.
```

## Credentials

The script automatically loads credentials from Granola's configuration:
- **Location**: `~/Library/Application Support/Granola/supabase.json`
- **Required**: Active Granola desktop app installation
- **No manual configuration needed**

## Logging

The script creates detailed logs in `granola_sync.log` including:
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

3. **"Unexpected transcript format"**
   - Run the test script to see actual API response format
   - Check logs for detailed error information

### Debug Mode

For detailed debugging, check the log file:

```bash
tail -f granola_sync.log
```

### Reset Sync State

To force a complete re-sync of all documents:

```bash
# Delete sync state file
rm .granola_sync_state.json

# Run full sync
python main.py /path/to/your/obsidian/folder
```

## Daily Usage Workflow

1. **Set up once**: Install and test the script
2. **Daily run**: Execute `python main.py ~/path/to/vault/Granola_Notes`
3. **Check results**: Review the sync statistics in the output
4. **Open Obsidian**: Your new/updated notes are ready!

The incremental sync makes daily usage fast - typically only processing a few new documents rather than your entire Granola library.

## File Structure

```
granola2obsidian/
├── main.py                      # Main sync script
├── test_transcript.py          # Test script for API
├── setup.sh                   # Automated setup script
├── requirements.txt           # Python dependencies
├── .granola_sync_state.json  # Sync state (created after first run)
├── granola_sync.log          # Detailed logs
└── README.md                 # This file
```

## Contributing

Feel free to submit issues, feature requests, or pull requests to improve the script! 