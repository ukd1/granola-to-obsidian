package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/log"
)

var (
	invalidFilenameChars = regexp.MustCompile(`[<>:"/\\|?*]`)
	ansiEscape           = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

type TranscriptSegment struct {
	Text           string `json:"text"`
	Speaker        string `json:"speaker"`
	StartTimestamp any    `json:"start_timestamp"`
	EndTimestamp   any    `json:"end_timestamp"`
}

type ProseMirrorContent struct {
	Type    string               `json:"type"`
	Content []ProseMirrorContent `json:"content,omitempty"`
	Attrs   map[string]any       `json:"attrs,omitempty"`
	Text    string               `json:"text,omitempty"`
}

type GranolaDocument struct {
	ID              string         `json:"id"`
	Title           any            `json:"title"`
	CreatedAt       string         `json:"created_at,omitempty"`
	UpdatedAt       string         `json:"updated_at,omitempty"`
	LastViewedPanel map[string]any `json:"last_viewed_panel,omitempty"`
}

type DocumentsResponse struct {
	Docs []GranolaDocument `json:"docs"`
}

type LocalNote struct {
	LocalFilename    string
	GranolaUpdatedAt string
	Path             string
}

// teeWriter writes to both a terminal and a log file, preserving the
// terminal file descriptor so charmbracelet/log retains color detection.
// ANSI escape codes are stripped from file output.
type teeWriter struct {
	terminal *os.File
	file     *os.File
}

func (w *teeWriter) Write(p []byte) (n int, err error) {
	n, err = w.terminal.Write(p)
	if w.file != nil {
		clean := ansiEscape.ReplaceAll(p, nil)
		w.file.Write(clean)
	}
	return
}

func (w *teeWriter) Fd() uintptr {
	return w.terminal.Fd()
}

func main() {
	fullSync := false
	dryRun := false

	var positionalArgs []string
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--full-sync":
			fullSync = true
		case "--dry-run":
			dryRun = true
		default:
			if !strings.HasPrefix(arg, "-") {
				positionalArgs = append(positionalArgs, arg)
			}
		}
	}

	if len(positionalArgs) != 1 {
		log.Fatal("Usage: granola-sync [OPTIONS] OUTPUT_DIR")
	}

	outputPath := filepath.Clean(positionalArgs[0])
	if err := validateOutputDir(outputPath); err != nil {
		log.Fatal(err)
	}

	// Set up dual logging: styled terminal output + clean log file
	logFile, err := os.OpenFile("granola_sync.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.Warn("Could not open log file", "err", err)
	} else {
		defer logFile.Close()
		log.SetOutput(&teeWriter{terminal: os.Stderr, file: logFile})
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client := &http.Client{Timeout: 60 * time.Second}

	log.Info("Starting Granola sync process")

	localNotes, transcriptPaths, err := walkOutputDir(outputPath)
	if err != nil {
		log.Error("Error scanning output directory", "err", err)
	}

	if dryRun {
		log.Info("DRY RUN MODE - No files will be created or modified")
	}
	if fullSync {
		log.Info("FULL SYNC MODE - All documents will be synced regardless of timestamps")
	}
	token, err := loadAccessToken()
	if err != nil || token == "" {
		log.Error("Failed to load access token. Exiting.", "err", err)
		os.Exit(1)
	}

	log.Info("Credentials loaded successfully. Fetching documents from Granola API...")

	apiResponse, err := fetchGranolaDocuments(ctx, client, token)
	if err != nil {
		log.Error("Failed to fetch documents", "err", err)
		os.Exit(1)
	}

	if len(apiResponse.Docs) == 0 {
		log.Error("API response format is unexpected - 'docs' key not found or empty")
		os.Exit(1)
	}

	log.Infof("Successfully fetched %d documents from Granola", len(apiResponse.Docs))

	transcriptFilesByID := buildTranscriptFileIndex(outputPath, apiResponse.Docs, localNotes, transcriptPaths)
	reconcileTranscriptPaths(outputPath, localNotes, transcriptFilesByID, dryRun)

	if err := syncDocuments(ctx, client, apiResponse.Docs, outputPath, localNotes, token, fullSync, dryRun); err != nil {
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

// tokenExpiryFromJWT extracts the unix-seconds expiry from a JWT's exp
// claim. Returns 0 if the token can't be parsed.
func tokenExpiryFromJWT(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}
	if exp, ok := payload["exp"].(float64); ok {
		return int64(exp)
	}
	return 0
}

// granolaUserDataDir returns the directory Granola.app uses for app data.
func granolaUserDataDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not get home directory: %v", err)
	}
	return filepath.Join(homeDir, "Library", "Application Support", "Granola"), nil
}

