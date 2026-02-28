package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/log"
)

type TranscriptSegment struct {
	Text         string `json:"text"`
	Speaker      string `json:"speaker"`
	StartTimestamp any    `json:"start_timestamp"`
	EndTimestamp   any    `json:"end_timestamp"`
}

type ProseMirrorContent struct {
	Type     string                 `json:"type"`
	Content  []ProseMirrorContent   `json:"content,omitempty"`
	Attrs    map[string]any         `json:"attrs,omitempty"`
	Text     string                 `json:"text,omitempty"`
}

type GranolaDocument struct {
	ID              string         `json:"id"`
	Title           any            `json:"title"`
	CreatedAt       string         `json:"created_at,omitempty"`
	UpdatedAt       string         `json:"updated_at,omitempty"`
	LastViewedPanel map[string]any `json:"last_viewed_panel,omitempty"`
}

type DocumentsResponse struct {
	Docs     []GranolaDocument `json:"docs"`
	Deleted  []any             `json:"deleted,omitempty"`
}

func main() {
	outputDir := ""
	fullSync := false
	dryRun := false
	clean := false

	args := os.Args[1:]
	for i, arg := range args {
		switch arg {
		case "--full-sync":
			fullSync = true
		case "--dry-run":
			dryRun = true
		case "--clean":
			clean = true
		default:
			if i == len(args)-1 && !strings.HasPrefix(arg, "-") {
				outputDir = arg
			}
		}
	}

	if outputDir == "" {
		log.Fatal("Usage: granola-sync [OPTIONS] OUTPUT_DIR")
	}

	outputPath := filepath.Clean(outputDir)
	if err := validateOutputDir(outputPath); err != nil {
		log.Fatal(err)
	}

	// Set up log file output (write to both stderr and file)
	logFile, err := os.OpenFile("granola_sync.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Warn("Could not open log file", "err", err)
	} else {
		defer logFile.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	}

	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		level, err := log.ParseLevel(lvl)
		if err != nil {
			log.Warnf("Invalid LOG_LEVEL '%s'. Falling back to DEBUG.", lvl)
			log.SetLevel(log.DebugLevel)
		} else {
			log.SetLevel(level)
		}
	}

	log.Info("Starting Granola sync process")

	localNotes, err := buildLocalNoteIndex(outputPath)
	if err != nil {
		log.Error("Error building local note index", "err", err)
	}

	if dryRun {
		log.Info("DRY RUN MODE - No files will be created or modified")
	}

	if fullSync {
		log.Info("FULL SYNC MODE - All documents will be synced regardless of timestamps")
	}

	if clean {
		log.Warn("--clean flag is not yet implemented")
	}

	token, err := loadCredentials()
	if err != nil || token == "" {
		log.Error("Failed to load credentials. Exiting.")
		os.Exit(1)
	}

	log.Info("Credentials loaded successfully. Fetching documents from Granola API...")

	apiResponse, err := fetchGranolaDocuments(token)
	if err != nil {
		log.Error("Failed to fetch documents", "err", err)
		os.Exit(1)
	}

	if len(apiResponse.Docs) == 0 {
		log.Error("API response format is unexpected - 'docs' key not found or empty")
		os.Exit(1)
	}

	log.Infof("Successfully fetched %d documents from Granola", len(apiResponse.Docs))

	transcriptFilesByID, err := buildTranscriptFileIndex(outputPath, apiResponse.Docs, localNotes)
	if err != nil {
		log.Error("Error building transcript file index", "err", err)
	}

	reconcileTranscriptPaths(outputPath, localNotes, transcriptFilesByID, dryRun)

	if err := syncDocuments(apiResponse.Docs, outputPath, localNotes, token, fullSync, dryRun); err != nil {
		log.Error("Sync error", "err", err)
		os.Exit(1)
	}
}

func validateOutputDir(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("output directory '%s' does not exist", path)
	}
	if err != nil {
		return fmt.Errorf("error accessing output directory: %v", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path '%s' is not a directory", path)
	}
	return nil
}

