#!/usr/bin/env python3
"""
Test script for Granola transcript API
"""

import json
import requests
from pathlib import Path
import logging

# Configure logging
logging.basicConfig(level=logging.INFO, format='%(levelname)s: %(message)s')
logger = logging.getLogger(__name__)

def load_credentials():
    """Load Granola credentials from supabase.json"""
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
            
        logger.info("✅ Successfully loaded credentials")
        return access_token
    except Exception as e:
        logger.error(f"Error reading credentials file: {str(e)}")
        return None

def fetch_recent_documents(token, limit=5):
    """Fetch recent documents to get document IDs for testing"""
    url = "https://api.granola.ai/v2/get-documents"
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
        "Accept": "*/*",
        "User-Agent": "Granola/5.354.0",
        "X-Client-Version": "5.354.0"
    }
    data = {
        "limit": limit,
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

def test_transcript_api(token, doc_id):
    """Test the transcript API with a specific document ID"""
    url = "https://api.granola.ai/v1/get-document-transcript"
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
        logger.info(f"🔍 Testing transcript API for document: {doc_id}")
        response = requests.post(url, headers=headers, json=data)
        response.raise_for_status()
        transcript_data = response.json()
        
        logger.info(f"✅ API Response received")
        logger.info(f"📊 Response type: {type(transcript_data)}")
        
        if isinstance(transcript_data, list):
            logger.info(f"📝 Number of transcript segments: {len(transcript_data)}")
            if transcript_data:
                # Test transcript segment parsing
                test_transcript_segment_parsing(transcript_data)
        else:
            logger.info(f"⚠️  Unexpected format - not a list")
            logger.info(f"📋 Full response: {transcript_data}")
            
        return transcript_data
        
    except requests.exceptions.HTTPError as e:
        if e.response.status_code == 404:
            logger.warning(f"❌ No transcript available for document {doc_id}")
            return None
        else:
            logger.error(f"❌ HTTP error: {e.response.status_code} - {str(e)}")
            return None
    except Exception as e:
        logger.error(f"❌ Error testing transcript API: {str(e)}")
        return None

def test_transcript_segment_parsing(transcript_data):
    """Test parsing of transcript segments and show field availability"""
    print("\n🧪 Testing Transcript Segment Parsing")
    print("-" * 40)
    
    if not transcript_data:
        print("❌ No transcript segments to test")
        return
        
    # Test first few segments
    test_segments = transcript_data[:3] if len(transcript_data) >= 3 else transcript_data
    
    for i, segment in enumerate(test_segments):
        print(f"\n📋 Segment {i+1}:")
        print(f"   🔍 All keys: {list(segment.keys())}")
        
        # Test different field name variations
        text_fields = ['text', 'content', 'transcript']
        speaker_fields = ['speaker', 'speaker_name', 'speakerName']
        time_fields = ['start_time', 'startTime', 'startTimestamp', 'start_timestamp', 'timestamp']
        end_time_fields = ['end_time', 'endTime', 'endTimestamp', 'end_timestamp']
        
        # Test text fields
        text_found = None
        for field in text_fields:
            if field in segment:
                text_found = field
                break
        print(f"   📝 Text field: {text_found} = '{segment.get(text_found, '')[:50]}{'...' if len(segment.get(text_found, '')) > 50 else ''}'")
        
        # Test speaker fields
        speaker_found = None
        for field in speaker_fields:
            if field in segment:
                speaker_found = field
                break
        print(f"   🎤 Speaker field: {speaker_found} = '{segment.get(speaker_found, 'N/A')}'")
        
        # Test start time fields
        start_time_found = None
        for field in time_fields:
            if field in segment:
                start_time_found = field
                break
        start_time_value = segment.get(start_time_found) if start_time_found else None
        print(f"   ⏰ Start time field: {start_time_found} = {start_time_value} (type: {type(start_time_value)})")
        
        # Test end time fields
        end_time_found = None
        for field in end_time_fields:
            if field in segment:
                end_time_found = field
                break
        end_time_value = segment.get(end_time_found) if end_time_found else None
        print(f"   ⏱️  End time field: {end_time_found} = {end_time_value} (type: {type(end_time_value)})")
        
        # Test our parsing logic
        print(f"   🔧 Parsed values:")
        text = segment.get('text', segment.get('content', ''))
        speaker = segment.get('speaker', segment.get('speaker_name', ''))
        start_time = segment.get('start_time', segment.get('startTimestamp', ''))
        end_time = segment.get('end_time', segment.get('endTimestamp', ''))
        
        print(f"      Text: '{text[:30]}{'...' if len(text) > 30 else ''}'")
        print(f"      Speaker: '{speaker}'")
        print(f"      Start time: {start_time} (type: {type(start_time)})")
        print(f"      End time: {end_time} (type: {type(end_time)})")
        
        # Test time formatting
        if start_time:
            if isinstance(start_time, (int, float)):
                formatted_time = f"{int(start_time//60):02d}:{int(start_time%60):02d}"
                print(f"      Formatted time: {formatted_time}")
            else:
                print(f"      Time as string: {start_time}")
    
    print(f"\n✅ Tested {len(test_segments)} transcript segments")
    
    # Test the format_transcript_segments function
    print(f"\n🎨 Testing format_transcript_segments function:")
    print("-" * 40)
    formatted = format_transcript_segments_test(test_segments)
    print(formatted[:200] + "..." if len(formatted) > 200 else formatted)

def format_transcript_segments_test(segments):
    """
    Test version of format_transcript_segments to see the output
    """
    if not segments or not isinstance(segments, list):
        return ""
        
    formatted_lines = []
    
    for segment in segments:
        if not isinstance(segment, dict):
            continue
            
        # Extract fields from TranscriptSegment - matching main.py logic
        text = segment.get('text', segment.get('content', ''))
        speaker = segment.get('speaker', segment.get('speaker_name', ''))
        start_time = segment.get('start_time', segment.get('startTimestamp', ''))
        end_time = segment.get('end_time', segment.get('endTimestamp', ''))
        
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

def main():
    print("🚀 Granola Transcript API Test Script")
    print("=" * 50)
    
    # Load credentials
    token = load_credentials()
    if not token:
        print("❌ Failed to load credentials. Exiting.")
        return
    
    # Fetch recent documents
    print("\n📄 Fetching recent documents...")
    api_response = fetch_recent_documents(token)
    if not api_response or "docs" not in api_response:
        print("❌ Failed to fetch documents. Exiting.")
        return
    
    documents = api_response["docs"]
    print(f"✅ Found {len(documents)} documents")
    
    # Show available documents
    print("\n📋 Available documents:")
    for i, doc in enumerate(documents):
        title = doc.get("title", "Untitled")
        doc_id = doc.get("id", "unknown")
        created = doc.get("created_at", "Unknown date")
        print(f"  {i+1}. {title} ({doc_id}) - {created}")
    
    if not documents:
        print("❌ No documents found. Exiting.")
        return
    
    # Test transcript API with the first document
    print(f"\n🧪 Testing transcript API with first document...")
    first_doc = documents[0]
    doc_id = first_doc.get("id")
    title = first_doc.get("title", "Untitled")
    
    print(f"📄 Document: {title}")
    print(f"🆔 ID: {doc_id}")
    
    transcript_data = test_transcript_api(token, doc_id)
    
    if transcript_data:
        print(f"\n✅ Transcript API test completed successfully!")
        print(f"📊 Response contains {len(transcript_data) if isinstance(transcript_data, list) else 'unknown'} segments")
    else:
        print(f"\n⚠️  No transcript data received (might be normal if document has no transcript)")
    
    print("\n🎉 Test complete!")

if __name__ == "__main__":
    main() 