// granolaSafeStoragePassword returns the keychain entry that Electron's
// safeStorage API uses to wrap Granola's data-encryption key.
func granolaSafeStoragePassword() ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", "Granola Safe Storage", "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("could not read 'Granola Safe Storage' from keychain (was access denied?): %v", err)
	}
	return bytes.TrimSpace(out), nil
}

// chromiumSafeStorageDecrypt mirrors Chromium's OSCrypt format used by
// Electron safeStorage on macOS: optional "v10"/"v11" prefix, AES-128-CBC
// with a 16-space IV, key derived via PBKDF2-HMAC-SHA1(saltysalt, 1003 iters).
func chromiumSafeStorageDecrypt(blob, password []byte) ([]byte, error) {
	if len(blob) >= 3 && (bytes.Equal(blob[:3], []byte("v10")) || bytes.Equal(blob[:3], []byte("v11"))) {
		blob = blob[3:]
	}
	key, err := pbkdf2.Key(sha1.New, string(password), []byte("saltysalt"), 1003, 16)
	if err != nil {
		return nil, fmt.Errorf("PBKDF2 derivation failed: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 || len(blob)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d not a multiple of AES block size", len(blob))
	}
	iv := bytes.Repeat([]byte{' '}, aes.BlockSize)
	out := make([]byte, len(blob))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, blob)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, fmt.Errorf("invalid PKCS7 padding")
	}
	return out[:len(out)-pad], nil
}

// loadGranolaDEK returns the 32-byte AES-256 data-encryption key Granola uses
// for its *.enc files. The DEK is stored at <userData>/storage.dek, wrapped
// by Electron safeStorage and base64-encoded.
func loadGranolaDEK(userDataDir string) ([]byte, error) {
	dekBlob, err := os.ReadFile(filepath.Join(userDataDir, "storage.dek"))
	if err != nil {
		return nil, fmt.Errorf("could not read storage.dek: %v", err)
	}
	password, err := granolaSafeStoragePassword()
	if err != nil {
		return nil, err
	}
	decoded, err := chromiumSafeStorageDecrypt(dekBlob, password)
	if err != nil {
		return nil, fmt.Errorf("could not decrypt storage.dek: %v", err)
	}
	dek, err := base64.StdEncoding.DecodeString(string(decoded))
	if err != nil {
		return nil, fmt.Errorf("could not base64-decode DEK: %v", err)
	}
	if len(dek) != 32 {
		return nil, fmt.Errorf("unexpected DEK length %d (expected 32)", len(dek))
	}
	return dek, nil
}

// decryptGranolaEncFile decrypts files Granola writes via its DEK-wrapped
// AES-256-GCM scheme: [12-byte IV][ciphertext][16-byte tag].
func decryptGranolaEncFile(path string, dek []byte) ([]byte, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(blob) < 12+16 {
		return nil, fmt.Errorf("encrypted file too short: %d bytes", len(blob))
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, blob[:12], blob[12:], nil)
}

// extractAccessToken returns the live access_token from a parsed credentials
// map, preferring workos_tokens over cognito_tokens. Returns "" if no usable
// token is found.
func extractAccessToken(creds map[string]any) string {
	for _, key := range []string{"workos_tokens", "cognito_tokens"} {
		tokenData := extractTokenMap(creds, key)
		if tokenData == nil {
			continue
		}
		if access, _ := tokenData["access_token"].(string); access != "" {
			return access
		}
	}
	return ""
}

