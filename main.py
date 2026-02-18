import argparse
import json
import logging
import os
from datetime import datetime
from pathlib import Path

import requests


def get_log_level():
    """
    Resolve LOG_LEVEL env var to a valid logging level.
    Accepts names like DEBUG/INFO/WARNING/ERROR/CRITICAL.
    """
    raw_value = os.getenv("LOG_LEVEL", "DEBUG").strip()
    normalized = raw_value.upper()
    level = getattr(logging, normalized, None)
    if isinstance(level, int):
        return level, normalized
    return logging.DEBUG, normalized


resolved_log_level, requested_log_level = get_log_level()

# Configure logging
logging.basicConfig(
    level=resolved_log_level,
    format="%(asctime)s - %(levelname)s - %(message)s",
    handlers=[logging.FileHandler("granola_sync.log"), logging.StreamHandler()],
)
logger = logging.getLogger(__name__)

if resolved_log_level == logging.DEBUG and requested_log_level != "DEBUG":
    logger.warning(
        f"Invalid LOG_LEVEL '{requested_log_level}'. Falling back to DEBUG."
    )


def extract_access_token(data):
    """
    Extract access token from known Granola credential shapes.
    Supports legacy `cognito_tokens` and newer `workos_tokens`.
    """
    for key in ("workos_tokens", "cognito_tokens"):
        token_payload = data.get(key)
        if not token_payload:
            continue

        try:
            token_data = (
                json.loads(token_payload)
                if isinstance(token_payload, str)
                else token_payload
            )
        except json.JSONDecodeError:
            logger.warning(f"Could not parse token payload from '{key}'")
            continue

        if isinstance(token_data, dict):
            access_token = token_data.get("access_token")
            if access_token:
                logger.debug(f"Loaded access token from '{key}'")
                return access_token

    # Fallback if a future schema stores token at the top level.
    access_token = data.get("access_token")
    if access_token:
        logger.debug("Loaded access token from top-level field")
        return access_token

    return None


def load_credentials():
    """
    Load Granola credentials from supabase.json
    """
    creds_path = Path.home() / "Library/Application Support/Granola/supabase.json"
    if not creds_path.exists():
        logger.error(f"Credentials file not found at: {creds_path}")
        return None

    try:
        with open(creds_path, "r") as f:
            data = json.load(f)

        access_token = extract_access_token(data)

        if not access_token:
            available_keys = ", ".join(sorted(data.keys()))
            logger.error(
                f"No access token found in credentials file. Available keys: {available_keys}"
            )
            return None

        logger.debug("Successfully loaded credentials")
        return access_token
    except Exception as e:
        logger.error(f"Error reading credentials file: {str(e)}")
        return None


def fetch_granola_documents(token):
    """
    Fetch all documents from Granola API with pagination.
    """
    url = "https://api.granola.ai/v2/get-documents"
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
        "Accept": "*/*",
        "User-Agent": "Granola/5.354.0",
        "X-Client-Version": "5.354.0",
    }
    page_size = 100
    max_pages = 1000
    offset = 0
    page_index = 0
    all_docs = []
    all_deleted = []
    seen_doc_ids = set()
    seen_deleted_ids = set()

    try:
        while page_index < max_pages:
            data = {
                "limit": page_size,
                "offset": offset,
                "include_last_viewed_panel": True,
            }
            response = requests.post(url, headers=headers, json=data)
            response.raise_for_status()
            payload = response.json()

            if not isinstance(payload, dict):
                logger.error(
                    f"Unexpected documents payload type: {type(payload)} at offset {offset}"
                )
                return None

            page_docs = payload.get("docs")
            if not isinstance(page_docs, list):
                logger.error(
                    f"Unexpected 'docs' type: {type(page_docs)} at offset {offset}"
                )
                return None

            page_deleted = payload.get("deleted", [])
            if not isinstance(page_deleted, list):
                page_deleted = []

            new_docs_in_page = 0
            for doc in page_docs:
                if not isinstance(doc, dict):
                    continue

                doc_id = doc.get("id")
                if doc_id and doc_id in seen_doc_ids:
                    continue

                if doc_id:
                    seen_doc_ids.add(doc_id)
                all_docs.append(doc)
                new_docs_in_page += 1

            for deleted_id in page_deleted:
                deleted_key = str(deleted_id)
                if deleted_key in seen_deleted_ids:
                    continue
                seen_deleted_ids.add(deleted_key)
                all_deleted.append(deleted_id)

            logger.debug(
                f"Fetched page {page_index + 1}: {len(page_docs)} docs ({new_docs_in_page} new), offset={offset}"
            )

            if not page_docs:
                break

            if len(page_docs) < page_size:
                break

            if new_docs_in_page == 0:
                logger.warning(
                    "Pagination yielded no new docs on a full page; stopping to avoid loop"
                )
                break

            offset += page_size
            page_index += 1

        if page_index >= max_pages:
            logger.warning(f"Reached pagination safety limit ({max_pages} pages)")

        return {"docs": all_docs, "deleted": all_deleted}
    except Exception as e:
        logger.error(f"Error fetching documents: {str(e)}")
        return None