func loadCredentials() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not get home directory: %v", err)
	}

	credsPath := filepath.Join(homeDir, "Library", "Application Support", "Granola", "supabase.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		log.Errorf("Credentials file not found at: %s", credsPath)
		return "", fmt.Errorf("credentials file not found")
	}

	var creds map[string]any
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("could not parse credentials: %v", err)
	}

	token := extractAccessToken(creds)
	if token == "" {
		return "", fmt.Errorf("no access token found in credentials")
	}

	log.Debug("Successfully loaded credentials")
	return token, nil
}

func extractAccessToken(creds map[string]any) string {
	for _, key := range []string{"workos_tokens", "cognito_tokens"} {
		tokenPayload := creds[key]
		if tokenPayload == nil {
			continue
		}

		var tokenData map[string]any

		switch v := tokenPayload.(type) {
		case string:
			if v == "" {
				continue
			}
			if err := json.Unmarshal([]byte(v), &tokenData); err != nil {
				log.Warnf("Could not parse token payload from '%s'", key)
				continue
			}
		case map[string]any:
			tokenData = v
		default:
			continue
		}

		if accessToken, ok := tokenData["access_token"].(string); ok && accessToken != "" {
			log.Debugf("Loaded access token from '%s'", key)
			return accessToken
		}
	}

	// Fallback: top-level access_token
	if accessToken, ok := creds["access_token"].(string); ok && accessToken != "" {
		log.Debug("Loaded access token from top-level field")
		return accessToken
	}

	return ""
}

func fetchGranolaDocuments(token string) (*DocumentsResponse, error) {
	url := "https://api.granola.ai/v2/get-documents"
	pageSize := 100
	maxPages := 1000
	offset := 0
	pageIndex := 0
	var allDocs []GranolaDocument
	var allDeleted []any
	seenDocIDs := make(map[string]bool)
	seenDeletedIDs := make(map[string]bool)

	client := &http.Client{Timeout: 60 * time.Second}

	for pageIndex < maxPages {
		reqBody := map[string]any{
			"limit":                      pageSize,
			"offset":                     offset,
			"include_last_viewed_panel":  true,
		}
		bodyBytes, _ := json.Marshal(reqBody)

		req, err := http.NewRequestWithContext(context.Background(), "POST", url, strings.NewReader(string(bodyBytes)))
		if err != nil {
			return nil, fmt.Errorf("could not create request: %v", err)
		}

		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("User-Agent", "Granola/5.354.0")
		req.Header.Set("X-Client-Version", "5.354.0")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HTTP request failed: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
		}

		var response DocumentsResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("could not decode response: %v", err)
		}
		resp.Body.Close()

		if response.Docs == nil {
			response.Docs = []GranolaDocument{}
		}
		if response.Deleted == nil {
			response.Deleted = []any{}
		}

		newDocsInPage := 0
		for _, doc := range response.Docs {
			if doc.ID == "" || !seenDocIDs[doc.ID] {
				seenDocIDs[doc.ID] = true
				allDocs = append(allDocs, doc)
				newDocsInPage++
			}
		}

		for _, deleted := range response.Deleted {
			if idStr, ok := deleted.(string); ok && !seenDeletedIDs[idStr] {
				seenDeletedIDs[idStr] = true
				allDeleted = append(allDeleted, deleted)
			} else if idInt, ok := deleted.(float64); ok {
				idStr := fmt.Sprintf("%v", idInt)
				if !seenDeletedIDs[idStr] {
					seenDeletedIDs[idStr] = true
					allDeleted = append(allDeleted, deleted)
				}
			}
		}

		log.Debugf("Fetched page %d: %d docs (%d new), offset=%d", pageIndex+1, len(response.Docs), newDocsInPage, offset)

		if len(response.Docs) == 0 {
			break
		}
		if len(response.Docs) < pageSize {
			break
		}
		if newDocsInPage == 0 {
			log.Warn("Pagination yielded no new docs on a full page; stopping to avoid loop")
			break
		}

		offset += pageSize
		pageIndex++
	}

	if pageIndex >= maxPages {
		log.Warnf("Reached pagination safety limit (%d pages)", maxPages)
	}

	return &DocumentsResponse{Docs: allDocs, Deleted: allDeleted}, nil
}