// loadAccessToken reads Granola's stored credentials and returns a live
// access token. The encrypted supabase.json.enc is preferred since Granola
// actively rotates it; the plaintext supabase.json is tried as a fallback
// whenever the encrypted source is unavailable or doesn't yield a token (it
// can briefly be missing the expected keys mid-write).
func loadAccessToken() (string, error) {
	userDataDir, err := granolaUserDataDir()
	if err != nil {
		return "", err
	}

	encPath := filepath.Join(userDataDir, "supabase.json.enc")
	plainPath := filepath.Join(userDataDir, "supabase.json")

	tryDecrypted := func() (string, error) {
		if !fileExists(encPath) {
			return "", fmt.Errorf("not present")
		}
		dek, err := loadGranolaDEK(userDataDir)
		if err != nil {
			return "", fmt.Errorf("DEK load failed: %v", err)
		}
		plain, err := decryptGranolaEncFile(encPath, dek)
		if err != nil {
			return "", fmt.Errorf("decrypt failed: %v", err)
		}
		var creds map[string]any
		if err := json.Unmarshal(plain, &creds); err != nil {
			return "", fmt.Errorf("parse failed: %v", err)
		}
		access := extractAccessToken(creds)
		if access == "" {
			return "", fmt.Errorf("no workos_tokens or cognito_tokens in decrypted payload")
		}
		return access, nil
	}

	tryPlain := func() (string, error) {
		data, err := os.ReadFile(plainPath)
		if err != nil {
			return "", fmt.Errorf("read failed: %v", err)
		}
		var creds map[string]any
		if err := json.Unmarshal(data, &creds); err != nil {
			return "", fmt.Errorf("parse failed: %v", err)
		}
		access := extractAccessToken(creds)
		if access == "" {
			return "", fmt.Errorf("no workos_tokens or cognito_tokens in payload")
		}
		return access, nil
	}

	access, encErr := tryDecrypted()
	if access != "" {
		if exp := tokenExpiryFromJWT(access); exp != 0 && time.Now().Unix() >= exp {
			log.Warnf("Token from %s has expired; trying plaintext fallback", encPath)
		} else {
			log.Debugf("Loaded access token from %s", encPath)
			return access, nil
		}
	} else {
		log.Debugf("Could not load token from %s: %v", encPath, encErr)
	}

	access, plainErr := tryPlain()
	if access == "" {
		return "", fmt.Errorf("could not load Granola credentials (encrypted: %v; plaintext: %v) — open the Granola desktop app to refresh, then re-run", encErr, plainErr)
	}
	if exp := tokenExpiryFromJWT(access); exp != 0 && time.Now().Unix() >= exp {
		return "", fmt.Errorf("Granola access tokens in both %s and %s have expired; open the Granola desktop app to refresh, then re-run", encPath, plainPath)
	}
	log.Debugf("Loaded access token from %s", plainPath)
	return access, nil
}

func extractTokenMap(creds map[string]any, key string) map[string]any {
	payload := creds[key]
	if payload == nil {
		return nil
	}
	switch v := payload.(type) {
	case string:
		if v == "" {
			return nil
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			log.Warnf("Could not parse token payload from '%s'", key)
			return nil
		}
		return parsed
	case map[string]any:
		return v
	}
	return nil
}

func setAPIHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Granola/5.354.0")
	req.Header.Set("X-Client-Version", "5.354.0")
}

