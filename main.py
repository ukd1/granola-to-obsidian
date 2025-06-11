import argparse
import logging
from pathlib import Path
import traceback
import json
import os
import requests
from datetime import datetime

# Configure logging
logging.basicConfig(
    level=logging.DEBUG,
    format='%(asctime)s - %(levelname)s - %(message)s',
    handlers=[
        logging.FileHandler('granola_sync.log'),
        logging.StreamHandler()
    ]
)
logger = logging.getLogger(__name__)

def load_credentials():
    """
    Load Granola credentials from supabase.json
    """
    creds_path = Path.home() / "Library/Application Support/Granola/supabase.json"
    if not creds_path.exists():
        logger.error(f"Credentials file not found at: {creds_path}")
        return None
    
    try:
        with open(creds_path, 'r') as f:
            data = json.load(f)
            
        # Parse the cognito_tokens string into a dict
        cognito_tokens = json.loads(data['cognito_tokens'])
        access_token = cognito_tokens.get('access_token')
        
        if not access_token:
            logger.error("No access token found in credentials file")
            return None
            
        logger.debug("Successfully loaded credentials")
        return access_token
    except Exception as e:
        logger.error(f"Error reading credentials file: {str(e)}")
        return None

def fetch_granola_documents(token):
    """
    Fetch documents from Granola API
    """
    url = "https://api.granola.ai/v2/get-documents"
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
        "Accept": "*/*",
        "User-Agent": "Granola/5.354.0",
        "X-Client-Version": "5.354.0"
    }
    data = {
        "limit": 100,
        "offset": 0,
        "include_last_viewed_panel": True
    }
    
    try:
        response = requests.post(url, headers=headers, json=data)
        response.raise_for_status()
        return response.json()
    except Exception as e:
        logger.error(f"Error fetching documents: {str(e)}")
        return None

def fetch_document_transcript(token, doc_id):
    """
    Fetch transcript for a specific document from Granola API
    Returns a List[TranscriptSegment] as per API specification
    """
    url = f"https://api.granola.ai/v1/get-document-transcript"
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
        "Accept": "*/*",
        "User-Agent": "Granola/5.354.0",
        "X-Client-Version": "5.354.0"
    }
    data = {
        "document_id": doc_id
    }
    
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
            return format_transcript_segments(transcript_data)
        else:
            logger.error(f"Unexpected transcript format for document {doc_id} - expected list, got {type(transcript_data)}")
            logger.debug(f"Full response: {transcript_data}")
            return None
            
    except requests.exceptions.HTTPError as e:
        if e.response.status_code == 404:
            logger.debug(f"No transcript available for document {doc_id}")
            return None
        else:
            logger.error(f"HTTP error fetching transcript for document {doc_id}: {str(e)}")
            return None
    except Exception as e:
        logger.error(f"Error fetching transcript for document {doc_id}: {str(e)}")
        return None

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
        text = segment.get('text', segment.get('content', ''))
        speaker = segment.get('speaker', segment.get('speaker_name', ''))
        start_time = segment.get('start_timestamp', segment.get('startTimestamp', ''))
        end_time = segment.get('end_timestamp', segment.get('endTimestamp', ''))
        
        if not text:
            continue
            
        # Format the segment based on available information
        if speaker and start_time:
            # Format time if it's a number (seconds) vs string
            if isinstance(start_time, (int, float)):
                time_str = f"{int(start_time//60):02d}:{int(start_time%60):02d}"
            else:
                time_str = str(start_time)
            formatted_lines.append(f"**{speaker}** [{time_str}]: {text}")
        elif speaker:
            formatted_lines.append(f"**{speaker}**: {text}")
        elif start_time:
            if isinstance(start_time, (int, float)):
                time_str = f"{int(start_time//60):02d}:{int(start_time%60):02d}"
            else:
                time_str = str(start_time)
            formatted_lines.append(f"[{time_str}]: {text}")
        else:
            formatted_lines.append(text)
    
    return '\n\n'.join(formatted_lines)

def convert_prosemirror_to_markdown(content):
    """
    Convert ProseMirror JSON to Markdown
    """
    if not content or not isinstance(content, dict) or 'content' not in content:
        return ""
        
    markdown = []
    
    def process_node(node):
        if not isinstance(node, dict):
            return ""
            
        node_type = node.get('type', '')
        content = node.get('content', [])
        text = node.get('text', '')
        
        if node_type == 'heading':
            level = node.get('attrs', {}).get('level', 1)
            heading_text = ''.join(process_node(child) for child in content)
            return f"{'#' * level} {heading_text}\n\n"
            
        elif node_type == 'paragraph':
            para_text = ''.join(process_node(child) for child in content)
            return f"{para_text}\n\n"
            
        elif node_type == 'bulletList':
            items = []
            for item in content:
                if item.get('type') == 'listItem':
                    item_content = ''.join(process_node(child) for child in item.get('content', []))
                    items.append(f"- {item_content.strip()}")
            return '\n'.join(items) + '\n\n'
            
        elif node_type == 'text':
            return text
            
        return ''.join(process_node(child) for child in content)
    
    return process_node(content)