func fetchDocumentTranscript(token, docID string) (string, []TranscriptSegment, error) {
	url := fmt.Sprintf("https://api.granola.ai/v1/get-document-transcript")
	data, _ := json.Marshal(map[string]string{"document_id": docID})

	req, err := http.NewRequestWithContext(context.Background(), "POST", url, strings.NewReader(string(data)))
	if err != nil {
		return "", nil, fmt.Errorf("could not create request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Granola/5.354.0")
	req.Header.Set("X-Client-Version", "5.354.0")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		log.Debugf("No transcript available for document %s", docID)
		return "", nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var transcript []TranscriptSegment
	if err := json.NewDecoder(resp.Body).Decode(&transcript); err != nil {
		return "", nil, fmt.Errorf("could not decode transcript: %v", err)
	}

	log.Debugf("Transcript segments count: %d", len(transcript))
	formatted := formatTranscriptSegments(transcript)

	return formatted, transcript, nil
}

func formatTranscriptSegments(segments []TranscriptSegment) string {
	var lines []string

	for _, segment := range segments {
		if segment.Text == "" {
			continue
		}

		timeStr := formatTime(segment.StartTimestamp)

		if segment.Speaker != "" && timeStr != "" {
			lines = append(lines, fmt.Sprintf("**%s** [%s]: %s", segment.Speaker, timeStr, segment.Text))
		} else if segment.Speaker != "" {
			lines = append(lines, fmt.Sprintf("**%s**: %s", segment.Speaker, segment.Text))
		} else if timeStr != "" {
			lines = append(lines, fmt.Sprintf("[%s]: %s", timeStr, segment.Text))
		} else {
			lines = append(lines, segment.Text)
		}
	}

	return strings.Join(lines, "\n\n")
}

func formatTime(timestamp any) string {
	if timestamp == nil {
		return ""
	}

	switch v := timestamp.(type) {
	case float64:
		minutes := int(v / 60)
		seconds := int(v - float64(int(v/60)*60))
		return fmt.Sprintf("%02d:%02d", minutes, seconds)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", timestamp)
	}
}

func convertProseMirrorToMarkdown(content ProseMirrorContent) string {
	return processNode(content)
}

func processNode(node ProseMirrorContent) string {
	switch node.Type {
	case "heading":
		level := 1
		if attrs, ok := node.Attrs["level"].(float64); ok {
			level = int(attrs)
		}
		var headingText string
		for _, child := range node.Content {
			headingText += processNode(child)
		}
		return fmt.Sprintf("%s %s\n\n", strings.Repeat("#", level), headingText)

	case "paragraph":
		var paraText string
		for _, child := range node.Content {
			paraText += processNode(child)
		}
		return paraText + "\n\n"

	case "bulletList":
		var items []string
		for _, item := range node.Content {
			if item.Type == "listItem" {
				var itemContent string
				for _, child := range item.Content {
					itemContent += processNode(child)
				}
				items = append(items, "- "+strings.TrimSpace(itemContent))
			}
		}
		return strings.Join(items, "\n") + "\n\n"

	case "text":
		return node.Text

	default:
		var result string
		for _, child := range node.Content {
			result += processNode(child)
		}
		return result
	}
}

func normalizeTitle(title any) string {
	var normalized string
	switch v := title.(type) {
	case string:
		normalized = strings.TrimSpace(v)
	case nil:
		normalized = ""
	default:
		normalized = strings.TrimSpace(fmt.Sprintf("%v", title))
	}
	if normalized == "" {
		return "Untitled Granola Note"
	}
	return normalized
}

func sanitizeFilename(title any) string {
	safeTitle := normalizeTitle(title)
	re := regexp.MustCompile(`[<>:"/\\|?*]`)
	result := re.ReplaceAllString(safeTitle, "")
	if result == "" {
		return "Untitled Granola Note"
	}
	return result
}

func extractDateFromDoc(doc GranolaDocument) string {
	dateStr := doc.CreatedAt
	if dateStr == "" {
		dateStr = doc.UpdatedAt
	}

	if dateStr == "" {
		return time.Now().Format("2006-01-02")
	}

	dateStr = strings.ReplaceAll(dateStr, "Z", "+00:00")
	t, err := time.Parse(time.RFC3339, dateStr)
	if err != nil {
		log.Warnf("Could not parse date '%s', using current date", dateStr)
		return time.Now().Format("2006-01-02")
	}

	return t.Format("2006-01-02")
}

func buildOutputRelativePath(datePrefix, filename string) string {
	parts := strings.Split(datePrefix, "-")
	if len(parts) != 3 {
		log.Warnf("Unexpected date prefix '%s', falling back to unsorted root path", datePrefix)
		return filename
	}

	return filepath.Join(parts[0], parts[1], parts[2], filename)
}

func parseFrontmatterFile(filepath string) (map[string]string, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		log.Warnf("Could not read markdown file '%s': %v", filepath, err)
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("no frontmatter found")
	}

	frontmatter := make(map[string]string)
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		stripped := strings.TrimSpace(line)
		if stripped == "---" {
			break
		}
		if !strings.Contains(stripped, ":") {
			continue
		}

		parts := strings.SplitN(stripped, ":", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if key == "" {
			continue
		}

		if len(value) >= 2 && ((strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")) ||
			(strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'"))) {
			value = value[1 : len(value)-1]
		}

		frontmatter[key] = value
	}

	return frontmatter, nil
}

func buildLocalNoteIndex(outputPath string) (map[string]map[string]string, error) {
	notesByID := make(map[string]map[string]string)
	duplicateIDs := make(map[string]bool)

	err := filepath.Walk(outputPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".md") {
			frontmatter, err := parseFrontmatterFile(path)
			if err != nil {
				return nil
			}

			granolaID := strings.TrimSpace(frontmatter["granola_id"])
			if granolaID == "" {
				return nil
			}

			granolaUpdatedAt := frontmatter["updated_at"]
			if granolaUpdatedAt == "" {
				granolaUpdatedAt = frontmatter["created_at"]
			}

			noteEntry := map[string]string{
				"local_filename":     strings.TrimPrefix(path, outputPath+"/"),
				"granola_updated_at": granolaUpdatedAt,
				"path":               path,
			}

			if existing, ok := notesByID[granolaID]; ok {
				duplicateIDs[granolaID] = true
				stat1, err1 := os.Stat(path)
				stat2, err2 := os.Stat(existing["path"])
				if err1 == nil && err2 == nil {
					if stat1.ModTime().After(stat2.ModTime()) {
						notesByID[granolaID] = noteEntry
					}
				}
				log.Warnf("Duplicate granola_id '%s' found in local notes. Using most recently modified file.", granolaID)
				return nil
			}

			notesByID[granolaID] = noteEntry
		}
		return nil
	})

	if err != nil {
		log.Error("Error walking output path:", err)
	}

	if len(duplicateIDs) > 0 {
		log.Warnf("Found %d duplicate granola_id values in local markdown files.", len(duplicateIDs))
	}

	log.Infof("Indexed %d existing notes by granola_id", len(notesByID))

	return notesByID, nil
}