func fetchGranolaDocuments(ctx context.Context, client *http.Client, token string) (*DocumentsResponse, error) {
	url := "https://api.granola.ai/v2/get-documents"
	pageSize := 100
	maxPages := 1000
	offset := 0
	pageIndex := 0
	var allDocs []GranolaDocument
	seenDocIDs := make(map[string]bool)

	for pageIndex < maxPages {
		reqBody := map[string]any{
			"limit":                     pageSize,
			"offset":                    offset,
			"include_last_viewed_panel": true,
		}
		bodyBytes, _ := json.Marshal(reqBody)

		req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(bodyBytes)))
		if err != nil {
			return nil, fmt.Errorf("could not create request: %v", err)
		}
		setAPIHeaders(req, token)

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
		newDocsInPage := 0
		for _, doc := range response.Docs {
			if doc.ID == "" || !seenDocIDs[doc.ID] {
				seenDocIDs[doc.ID] = true
				allDocs = append(allDocs, doc)
				newDocsInPage++
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

	return &DocumentsResponse{Docs: allDocs}, nil
}

func fetchDocumentTranscript(ctx context.Context, client *http.Client, token, docID string) (string, []TranscriptSegment, error) {
	data, _ := json.Marshal(map[string]string{"document_id": docID})

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.granola.ai/v1/get-document-transcript", strings.NewReader(string(data)))
	if err != nil {
		return "", nil, fmt.Errorf("could not create request: %v", err)
	}
	setAPIHeaders(req, token)

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
		minutes := int(v) / 60
		seconds := int(v) % 60
		return fmt.Sprintf("%02d:%02d", minutes, seconds)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", timestamp)
	}
}