def sanitize_filename(title):
    """
    Convert a title to a valid filename
    """
    # Remove invalid characters
    invalid_chars = '<>:"/\\|?*'
    filename = ''.join(c for c in title if c not in invalid_chars)
    # Replace spaces with underscores
    filename = filename.replace(' ', '_')
    return filename

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
        dt = datetime.fromisoformat(date_str.replace('Z', '+00:00'))
        return dt.strftime("%Y-%m-%d")
    except (ValueError, AttributeError):
        logger.warning(f"Could not parse date '{date_str}', using current date")
        return datetime.now().strftime("%Y-%m-%d")

def main():
    logger.info("Starting Granola sync process")
    parser = argparse.ArgumentParser(description="Fetch Granola notes and save them as Markdown files in an Obsidian folder.")
    parser.add_argument("output_dir", type=str, help="The full path to the Obsidian subfolder where notes should be saved.")
    args = parser.parse_args()

    output_path = Path(args.output_dir)
    logger.info(f"Output directory set to: {output_path}")
    
    if not output_path.is_dir():
        logger.error(f"Output directory '{output_path}' does not exist or is not a directory.")
        logger.error("Please create it first.")
        return

    logger.info("Attempting to load credentials...")
    token = load_credentials()
    if not token:
        logger.error("Failed to load credentials. Exiting.")
        return

    logger.info("Credentials loaded successfully. Fetching documents from Granola API...")
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

    synced_count = 0
    for doc in documents:
        title = doc.get("title", "Untitled Granola Note")
        doc_id = doc.get("id", "unknown_id")
        logger.info(f"Processing document: {title} (ID: {doc_id})")
        
        content_to_parse = None
        if doc.get("last_viewed_panel") and \
           isinstance(doc["last_viewed_panel"], dict) and \
           doc["last_viewed_panel"].get("content") and \
           isinstance(doc["last_viewed_panel"]["content"], dict) and \
           doc["last_viewed_panel"]["content"].get("type") == "doc":
            content_to_parse = doc["last_viewed_panel"]["content"]
            logger.debug(f"Found content to parse for document: {title}")

        if not content_to_parse:
            logger.warning(f"Skipping document '{title}' (ID: {doc_id}) - no suitable content found in 'last_viewed_panel'")
            continue
        
        try:
            logger.debug(f"Converting document to markdown: {title}")
            markdown_content = convert_prosemirror_to_markdown(content_to_parse)
            
            # Fetch transcript for this document
            transcript = fetch_document_transcript(token, doc_id)
            
            # Add a frontmatter block for metadata
            frontmatter = f"---\n"
            frontmatter += f"granola_id: {doc_id}\n"
            escaped_title_for_yaml = title.replace('"', '\\"') 
            frontmatter += f'title: "{escaped_title_for_yaml}"\n'
            
            if doc.get("created_at"):
                frontmatter += f"created_at: {doc.get('created_at')}\n"
            if doc.get("updated_at"):
                frontmatter += f"updated_at: {doc.get('updated_at')}\n"
            
            # Add transcript availability to frontmatter
            frontmatter += f"has_transcript: {'true' if transcript else 'false'}\n"
            frontmatter += f"---\n\n"
            
            # Build the final markdown content
            final_markdown = frontmatter + markdown_content
            
            # Add transcript section if available
            if transcript:
                logger.debug(f"Adding transcript section for document: {title}")
                final_markdown += "\n\n---\n\n## 📝 Full Transcript\n\n"
                final_markdown += transcript.strip()

            # Generate filename with date prefix
            date_prefix = extract_date_from_doc(doc)
            sanitized_title = sanitize_filename(title)
            filename = f"{date_prefix} - {sanitized_title}.md"
            filepath = output_path / filename

            logger.debug(f"Writing file to: {filepath}")
            with open(filepath, 'w', encoding='utf-8') as f:
                f.write(final_markdown)
            logger.info(f"Successfully saved: {filepath}")
            synced_count += 1
        except Exception as e:
            logger.error(f"Error processing document '{title}' (ID: {doc_id}): {str(e)}")
            logger.debug("Full traceback:", exc_info=True)

    logger.info(f"Sync complete. {synced_count} notes saved to '{output_path}'")

if __name__ == "__main__":
    main()