def fetch_document_transcript(token, doc_id):
    """
    Fetch transcript for a specific document from Granola API
    Returns (formatted_markdown: str | None, transcript_data: list | None)
    """
    url = f"https://api.granola.ai/v1/get-document-transcript"
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
        "Accept": "*/*",
        "User-Agent": "Granola/5.354.0",
        "X-Client-Version": "5.354.0",
    }
    data = {"document_id": doc_id}

    try:
        logger.debug(f"Fetching transcript for document ID: {doc_id}")
        response = requests.post(url, headers=headers, json=data)
        response.raise_for_status()
        transcript_data = response.json()

        # Debug: Log the actual response structure
        logger.debug(f"Transcript response type: {type(transcript_data)}")
        if isinstance(transcript_data, list):
            logger.debug(f"Transcript segments count: {len(transcript_data)}")

        # API should return List[TranscriptSegment]
        if isinstance(transcript_data, list):
            return format_transcript_segments(transcript_data), transcript_data
        else:
            logger.error(
                f"Unexpected transcript format for document {doc_id} - expected list, got {type(transcript_data)}"
            )
            logger.debug(f"Full response: {transcript_data}")
            return None, None

    except requests.exceptions.HTTPError as e:
        if e.response.status_code == 404:
            logger.debug(f"No transcript available for document {doc_id}")
            return None, None
        else:
            logger.error(
                f"HTTP error fetching transcript for document {doc_id}: {str(e)}"
            )
            return None, None
    except Exception as e:
        logger.error(f"Error fetching transcript for document {doc_id}: {str(e)}")
        return None, None


def format_transcript_segments(segments):
    """
    Format a list of TranscriptSegment objects into readable markdown text
    Based on the TranscriptSegment type definition from granola-py-client
    """
    if not segments or not isinstance(segments, list):
        return ""

    formatted_lines = []

    for segment in segments:
        if not isinstance(segment, dict):
            continue

        # Extract fields from TranscriptSegment - update these based on actual type definition
        text = segment.get("text", segment.get("content", ""))
        speaker = segment.get("speaker", segment.get("speaker_name", ""))
        start_time = segment.get("start_timestamp", segment.get("startTimestamp", ""))
        end_time = segment.get("end_timestamp", segment.get("endTimestamp", ""))

        if not text:
            continue

        # Format the segment based on available information
        if speaker and start_time:
            # Format time if it's a number (seconds) vs string
            if isinstance(start_time, (int, float)):
                time_str = f"{int(start_time // 60):02d}:{int(start_time % 60):02d}"
            else:
                time_str = str(start_time)
            formatted_lines.append(f"**{speaker}** [{time_str}]: {text}")
        elif speaker:
            formatted_lines.append(f"**{speaker}**: {text}")
        elif start_time:
            if isinstance(start_time, (int, float)):
                time_str = f"{int(start_time // 60):02d}:{int(start_time % 60):02d}"
            else:
                time_str = str(start_time)
            formatted_lines.append(f"[{time_str}]: {text}")
        else:
            formatted_lines.append(text)

    return "\n\n".join(formatted_lines)


def convert_prosemirror_to_markdown(content):
    """
    Convert ProseMirror JSON to Markdown
    """
    if not content or not isinstance(content, dict) or "content" not in content:
        return ""

    markdown = []

    def process_node(node):
        if not isinstance(node, dict):
            return ""

        node_type = node.get("type", "")
        content = node.get("content", [])
        text = node.get("text", "")

        if node_type == "heading":
            level = node.get("attrs", {}).get("level", 1)
            heading_text = "".join(process_node(child) for child in content)
            return f"{'#' * level} {heading_text}\n\n"

        elif node_type == "paragraph":
            para_text = "".join(process_node(child) for child in content)
            return f"{para_text}\n\n"

        elif node_type == "bulletList":
            items = []
            for item in content:
                if item.get("type") == "listItem":
                    item_content = "".join(
                        process_node(child) for child in item.get("content", [])
                    )
                    items.append(f"- {item_content.strip()}")
            return "\n".join(items) + "\n\n"

        elif node_type == "text":
            return text

        return "".join(process_node(child) for child in content)

    return process_node(content)