func buildTranscriptFileIndex(outputPath string, documents []GranolaDocument, localNotes map[string]map[string]string) (map[string]string, error) {
	transcriptFilesByID := make(map[string]string)
	matchedTranscriptPaths := make(map[string]bool)

	var allTranscriptPaths []string
	err := filepath.Walk(outputPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".transcript.json") {
			allTranscriptPaths = append(allTranscriptPaths, path)
		}
		return nil
	})

	if err != nil {
		log.Error("Error walking output path for transcripts:", err)
	}

	for _, doc := range documents {
		docID := doc.ID
		if docID == "" {
			continue
		}

		candidatePaths := []string{}
		if note, ok := localNotes[docID]; ok {
			localNotePath := filepath.Join(outputPath, note["local_filename"])
			candidatePaths = append(candidatePaths, strings.TrimSuffix(localNotePath, ".md")+".transcript.json")
		}

		datePrefix := extractDateFromDoc(doc)
		filename := fmt.Sprintf("%s - %s.md", datePrefix, sanitizeFilename(doc.Title))
		generatedNotePath := buildOutputRelativePath(datePrefix, filename)
		candidatePaths = append(candidatePaths, strings.TrimSuffix(filepath.Join(outputPath, generatedNotePath), ".md")+".transcript.json")

		var existingCandidates []string
		seen := make(map[string]bool)

		for _, candidate := range candidatePaths {
			candidateKey := filepath.ToSlash(candidate)
			if !seen[candidateKey] && fileExists(candidate) {
				seen[candidateKey] = true
				existingCandidates = append(existingCandidates, candidate)
			}
		}

		if len(existingCandidates) == 0 {
			continue
		}

		selectedPath := existingCandidates[0]
		if len(existingCandidates) > 1 {
			log.Warnf("Multiple transcript sidecars found for document ID %s. Using %s", docID, selectedPath)
		}

		transcriptFilesByID[docID] = selectedPath
		matchedTranscriptPaths[selectedPath] = true
	}

	unmatchedCount := len(allTranscriptPaths)
	for _, p := range allTranscriptPaths {
		if matchedTranscriptPaths[p] {
			unmatchedCount--
		}
	}

	if unmatchedCount > 0 {
		log.Warnf("Found %d transcript sidecar files that could not be matched to a document ID", unmatchedCount)
	}

	log.Infof("Indexed %d transcript sidecar files by document ID", len(transcriptFilesByID))
	return transcriptFilesByID, nil
}