func convertProseMirrorToMarkdown(node ProseMirrorContent) string {
	switch node.Type {
	case "heading":
		level := 1
		if attrs, ok := node.Attrs["level"].(float64); ok {
			level = int(attrs)
		}
		var b strings.Builder
		for _, child := range node.Content {
			b.WriteString(convertProseMirrorToMarkdown(child))
		}
		return fmt.Sprintf("%s %s\n\n", strings.Repeat("#", level), b.String())

	case "paragraph":
		var b strings.Builder
		for _, child := range node.Content {
			b.WriteString(convertProseMirrorToMarkdown(child))
		}
		return b.String() + "\n\n"

	case "bulletList":
		var items []string
		for _, item := range node.Content {
			if item.Type == "listItem" {
				var b strings.Builder
				for _, child := range item.Content {
					b.WriteString(convertProseMirrorToMarkdown(child))
				}
				items = append(items, "- "+strings.TrimSpace(b.String()))
			}
		}
		return strings.Join(items, "\n") + "\n\n"

	case "text":
		return node.Text

	default:
		var b strings.Builder
		for _, child := range node.Content {
			b.WriteString(convertProseMirrorToMarkdown(child))
		}
		return b.String()
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
	result := invalidFilenameChars.ReplaceAllString(safeTitle, "")
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
	t, err := time.Parse("2006-01-02", datePrefix)
	if err != nil {
		log.Warnf("Unexpected date prefix '%s', falling back to unsorted root path", datePrefix)
		return filename
	}

	yearDir := t.Format("2006")
	monthDir := t.Format("01") + " - " + t.Format("Jan")
	return filepath.Join("granola", yearDir, monthDir, filename)
}

func parseFrontmatter(fpath string) (map[string]string, error) {
	f, err := os.Open(fpath)
	if err != nil {
		log.Warnf("Could not read markdown file '%s': %v", fpath, err)
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)

	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return nil, fmt.Errorf("no frontmatter found")
	}

	frontmatter := make(map[string]string)
	for scanner.Scan() {
		line := scanner.Text()
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

// walkOutputDir scans the output directory once, collecting both the local
// note index (by granola_id) and all transcript sidecar paths.
func walkOutputDir(outputPath string) (map[string]LocalNote, []string, error) {
	notesByID := make(map[string]LocalNote)
	duplicateIDs := make(map[string]bool)
	var transcriptPaths []string

	err := filepath.Walk(outputPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		if strings.HasSuffix(path, ".transcript.json") {
			transcriptPaths = append(transcriptPaths, path)
			return nil
		}

		if !strings.HasSuffix(path, ".md") {
			return nil
		}

		frontmatter, err := parseFrontmatter(path)
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

		noteEntry := LocalNote{
			LocalFilename:    strings.TrimPrefix(path, outputPath+"/"),
			GranolaUpdatedAt: granolaUpdatedAt,
			Path:             path,
		}

		if existing, ok := notesByID[granolaID]; ok {
			duplicateIDs[granolaID] = true
			existingStat, err := os.Stat(existing.Path)
			if err == nil && info.ModTime().After(existingStat.ModTime()) {
				notesByID[granolaID] = noteEntry
			}
			log.Warnf("Duplicate granola_id '%s' found in local notes. Using most recently modified file.", granolaID)
			return nil
		}

		notesByID[granolaID] = noteEntry
		return nil
	})

	if err != nil {
		log.Error("Error walking output path", "err", err)
	}

	if len(duplicateIDs) > 0 {
		log.Warnf("Found %d duplicate granola_id values in local markdown files.", len(duplicateIDs))
	}

	log.Infof("Indexed %d existing notes by granola_id", len(notesByID))
	return notesByID, transcriptPaths, nil
}

func buildTranscriptFileIndex(outputPath string, documents []GranolaDocument, localNotes map[string]LocalNote, allTranscriptPaths []string) map[string]string {
	transcriptFilesByID := make(map[string]string)
	matchedTranscriptPaths := make(map[string]bool)

	for _, doc := range documents {
		docID := doc.ID
		if docID == "" {
			continue
		}

		candidatePaths := []string{}
		if note, ok := localNotes[docID]; ok {
			localNotePath := filepath.Join(outputPath, note.LocalFilename)
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

	unmatchedCount := 0
	for _, p := range allTranscriptPaths {
		if !matchedTranscriptPaths[p] {
			unmatchedCount++
		}
	}

	if unmatchedCount > 0 {
		log.Warnf("Found %d transcript sidecar files that could not be matched to a document ID", unmatchedCount)
	}

	log.Infof("Indexed %d transcript sidecar files by document ID", len(transcriptFilesByID))
	return transcriptFilesByID
}

func reconcileTranscriptPaths(outputPath string, localNotes map[string]LocalNote, transcriptFilesByID map[string]string, dryRun bool) int {
	renamedCount := 0

	for docID, noteEntry := range localNotes {
		actualTranscriptPath, ok := transcriptFilesByID[docID]
		if !ok {
			continue
		}

		expectedNotePath := filepath.Join(outputPath, noteEntry.LocalFilename)
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

func needsSync(doc GranolaDocument, localNotes map[string]LocalNote, forceFullSync bool) (bool, string) {
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
	localUpdatedAt := docState.GranolaUpdatedAt

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

func syncDocuments(ctx context.Context, client *http.Client, documents []GranolaDocument, outputPath string, localNotes map[string]LocalNote, token string, fullSync, dryRun bool) error {
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
			localRelativeFilename = existingNote.LocalFilename
			filePath = filepath.Join(outputPath, localRelativeFilename)
		} else {
			localRelativePath := buildGeneratedNoteRelativePath(doc)
			localRelativeFilename = localRelativePath
			filePath = filepath.Join(outputPath, localRelativePath)
		}

		transcriptJSONPath := strings.TrimSuffix(filePath, ".md") + ".transcript.json"
		_, isUpdate := localNotes[docID]

		if dryRun {
			if contentToParse == nil {
				var transcriptMarkdown string
				if docID != "" {
					transcriptMarkdown, _, _ = fetchDocumentTranscript(ctx, client, token, docID)
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
			transcriptMarkdown, transcriptData, err = fetchDocumentTranscript(ctx, client, token, docID)
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
			localNotes[docID] = LocalNote{
				LocalFilename:    localRelativeFilename,
				GranolaUpdatedAt: granolaUpdatedAt,
				Path:             filePath,
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
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "granola_id: %s\n", doc.ID)
	escapedTitle := strings.ReplaceAll(normalizeTitle(doc.Title), `"`, `\"`)
	fmt.Fprintf(&b, "title: \"%s\"\n", escapedTitle)

	if doc.CreatedAt != "" {
		fmt.Fprintf(&b, "created_at: %s\n", doc.CreatedAt)
	}
	if doc.UpdatedAt != "" {
		fmt.Fprintf(&b, "updated_at: %s\n", doc.UpdatedAt)
	}

	if hasTranscript {
		b.WriteString("has_transcript: true\n")
	} else {
		b.WriteString("has_transcript: false\n")
	}
	b.WriteString("with: []\n")

	b.WriteString("tags:\n  - meeting\n")
	b.WriteString("---\n\n")
	return b.String()
}