def normalize_title(title):
    """
    Return a safe, non-empty title string for filenames/frontmatter.
    """
    if isinstance(title, str):
        normalized = title.strip()
    elif title is None:
        normalized = ""
    else:
        normalized = str(title).strip()

    return normalized or "Untitled Granola Note"


def sanitize_filename(title):
    """
    Convert a title to a valid filename
    """
    # Remove invalid characters
    invalid_chars = '<>:"/\\|?*'
    safe_title = normalize_title(title)
    filename = "".join(c for c in safe_title if c not in invalid_chars)
    return filename or "Untitled Granola Note"


def extract_date_from_doc(doc):
    """
    Extract and format date from document for filename prefix
    """
    # Try created_at first, then updated_at as fallback
    date_str = doc.get("created_at") or doc.get("updated_at")

    if not date_str:
        # If no date available, use current date
        return datetime.now().strftime("%Y-%m-%d")

    try:
        # Parse the ISO date string and format as YYYY-MM-DD
        # Granola dates are typically in ISO format like "2024-01-15T10:30:00Z"
        dt = datetime.fromisoformat(date_str.replace("Z", "+00:00"))
        return dt.strftime("%Y-%m-%d")
    except (ValueError, AttributeError):
        logger.warning(f"Could not parse date '{date_str}', using current date")
        return datetime.now().strftime("%Y-%m-%d")


def build_output_relative_path(date_prefix, filename):
    """
    Build output path as YYYY/MM/DD/<existing filename>.
    """
    try:
        year, month, day = date_prefix.split("-")
    except ValueError:
        logger.warning(
            f"Unexpected date prefix '{date_prefix}', falling back to unsorted root path"
        )
        return Path(filename)

    return Path(year) / month / day / filename


def parse_frontmatter_file(filepath):
    """
    Parse simple YAML frontmatter from a markdown file.
    This supports scalar `key: value` pairs used by this script.
    """
    try:
        with open(filepath, "r", encoding="utf-8") as f:
            lines = f.read().splitlines()
    except Exception as e:
        logger.warning(f"Could not read markdown file '{filepath}': {str(e)}")
        return {}

    if len(lines) < 3 or lines[0].strip() != "---":
        return {}

    frontmatter = {}
    for line in lines[1:]:
        stripped = line.strip()
        if stripped == "---":
            break
        if ":" not in line:
            continue

        key, value = line.split(":", 1)
        key = key.strip()
        value = value.strip()
        if not key:
            continue

        if len(value) >= 2 and (
            (value[0] == '"' and value[-1] == '"')
            or (value[0] == "'" and value[-1] == "'")
        ):
            value = value[1:-1]

        frontmatter[key] = value

    return frontmatter


def build_local_note_index(output_path):
    """
    Build an in-memory index of existing notes by granola_id from frontmatter.
    """
    notes_by_id = {}
    duplicate_ids = set()

    for filepath in output_path.rglob("*.md"):
        frontmatter = parse_frontmatter_file(filepath)
        granola_id = str(frontmatter.get("granola_id", "")).strip()
        if not granola_id:
            continue

        note_entry = {
            "local_filename": filepath.relative_to(output_path).as_posix(),
            "granola_updated_at": frontmatter.get("updated_at")
            or frontmatter.get("created_at"),
            "path": filepath,
        }

        if granola_id in notes_by_id:
            duplicate_ids.add(granola_id)
            current_entry = notes_by_id[granola_id]
            try:
                use_new_entry = filepath.stat().st_mtime > current_entry["path"].stat().st_mtime
            except OSError:
                use_new_entry = False

            if use_new_entry:
                notes_by_id[granola_id] = note_entry

            logger.warning(
                f"Duplicate granola_id '{granola_id}' found in local notes. Using most recently modified file."
            )
            continue

        notes_by_id[granola_id] = note_entry

    if duplicate_ids:
        logger.warning(
            f"Found {len(duplicate_ids)} duplicate granola_id values in local markdown files."
        )

    logger.info(f"Indexed {len(notes_by_id)} existing notes by granola_id")
    return notes_by_id