func reconcileTranscriptPaths(outputPath string, localNotes map[string]map[string]string, transcriptFilesByID map[string]string, dryRun bool) int {
	renamedCount := 0

	for docID, noteEntry := range localNotes {
		actualTranscriptPath, ok := transcriptFilesByID[docID]
		if !ok {
			continue
		}

		expectedNotePath := filepath.Join(outputPath, noteEntry["local_filename"])
		expectedTranscriptPath := strings.TrimSuffix(expectedNotePath, ".md") + ".transcript.json"

		if actualTranscriptPath == expectedTranscriptPath {
			continue
		}

		if fileExists(expectedTranscriptPath) {
			log.Warnf("Expected transcript path already exists for document ID %s; leaving existing file at %s", docID, actualTranscriptPath)
			continue
		}

		if dryRun {
			log.Infof("[DRY RUN] Would rename transcript sidecar for document ID %s: %s -> %s", docID, actualTranscriptPath, expectedTranscriptPath)
			renamedCount++
			continue
		}

		if err := os.MkdirAll(filepath.Dir(expectedTranscriptPath), 0755); err != nil {
			log.Warnf("Could not create directory for transcript sidecar rename for document ID %s: %v", docID, err)
			continue
		}

		if err := os.Rename(actualTranscriptPath, expectedTranscriptPath); err != nil {
			log.Warnf("Could not rename transcript sidecar for document ID %s: %v", docID, err)
			continue
		}

		transcriptFilesByID[docID] = expectedTranscriptPath
		log.Infof("Renamed transcript sidecar for document ID %s: %s -> %s", docID, actualTranscriptPath, expectedTranscriptPath)
		renamedCount++
	}

	if renamedCount > 0 {
		prefix := ""
		verb := ""
		if dryRun {
			prefix = "[DRY RUN] "
			verb = "would be "
		}
		log.Infof("%sTranscript sidecar rename checks: %d %srenamed", prefix, renamedCount, verb)
	}

	return renamedCount
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

func needsSync(doc GranolaDocument, localNotes map[string]map[string]string, forceFullSync bool) (bool, string) {
	if forceFullSync {
		return true, "full-sync requested"
	}

	docID := doc.ID
	if docID == "" {
		return true, "missing document ID"
	}

	if _, ok := localNotes[docID]; !ok {
		return true, "new document"
	}

	docState := localNotes[docID]
	granolaUpdatedAt := doc.UpdatedAt
	if granolaUpdatedAt == "" {
		granolaUpdatedAt = doc.CreatedAt
	}
	localUpdatedAt := docState["granola_updated_at"]

	if granolaUpdatedAt == "" {
		return true, "no timestamp from Granola"
	}

	if localUpdatedAt == "" {
		return true, "local note missing updated_at frontmatter"
	}

	if granolaUpdatedAt > localUpdatedAt {
		return true, fmt.Sprintf("document updated (%s > %s)", granolaUpdatedAt, localUpdatedAt)
	}

	return false, "document unchanged"
}

func syncDocuments(documents []GranolaDocument, outputPath string, localNotes map[string]map[string]string, token string, fullSync, dryRun bool) error {
	syncedCount := 0
	updatedCount := 0
	skippedCount := 0

	for _, doc := range documents {
		title := normalizeTitle(doc.Title)
		docID := doc.ID
		displayDocID := docID
		if displayDocID == "" {
			displayDocID = "unknown_id"
		}

		shouldSync, reason := needsSync(doc, localNotes, fullSync)

		if !shouldSync {
			log.Debugf("Skipping document '%s' (ID: %s) - %s", title, displayDocID, reason)
			skippedCount++
			continue
		}

		log.Infof("Processing document: %s (ID: %s) - %s", title, displayDocID, reason)

		var contentToParse *ProseMirrorContent
		if lastViewedPanelRaw, ok := doc.LastViewedPanel["content"].(map[string]any); ok {
			if lastViewedPanelRaw["type"] == "doc" {
				contentToParse = parseProseMirrorContent(lastViewedPanelRaw)
			}
		}

		var localRelativeFilename string
		var filePath string

		if existingNote, ok := localNotes[docID]; ok {
			localRelativeFilename = existingNote["local_filename"]
			filePath = filepath.Join(outputPath, localRelativeFilename)
		} else {
			localRelativePath := buildGeneratedNoteRelativePath(doc)
			localRelativeFilename = localRelativePath
			filePath = filepath.Join(outputPath, localRelativePath)
		}

		transcriptJSONPath := strings.TrimSuffix(filePath, ".md") + ".transcript.json"
		isUpdate := localNotes[docID] != nil

		if dryRun {
			if contentToParse == nil {
				var transcriptMarkdown string
				if docID != "" {
					transcriptMarkdown, _, _ = fetchDocumentTranscript(token, docID)
				}
				if transcriptMarkdown == "" {
					log.Warnf("[DRY RUN] Would SKIP document '%s' (ID: %s) - no suitable content found in 'last_viewed_panel' and no transcript available", title, displayDocID)
					skippedCount++
					continue
				}
			}

			action := "UPDATE"
			if !isUpdate {
				action = "CREATE"
			}

			if contentToParse != nil {
				log.Infof("[DRY RUN] Would %s: %s", action, localRelativeFilename)
			} else {
				log.Infof("[DRY RUN] Would %s (transcript-only): %s", action, localRelativeFilename)
			}

			if isUpdate {
				updatedCount++
			} else {
				syncedCount++
			}
			continue
		}

		markdownContent := ""
		if contentToParse != nil {
			log.Debugf("Converting document to markdown: %s", title)
			markdownContent = convertProseMirrorToMarkdown(*contentToParse)
		} else {
			log.Infof("No suitable content found in 'last_viewed_panel' for '%s' (ID: %s); attempting transcript-only export", title, displayDocID)
		}

		var transcriptMarkdown string
		var transcriptData []TranscriptSegment
		if docID != "" {
			var err error
			transcriptMarkdown, transcriptData, err = fetchDocumentTranscript(token, docID)
			if err != nil {
				log.Errorf("Error fetching transcript for %s: %v", docID, err)
			}
		}

		if contentToParse == nil && transcriptData == nil {
			log.Warnf("Skipping document '%s' (ID: %s) - no suitable content found in 'last_viewed_panel' and no transcript available", title, displayDocID)
			skippedCount++
			continue
		}

		frontmatter := buildFrontmatter(doc, transcriptData != nil)
		finalMarkdown := frontmatter

		if markdownContent != "" {
			finalMarkdown += markdownContent
		} else {
			finalMarkdown += fmt.Sprintf("# %s\n\n", title)
			finalMarkdown += "_Transcript-only export (note body unavailable from Granola API)._"
		}

		if transcriptMarkdown != "" {
			log.Debugf("Adding transcript section for document: %s", title)
			if markdownContent != "" {
				finalMarkdown += "\n\n---\n\n## Transcript\n\n"
			} else {
				finalMarkdown += "\n\n## Transcript\n\n"
			}
			finalMarkdown += strings.TrimSpace(transcriptMarkdown)
		}

		log.Debugf("Writing file to: %s", filePath)
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			log.Errorf("Error creating directory: %v", err)
			continue
		}

		if err := os.WriteFile(filePath, []byte(finalMarkdown), 0644); err != nil {
			log.Errorf("Error writing file: %v", err)
			continue
		}

		if transcriptData != nil {
			transcriptBytes, _ := json.MarshalIndent(transcriptData, "", "  ")
			if err := os.WriteFile(transcriptJSONPath, transcriptBytes, 0644); err != nil {
				log.Errorf("Error writing transcript sidecar: %v", err)
			}
		} else if fileExists(transcriptJSONPath) {
			log.Debugf("Removing stale transcript sidecar: %s", transcriptJSONPath)
			os.Remove(transcriptJSONPath)
		}

		if docID != "" {
			granolaUpdatedAt := doc.UpdatedAt
			if granolaUpdatedAt == "" {
				granolaUpdatedAt = doc.CreatedAt
			}
			localNotes[docID] = map[string]string{
				"local_filename":     localRelativeFilename,
				"granola_updated_at": granolaUpdatedAt,
				"path":               filePath,
			}
		}

		action := "Updated"
		if !isUpdate {
			action = "Created"
		}
		log.Infof("%s: %s", action, filePath)

		if isUpdate {
			updatedCount++
		} else {
			syncedCount++
		}
	}

	totalProcessed := syncedCount + updatedCount
	log.Infof("Sync complete! Created: %d, Updated: %d, Skipped: %d, Total processed: %d", syncedCount, updatedCount, skippedCount, totalProcessed)

	if dryRun {
		log.Info("DRY RUN - No actual changes were made")
	}

	return nil
}