def build_generated_note_relative_path(doc):
    """
    Build the default generated relative note path for a Granola document.
    """
    title = normalize_title(doc.get("title"))
    date_prefix = extract_date_from_doc(doc)
    sanitized_title = sanitize_filename(title)
    filename = f"{date_prefix} - {sanitized_title}.md"
    return build_output_relative_path(date_prefix, filename)


def build_transcript_file_index(output_path, documents, local_notes_by_id):
    """
    Build a mapping of document ID -> transcript sidecar path.
    Preference order:
    1) sidecar next to the currently indexed local note path
    2) sidecar at the script's generated default path for this doc
    """
    transcript_files_by_id = {}
    matched_transcript_paths = set()
    all_transcript_paths = set(output_path.rglob("*.transcript.json"))

    for doc in documents:
        doc_id = doc.get("id")
        if not doc_id:
            continue

        candidate_paths = []
        local_note = local_notes_by_id.get(doc_id)
        if local_note:
            local_note_path = output_path / Path(local_note["local_filename"])
            candidate_paths.append(local_note_path.with_suffix(".transcript.json"))

        generated_note_path = output_path / build_generated_note_relative_path(doc)
        candidate_paths.append(generated_note_path.with_suffix(".transcript.json"))

        existing_candidates = []
        seen = set()
        for candidate in candidate_paths:
            candidate_key = candidate.as_posix()
            if candidate_key in seen:
                continue
            seen.add(candidate_key)
            if candidate.exists():
                existing_candidates.append(candidate)

        if not existing_candidates:
            continue

        selected_path = existing_candidates[0]
        if len(existing_candidates) > 1:
            logger.warning(
                f"Multiple transcript sidecars found for document ID {doc_id}. Using {selected_path}"
            )

        transcript_files_by_id[doc_id] = selected_path
        matched_transcript_paths.add(selected_path)

    unmatched_transcript_count = len(all_transcript_paths - matched_transcript_paths)
    if unmatched_transcript_count:
        logger.warning(
            f"Found {unmatched_transcript_count} transcript sidecar files that could not be matched to a document ID"
        )

    logger.info(
        f"Indexed {len(transcript_files_by_id)} transcript sidecar files by document ID"
    )
    return transcript_files_by_id


def reconcile_transcript_paths(
    output_path, local_notes_by_id, transcript_files_by_id, dry_run=False
):
    """
    Ensure each note's transcript sidecar path matches the note's current filename.
    Renames sidecars when a note has been renamed locally.
    """
    renamed_count = 0

    for doc_id, note_entry in local_notes_by_id.items():
        actual_transcript_path = transcript_files_by_id.get(doc_id)
        if not actual_transcript_path:
            continue

        expected_note_path = output_path / Path(note_entry["local_filename"])
        expected_transcript_path = expected_note_path.with_suffix(".transcript.json")
        if actual_transcript_path == expected_transcript_path:
            continue

        if expected_transcript_path.exists():
            logger.warning(
                f"Expected transcript path already exists for document ID {doc_id}; leaving existing file at {actual_transcript_path}"
            )
            continue

        if dry_run:
            logger.info(
                f"[DRY RUN] Would rename transcript sidecar for document ID {doc_id}: {actual_transcript_path} -> {expected_transcript_path}"
            )
            renamed_count += 1
            continue

        try:
            expected_transcript_path.parent.mkdir(parents=True, exist_ok=True)
            actual_transcript_path.rename(expected_transcript_path)
            transcript_files_by_id[doc_id] = expected_transcript_path
            logger.info(
                f"Renamed transcript sidecar for document ID {doc_id}: {actual_transcript_path} -> {expected_transcript_path}"
            )
            renamed_count += 1
        except Exception as e:
            logger.warning(
                f"Could not rename transcript sidecar for document ID {doc_id}: {str(e)}"
            )

    if renamed_count:
        logger.info(
            f"{'[DRY RUN] ' if dry_run else ''}Transcript sidecar rename checks: {renamed_count} {'would be ' if dry_run else ''}renamed"
        )

    return renamed_count