func buildGeneratedNoteRelativePath(doc GranolaDocument) string {
	title := normalizeTitle(doc.Title)
	datePrefix := extractDateFromDoc(doc)
	sanitizedTitle := sanitizeFilename(title)
	filename := fmt.Sprintf("%s - %s.md", datePrefix, sanitizedTitle)
	return buildOutputRelativePath(datePrefix, filename)
}

func parseProseMirrorContent(data map[string]any) *ProseMirrorContent {
	content := ProseMirrorContent{
		Type: data["type"].(string),
	}

	if attrs, ok := data["attrs"].(map[string]any); ok {
		content.Attrs = attrs
	}

	if text, ok := data["text"].(string); ok {
		content.Text = text
	}

	if rawContent, ok := data["content"].([]any); ok {
		for _, item := range rawContent {
			if itemMap, ok := item.(map[string]any); ok {
				content.Content = append(content.Content, *parseProseMirrorContent(itemMap))
			}
		}
	}

	return &content
}

func buildFrontmatter(doc GranolaDocument, hasTranscript bool) string {
	frontmatter := "---\n"
	frontmatter += fmt.Sprintf("granola_id: %s\n", doc.ID)
	escapedTitle := strings.ReplaceAll(normalizeTitle(doc.Title), `"`, `\"`)
	frontmatter += fmt.Sprintf("title: \"%s\"\n", escapedTitle)

	if doc.CreatedAt != "" {
		frontmatter += fmt.Sprintf("created_at: %s\n", doc.CreatedAt)
	}
	if doc.UpdatedAt != "" {
		frontmatter += fmt.Sprintf("updated_at: %s\n", doc.UpdatedAt)
	}

	if hasTranscript {
		frontmatter += "has_transcript: true\n"
	} else {
		frontmatter += "has_transcript: false\n"
	}

	frontmatter += "---\n\n"

	return frontmatter
}