def needs_sync(doc, local_notes_by_id, force_full_sync=False):
    """
    Determine if a document needs to be synced
    Returns (needs_sync: bool, reason: str)
    """
    if force_full_sync:
        return True, "full-sync requested"

    doc_id = doc.get("id")
    if not doc_id:
        return True, "missing document ID"

    # Check if document exists locally by granola_id frontmatter
    if doc_id not in local_notes_by_id:
        return True, "new document"

    doc_state = local_notes_by_id[doc_id]
    granola_updated_at = doc.get("updated_at") or doc.get("created_at")
    local_updated_at = doc_state.get("granola_updated_at")

    if not granola_updated_at:
        return True, "no timestamp from Granola"

    if not local_updated_at:
        return True, "local note missing updated_at frontmatter"

    # Compare timestamps (both should be ISO format strings)
    if granola_updated_at > local_updated_at:
        return (
            True,
            f"document updated ({granola_updated_at} > {local_updated_at})",
        )

    return False, "document unchanged"


def main():
    logger.info("Starting Granola sync process")
    parser = argparse.ArgumentParser(
        description="Fetch Granola notes and save them as Markdown files in an Obsidian folder."
    )
    parser.add_argument(
        "output_dir",
        type=str,
        help="The full path to the Obsidian subfolder where notes should be saved.",
    )
    parser.add_argument(
        "--full-sync",
        action="store_true",
        help="Force sync all documents (ignore timestamps)",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Show what would be synced without making changes",
    )
    parser.add_argument(
        "--clean",
        action="store_true",
        help="Remove local files for documents deleted from Granola (not implemented yet)",
    )
    args = parser.parse_args()

    output_path = Path(args.output_dir)
    logger.info(f"Output directory set to: {output_path}")

    if not output_path.is_dir():
        logger.error(
            f"Output directory '{output_path}' does not exist or is not a directory."
        )
        logger.error("Please create it first.")
        return

    local_notes_by_id = build_local_note_index(output_path)

    if args.dry_run:
        logger.info("DRY RUN MODE - No files will be created or modified")

    if args.full_sync:
        logger.info(
            "FULL SYNC MODE - All documents will be synced regardless of timestamps"
        )

    logger.info("Attempting to load credentials...")
    token = load_credentials()
    if not token:
        logger.error("Failed to load credentials. Exiting.")
        return

    logger.info(
        "Credentials loaded successfully. Fetching documents from Granola API..."
    )
    api_response = fetch_granola_documents(token)

    if not api_response:
        logger.error("Failed to fetch documents - API response is empty")
        return

    if "docs" not in api_response:
        logger.error("API response format is unexpected - 'docs' key not found")
        logger.debug(f"API Response: {api_response}")
        return

    documents = api_response["docs"]
    logger.info(f"Successfully fetched {len(documents)} documents from Granola")

    transcript_files_by_id = build_transcript_file_index(
        output_path, documents, local_notes_by_id
    )
    reconcile_transcript_paths(
        output_path, local_notes_by_id, transcript_files_by_id, args.dry_run
    )

    # Process documents with incremental sync logic
    synced_count = 0
    updated_count = 0
    skipped_count = 0

    for doc in documents:
        title = normalize_title(doc.get("title"))
        doc_id = doc.get("id")
        display_doc_id = doc_id or "unknown_id"

        # Check if document needs syncing
        should_sync, reason = needs_sync(doc, local_notes_by_id, args.full_sync)

        if not should_sync:
            logger.debug(
                f"Skipping document '{title}' (ID: {display_doc_id}) - {reason}"
            )
            skipped_count += 1
            continue

        logger.info(
            f"Processing document: {title} (ID: {display_doc_id}) - {reason}"
        )

        content_to_parse = None
        if (
            doc.get("last_viewed_panel")
            and isinstance(doc["last_viewed_panel"], dict)
            and doc["last_viewed_panel"].get("content")
            and isinstance(doc["last_viewed_panel"]["content"], dict)
            and doc["last_viewed_panel"]["content"].get("type") == "doc"
        ):
            content_to_parse = doc["last_viewed_panel"]["content"]
            logger.debug(f"Found content to parse for document: {title}")

        try:
            existing_note = local_notes_by_id.get(doc_id) if doc_id else None

            if existing_note:
                local_relative_filename = existing_note["local_filename"]
                filepath = output_path / Path(local_relative_filename)
            else:
                local_relative_path = build_generated_note_relative_path(doc)
                local_relative_filename = local_relative_path.as_posix()
                filepath = output_path / local_relative_path

            transcript_json_path = filepath.with_suffix(".transcript.json")
            is_update = existing_note is not None

            if args.dry_run:
                if not content_to_parse:
                    transcript_markdown, transcript_data = (
                        fetch_document_transcript(token, doc_id)
                        if doc_id
                        else (None, None)
                    )
                    if not transcript_data:
                        logger.warning(
                            f"[DRY RUN] Would SKIP document '{title}' (ID: {display_doc_id}) - no suitable content found in 'last_viewed_panel' and no transcript available"
                        )
                        skipped_count += 1
                        continue

                action = "UPDATE" if is_update else "CREATE"
                if content_to_parse:
                    logger.info(f"[DRY RUN] Would {action}: {local_relative_filename}")
                else:
                    logger.info(
                        f"[DRY RUN] Would {action} (transcript-only): {local_relative_filename}"
                    )
                if is_update:
                    updated_count += 1
                else:
                    synced_count += 1
                continue

            if content_to_parse:
                logger.debug(f"Converting document to markdown: {title}")
                markdown_content = convert_prosemirror_to_markdown(content_to_parse)
            else:
                logger.info(
                    f"No suitable content found in 'last_viewed_panel' for '{title}' (ID: {display_doc_id}); attempting transcript-only export"
                )
                markdown_content = ""

            # Fetch transcript for this document
            transcript_markdown, transcript_data = (
                fetch_document_transcript(token, doc_id) if doc_id else (None, None)
            )

            if not content_to_parse and not transcript_data:
                logger.warning(
                    f"Skipping document '{title}' (ID: {display_doc_id}) - no suitable content found in 'last_viewed_panel' and no transcript available"
                )
                skipped_count += 1
                continue

            # Add a frontmatter block for metadata
            frontmatter = f"---\n"
            frontmatter += f"granola_id: {doc_id or ''}\n"
            escaped_title_for_yaml = title.replace('"', '\\"')
            frontmatter += f'title: "{escaped_title_for_yaml}"\n'

            if doc.get("created_at"):
                frontmatter += f"created_at: {doc.get('created_at')}\n"
            if doc.get("updated_at"):
                frontmatter += f"updated_at: {doc.get('updated_at')}\n"

            # Add transcript availability to frontmatter
            frontmatter += (
                f"has_transcript: {'true' if transcript_data else 'false'}\n"
            )
            frontmatter += f"---\n\n"

            # Build the final markdown content
            final_markdown = frontmatter
            if markdown_content:
                final_markdown += markdown_content
            else:
                final_markdown += f"# {title}\n\n"
                final_markdown += (
                    "_Transcript-only export (note body unavailable from Granola API)._"
                )

            # Add transcript section if available
            if transcript_markdown:
                logger.debug(f"Adding transcript section for document: {title}")
                if markdown_content:
                    final_markdown += "\n\n---\n\n## Transcript\n\n"
                else:
                    final_markdown += "\n\n## Transcript\n\n"
                final_markdown += transcript_markdown.strip()

            logger.debug(f"Writing file to: {filepath}")
            filepath.parent.mkdir(parents=True, exist_ok=True)
            with open(filepath, "w", encoding="utf-8") as f:
                f.write(final_markdown)

            if transcript_data is not None:
                logger.debug(f"Writing transcript sidecar to: {transcript_json_path}")
                with open(transcript_json_path, "w", encoding="utf-8") as f:
                    json.dump(transcript_data, f, indent=2, ensure_ascii=False)
            elif transcript_json_path.exists():
                logger.debug(f"Removing stale transcript sidecar: {transcript_json_path}")
                transcript_json_path.unlink()

            # Update in-memory local index so this run can make consistent decisions
            if doc_id:
                local_notes_by_id[doc_id] = {
                    "local_filename": local_relative_filename,
                    "granola_updated_at": doc.get("updated_at")
                    or doc.get("created_at"),
                    "path": filepath,
                }

            action = "Updated" if is_update else "Created"
            logger.info(f"{action}: {filepath}")

            if is_update:
                updated_count += 1
            else:
                synced_count += 1

        except Exception as e:
            logger.error(
                f"Error processing document '{title}' (ID: {display_doc_id}): {str(e)}"
            )
            logger.debug("Full traceback:", exc_info=True)

    # Report sync statistics
    total_processed = synced_count + updated_count
    logger.info(
        f"Sync complete! Created: {synced_count}, Updated: {updated_count}, Skipped: {skipped_count}, Total processed: {total_processed}"
    )

    if args.dry_run:
        logger.info("DRY RUN - No actual changes were made")


if __name__ == "__main__":
    main()
