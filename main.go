package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"golang.org/x/net/html"
)

var (
	invalidFilenameChars = regexp.MustCompile(`[<>:"/\\|?*]`)
	ansiEscape           = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	htmlInlineSpace      = regexp.MustCompile(`\s+`)
	htmlBlankLines       = regexp.MustCompile(`\n{3,}`)
)

const (
	granolaClientVersion       = "7.452.4"
	granolaAuthKeychainService = "ai.granola.sync"
	granolaAuthKeychainAccount = "session"
	granolaAuthCallbackScheme  = "granola"
	granolaAuthWebRedirect     = "https://www.granola.ai/app-redirect"
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

type GranolaTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ObtainedAt   int64  `json:"obtained_at"`
	TokenType    string `json:"token_type,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	SignInMethod string `json:"sign_in_method,omitempty"`
	ExternalID   string `json:"external_id,omitempty"`
}

type granolaAuthCompleteResponse struct {
	Tokens GranolaTokens `json:"tokens"`
}

type granolaAppIdentity struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// DocumentPanel is one AI-generated panel (e.g. the meeting summary) returned by
// the get-document-panels endpoint. Content is HTML. This is the source of truth
// for summaries regardless of whether the panel was ever opened in the desktop
// app, unlike the last_viewed_panel field embedded in the documents response.
type DocumentPanel struct {
	ID               string       `json:"id"`
	TemplateSlug     string       `json:"template_slug"`
	Content          PanelContent `json:"content"`
	ContentUpdatedAt string       `json:"content_updated_at"`
	UpdatedAt        string       `json:"updated_at"`
	DeletedAt        any          `json:"deleted_at"`
}

// PanelContent is a panel body as served by Granola: usually an HTML string,
// but newer panels return the ProseMirror document as a JSON object. Objects
// are kept as their raw JSON text so panelContentToMarkdown can handle both.
type PanelContent string

func (c *PanelContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*c = ""
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		*c = PanelContent(text)
		return nil
	}
	*c = PanelContent(trimmed)
	return nil
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
	authProvider := strings.ToLower(strings.TrimSpace(os.Getenv("GRANOLA_AUTH_PROVIDER")))
	if authProvider == "" {
		authProvider = "google"
	}

	var positionalArgs []string
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--full-sync":
			fullSync = true
		case "--dry-run":
			dryRun = true
		default:
			if strings.HasPrefix(arg, "--auth-provider=") {
				authProvider = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(arg, "--auth-provider=")))
			} else if !strings.HasPrefix(arg, "-") {
				positionalArgs = append(positionalArgs, arg)
			}
		}
	}

	if authProvider != "google" && authProvider != "microsoft" {
		log.Fatal("--auth-provider must be google or microsoft")
	}

	if len(positionalArgs) != 1 {
		log.Fatal("Usage: granola-sync [--dry-run] [--full-sync] [--auth-provider=google|microsoft] OUTPUT_DIR")
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

	session, err := loadOrAuthenticateAccessToken(ctx, client, authProvider)
	if err != nil || session == nil || session.AccessToken == "" {
		log.Error("Failed to authenticate with Granola. Exiting.", "err", err)
		os.Exit(1)
	}
	token := session.AccessToken

	log.Info("Credentials loaded successfully. Fetching documents from Granola API...")

	apiResponse, err := fetchGranolaDocuments(ctx, client, token)
	if err != nil {
		log.Error("Failed to fetch documents", "err", err)
		os.Exit(1)
	}

	if len(apiResponse.Docs) == 0 {
		account := granolaTokenEmail(session)
		if account == "" {
			account = "identity fingerprint " + tokenIdentityFingerprint(session)
		}
		log.Error("Granola returned no documents for this session", "account", account)
		diagnoseEmptyDocuments(ctx, client, token)
		log.Errorf("If that is not the account holding your notes, remove the stored session and re-run to sign in again: security delete-generic-password -s %s", granolaAuthKeychainService)
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

func loadOrAuthenticateAccessToken(ctx context.Context, client *http.Client, authProvider string) (*GranolaTokens, error) {
	desktopIdentity, identityErr := loadGranolaAppIdentity()
	if identityErr != nil {
		log.Debug("Could not load the active Granola desktop identity", "err", identityErr)
	}

	tokens, keychainErr := loadGranolaSyncSession()
	if keychainErr == nil {
		if mismatch := sessionAccountMismatch(tokens, desktopIdentity); mismatch != "" {
			log.Warn("Stored Granola Sync session belongs to a different account; starting browser sign-in for the active Granola account", "detail", mismatch)
			tokens = nil
		} else {
			if !granolaTokensNeedRefresh(tokens, time.Now()) {
				log.Debug("Loaded Granola Sync session from macOS Keychain", "identity_fingerprint", tokenIdentityFingerprint(tokens))
				return tokens, nil
			}
			if tokens.RefreshToken != "" {
				refreshed, err := refreshGranolaTokens(ctx, client, tokens)
				if err == nil {
					return refreshed, nil
				}
				log.Warn("Stored Granola Sync session could not be refreshed; starting browser sign-in", "err", err)
			}
		}
	} else {
		log.Debug("No Granola Sync session found in macOS Keychain", "err", keychainErr)
	}

	legacyToken, legacyErr := loadAccessToken()
	if legacyErr == nil && legacyToken != "" {
		return &GranolaTokens{AccessToken: legacyToken}, nil
	}
	log.Debug("Legacy Granola desktop credentials are unavailable", "err", legacyErr)
	log.Info("Starting Granola browser sign-in. Complete the sign-in in your browser to continue.")

	tokens, err := authenticateWithGranola(ctx, client, authProvider, desktopIdentity.Email)
	if err != nil {
		return nil, fmt.Errorf("browser sign-in failed: %v", err)
	}
	if mismatch := sessionAccountMismatch(tokens, desktopIdentity); mismatch != "" {
		return nil, fmt.Errorf("browser sign-in used a different account than the active Granola desktop account (%s); choose the hinted account and retry", mismatch)
	}
	if email := granolaTokenEmail(tokens); email != "" {
		log.Info("Authenticated with Granola", "account", email)
	} else {
		log.Info("Authenticated with Granola", "identity_fingerprint", tokenIdentityFingerprint(tokens))
	}
	if err := saveGranolaSyncSession(tokens); err != nil {
		return nil, fmt.Errorf("could not save Granola session to macOS Keychain: %v", err)
	}
	return tokens, nil
}

// sessionAccountMismatch reports why a session does not belong to the active
// Granola desktop account, or "" when it matches or cannot be compared. Email
// is compared first: the desktop identity ID and the token's external_id can
// come from different ID namespaces, so an ID difference alone is not proof of
// a different account when the emails agree.
func sessionAccountMismatch(tokens *GranolaTokens, desktop granolaAppIdentity) string {
	tokenEmail := strings.ToLower(strings.TrimSpace(granolaTokenEmail(tokens)))
	desktopEmail := strings.ToLower(strings.TrimSpace(desktop.Email))
	if tokenEmail != "" && desktopEmail != "" {
		if tokenEmail == desktopEmail {
			return ""
		}
		return fmt.Sprintf("session is for %s, desktop app is signed in as %s", tokenEmail, desktopEmail)
	}
	tokenID := granolaTokenExternalID(tokens)
	if tokenID != "" && desktop.ID != "" && tokenID != desktop.ID {
		return "session external_id differs from the desktop user ID"
	}
	return ""
}

// granolaTokenEmail returns the account email carried by the session's ID
// token or access token, or "" when neither includes one.
func granolaTokenEmail(tokens *GranolaTokens) string {
	if tokens == nil {
		return ""
	}
	for _, raw := range []string{tokens.IDToken, tokens.AccessToken} {
		if email := jwtStringClaim(raw, "email"); email != "" {
			return email
		}
	}
	return ""
}

// jwtStringClaim returns a string claim from an unverified JWT payload, or "".
func jwtStringClaim(token, claim string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	value, _ := claims[claim].(string)
	return value
}

func granolaTokensNeedRefresh(tokens *GranolaTokens, now time.Time) bool {
	if tokens == nil || tokens.AccessToken == "" {
		return true
	}
	const refreshWindow = 2 * time.Minute
	if exp := tokenExpiryFromJWT(tokens.AccessToken); exp != 0 {
		return now.Add(refreshWindow).Unix() >= exp
	}
	if tokens.ObtainedAt > 0 && tokens.ExpiresIn > 0 {
		expiresAt := time.UnixMilli(tokens.ObtainedAt).Add(time.Duration(tokens.ExpiresIn) * time.Second)
		return !now.Add(refreshWindow).Before(expiresAt)
	}
	return false
}

func loadGranolaSyncSession() (*GranolaTokens, error) {
	out, err := readGranolaSyncKeychain(granolaAuthKeychainService, granolaAuthKeychainAccount)
	if err != nil {
		return nil, fmt.Errorf("keychain session unavailable: %v", err)
	}

	var tokens GranolaTokens
	if err := json.Unmarshal(bytes.TrimSpace(out), &tokens); err != nil {
		return nil, fmt.Errorf("invalid Granola Sync session in keychain: %v", err)
	}
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("Granola Sync keychain session has no access token")
	}
	if fingerprint := tokenIdentityFingerprint(&tokens); fingerprint != "" {
		log.Debug("Loaded Granola Sync session identity", "identity_fingerprint", fingerprint)
	}
	return &tokens, nil
}

func granolaTokenExternalID(tokens *GranolaTokens) string {
	if tokens == nil {
		return ""
	}
	if tokens.ExternalID != "" {
		return tokens.ExternalID
	}
	parts := strings.Split(tokens.AccessToken, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	return claims.ExternalID
}

func tokenIdentityFingerprint(tokens *GranolaTokens) string {
	externalID := granolaTokenExternalID(tokens)
	if externalID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(externalID))
	return hex.EncodeToString(digest[:8])
}

func loadGranolaAppIdentity() (granolaAppIdentity, error) {
	userDataDir, err := granolaUserDataDir()
	if err != nil {
		return granolaAppIdentity{}, err
	}

	var sentryState struct {
		Scope struct {
			User granolaAppIdentity `json:"user"`
		} `json:"scope"`
	}
	if data, err := os.ReadFile(filepath.Join(userDataDir, "sentry", "scope_v3.json")); err == nil {
		if json.Unmarshal(data, &sentryState) == nil && (sentryState.Scope.User.ID != "" || sentryState.Scope.User.Email != "") {
			return sentryState.Scope.User, nil
		}
	}

	data, err := os.ReadFile(filepath.Join(userDataDir, "supabase.json"))
	if err != nil {
		return granolaAppIdentity{}, fmt.Errorf("active identity unavailable in Granola app data: %v", err)
	}
	var legacy struct {
		UserInfo json.RawMessage `json:"user_info"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return granolaAppIdentity{}, err
	}
	var identity granolaAppIdentity
	if err := json.Unmarshal(legacy.UserInfo, &identity); err == nil && (identity.ID != "" || identity.Email != "") {
		return identity, nil
	}
	var encoded string
	if err := json.Unmarshal(legacy.UserInfo, &encoded); err != nil {
		return granolaAppIdentity{}, fmt.Errorf("Granola user_info has an unsupported format")
	}
	if err := json.Unmarshal([]byte(encoded), &identity); err != nil {
		return granolaAppIdentity{}, err
	}
	return identity, nil
}

func saveGranolaSyncSession(tokens *GranolaTokens) error {
	if tokens == nil || tokens.AccessToken == "" {
		return fmt.Errorf("refusing to save an empty session")
	}
	payload, err := json.Marshal(tokens)
	if err != nil {
		return err
	}

	if err := writeGranolaSyncKeychain(granolaAuthKeychainService, granolaAuthKeychainAccount, payload); err != nil {
		return fmt.Errorf("Keychain Services write failed: %v", err)
	}
	return nil
}

func refreshGranolaTokens(ctx context.Context, client *http.Client, tokens *GranolaTokens) (*GranolaTokens, error) {
	body, err := json.Marshal(map[string]string{"refresh_token": tokens.RefreshToken})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.granola.ai/v1/refresh-access-token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setAPIHeaders(req, tokens.AccessToken)
	req.Header.Set("X-Granola-Time-Zone", time.Now().Location().String())

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("refresh endpoint returned %s", resp.Status)
	}

	var refreshed GranolaTokens
	if err := json.NewDecoder(resp.Body).Decode(&refreshed); err != nil {
		return nil, fmt.Errorf("could not decode refreshed session: %v", err)
	}
	if refreshed.AccessToken == "" {
		return nil, fmt.Errorf("refresh response did not include an access token")
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tokens.RefreshToken
	}
	refreshed.ObtainedAt = time.Now().UnixMilli()
	if err := saveGranolaSyncSession(&refreshed); err != nil {
		return nil, fmt.Errorf("token refreshed but could not be persisted: %v", err)
	}
	log.Debug("Refreshed Granola Sync session and updated macOS Keychain")
	return &refreshed, nil
}

func authenticateWithGranola(ctx context.Context, client *http.Client, provider, loginHint string) (*GranolaTokens, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("browser sign-in is currently supported only on macOS")
	}
	verifier, err := randomURLSafeString(32)
	if err != nil {
		return nil, err
	}
	signInClickID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	authURL, err := buildGranolaAuthURL(provider, verifier, signInClickID, loginHint)
	if err != nil {
		return nil, err
	}

	if err := exec.Command("open", authURL).Run(); err != nil {
		return nil, fmt.Errorf("could not open Granola sign-in in browser: %v", err)
	}
	log.Infof("Waiting for %s sign-in redirect (up to 10 minutes)...", provider)
	log.Info("Granola will open after sign-in; the matching callback will be read automatically from Granola's app data.")
	log.Info("If automatic detection does not work, copy the full https://www.granola.ai/app-redirect?... URL, paste it here, and press Return.")

	rawCallback, err := waitForGranolaAuthRedirect(ctx, os.Stdin, 10*time.Minute, signInClickID, findGranolaAuthRedirectInAppData)
	if err != nil {
		return nil, err
	}
	code, err := parseGranolaAuthCallback(rawCallback, signInClickID)
	if err != nil {
		return nil, err
	}
	log.Debug("Received matching login-complete callback from Granola")
	return exchangeGranolaAuthCode(ctx, client, code, verifier, signInClickID)
}

func randomURLSafeString(byteCount int) (string, error) {
	raw := make([]byte, byteCount)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func randomUUID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}

func buildGranolaAuthURL(provider, verifier, signInClickID, loginHint string) (string, error) {
	if provider != "google" && provider != "microsoft" {
		return "", fmt.Errorf("unsupported auth provider %q", provider)
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])

	u, err := url.Parse("https://api.granola.ai/v1/auth")
	if err != nil {
		return "", err
	}
	query := u.Query()
	query.Set("dev", "false")
	query.Set("platform", "macOS")
	query.Set("version", granolaClientVersion)
	query.Set("sign_in_click_id", signInClickID)
	query.Set("intent", "download")
	query.Set("provider", provider)
	query.Set("code_challenge", challenge)
	query.Set("redirect", granolaAuthWebRedirect)
	if loginHint != "" {
		query.Set("login_hint", loginHint)
		query.Set("select_account", "true")
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

type authRedirectFinder func(expectedSignInClickID string) (string, error)

func waitForGranolaAuthRedirect(ctx context.Context, input io.Reader, timeout time.Duration, signInClickID string, finder authRedirectFinder) (string, error) {
	inputResult := make(chan struct {
		value string
		err   error
	}, 1)
	if input != nil {
		go func() {
			line, err := bufio.NewReader(input).ReadString('\n')
			inputResult <- struct {
				value string
				err   error
			}{value: strings.TrimSpace(line), err: err}
		}()
	} else {
		inputResult = nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	var lastFinderErr error
	for {
		if finder != nil {
			callback, err := finder(signInClickID)
			if err != nil {
				lastFinderErr = err
			} else if callback != "" {
				return callback, nil
			}
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			if lastFinderErr != nil {
				return "", fmt.Errorf("timed out waiting for browser redirect URL (automatic Granola callback detection failed: %v)", lastFinderErr)
			}
			return "", fmt.Errorf("timed out waiting for browser redirect URL")
		case result := <-inputResult:
			if result.err != nil && result.value == "" {
				// Non-interactive launches have no stdin. Keep polling Granola's app data.
				inputResult = nil
				continue
			}
			if result.value == "" {
				return "", fmt.Errorf("no browser redirect URL was entered")
			}
			return result.value, nil
		case <-poll.C:
		}
	}
}

func findGranolaAuthRedirectInAppData(expectedSignInClickID string) (string, error) {
	userDataDir, err := granolaUserDataDir()
	if err != nil {
		return "", err
	}
	return findGranolaAuthRedirectInBreadcrumbFile(filepath.Join(userDataDir, "sentry", "scope_v3.json"), expectedSignInClickID)
}

func findGranolaAuthRedirectInBreadcrumbFile(path, expectedSignInClickID string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var sentryState struct {
		Scope struct {
			Breadcrumbs []struct {
				Data struct {
					Arguments []any `json:"arguments"`
				} `json:"data"`
			} `json:"breadcrumbs"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(contents, &sentryState); err != nil {
		return "", err
	}

	for breadcrumbIndex := len(sentryState.Scope.Breadcrumbs) - 1; breadcrumbIndex >= 0; breadcrumbIndex-- {
		arguments := sentryState.Scope.Breadcrumbs[breadcrumbIndex].Data.Arguments
		for argumentIndex := len(arguments) - 1; argumentIndex >= 0; argumentIndex-- {
			argument, ok := arguments[argumentIndex].(string)
			if !ok || !strings.Contains(argument, "login-complete") {
				continue
			}
			var payload struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal([]byte(argument), &payload); err != nil || payload.URL == "" {
				continue
			}
			callbackURL, err := url.Parse(payload.URL)
			if err != nil {
				continue
			}
			callbackID := callbackURL.Query().Get("signInClickId")
			if callbackID == "" {
				callbackID = callbackURL.Query().Get("sign_in_click_id")
			}
			if callbackID == expectedSignInClickID {
				return payload.URL, nil
			}
		}
	}
	return "", nil
}

func parseGranolaAuthCallback(rawCallback, expectedSignInClickID string) (string, error) {
	callbackURL, err := url.Parse(strings.TrimSpace(rawCallback))
	if err != nil {
		return "", fmt.Errorf("invalid auth callback: %v", err)
	}
	isAppURL := callbackURL.Scheme == granolaAuthCallbackScheme && callbackURL.Host == "login-complete"
	isWebRedirect := callbackURL.Scheme == "https" && callbackURL.Host == "www.granola.ai" && callbackURL.Path == "/app-redirect"
	if !isAppURL && !isWebRedirect {
		return "", fmt.Errorf("unexpected auth callback target %s://%s", callbackURL.Scheme, callbackURL.Host)
	}
	query := callbackURL.Query()
	if authErr := query.Get("error"); authErr != "" {
		return "", fmt.Errorf("Granola sign-in returned %s", authErr)
	}
	callbackID := query.Get("signInClickId")
	if callbackID == "" {
		callbackID = query.Get("sign_in_click_id")
	}
	if callbackID != "" && callbackID != expectedSignInClickID {
		return "", fmt.Errorf("auth callback state did not match this sign-in attempt")
	}
	code := query.Get("code")
	if code == "" {
		return "", fmt.Errorf("auth callback did not include a code")
	}
	return code, nil
}

func exchangeGranolaAuthCode(ctx context.Context, client *http.Client, code, verifier, signInClickID string) (*GranolaTokens, error) {
	payload := map[string]any{
		"code":          code,
		"isDev":         false,
		"platform":      "macOS",
		"dubId":         "",
		"signInClickId": signInClickID,
		"codeVerifier":  verifier,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.granola.ai/v1/workos-auth-complete", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Granola/"+granolaClientVersion)
	req.Header.Set("X-Client-Version", granolaClientVersion)
	req.Header.Set("X-Granola-Time-Zone", time.Now().Location().String())

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("auth completion endpoint returned %s", resp.Status)
	}

	var result granolaAuthCompleteResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("could not decode auth completion response: %v", err)
	}
	if result.Tokens.AccessToken == "" || result.Tokens.RefreshToken == "" {
		return nil, fmt.Errorf("auth completion response did not include a refreshable session")
	}
	if result.Tokens.ObtainedAt == 0 {
		result.Tokens.ObtainedAt = time.Now().UnixMilli()
	}
	return &result.Tokens, nil
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
	return loadAccessTokenFromDir(userDataDir)
}

func loadAccessTokenFromDir(userDataDir string) (string, error) {
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
			encErr = fmt.Errorf("access token expired at %s", time.Unix(exp, 0).Format(time.RFC3339))
			log.Warnf("Token from %s has expired; trying plaintext fallback", encPath)
		} else {
			log.Debugf("Loaded access token from %s", encPath)
			return access, nil
		}
	} else {
		log.Debugf("Could not load token from %s: %v", encPath, encErr)
	}

	access, plainErr := tryPlain()
	if access != "" {
		if exp := tokenExpiryFromJWT(access); exp != 0 && time.Now().Unix() >= exp {
			plainErr = fmt.Errorf("access token expired at %s", time.Unix(exp, 0).Format(time.RFC3339))
		} else {
			log.Debugf("Loaded access token from %s", plainPath)
			return access, nil
		}
	}

	return "", fmt.Errorf("could not load legacy Granola desktop credentials (encrypted: %v; plaintext: %v)", encErr, plainErr)
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
	req.Header.Set("User-Agent", "Granola/"+granolaClientVersion)
	req.Header.Set("X-Client-Version", granolaClientVersion)
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

		responseBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("could not read response: %v", err)
		}
		response, arrayResponse, err := decodeDocumentsPage(responseBody)
		if err != nil {
			return nil, fmt.Errorf("could not decode response: %v", err)
		}
		log.Debug("Documents API response shape", "shape", describeJSONShape(responseBody))

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
		if arrayResponse && len(response.Docs) > pageSize {
			log.Debug("Bulk documents endpoint returned the complete array; pagination is not needed")
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

func decodeDocumentsPage(data []byte) (DocumentsResponse, bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return DocumentsResponse{}, false, fmt.Errorf("empty response body")
	}
	if trimmed[0] == '[' {
		var docs []GranolaDocument
		if err := json.Unmarshal(trimmed, &docs); err != nil {
			return DocumentsResponse{}, true, err
		}
		return DocumentsResponse{Docs: docs}, true, nil
	}
	var response DocumentsResponse
	if err := json.Unmarshal(trimmed, &response); err != nil {
		return DocumentsResponse{}, false, err
	}
	return response, false, nil
}

// diagnoseEmptyDocuments probes the alternate documents endpoint and the
// legacy client version so an empty result can be attributed to the account
// rather than to an endpoint or header change. It only logs.
func diagnoseEmptyDocuments(ctx context.Context, client *http.Client, token string) {
	probes := []struct {
		url     string
		version string
	}{
		{"https://api.granola.ai/v1/get-documents", granolaClientVersion},
		{"https://api.granola.ai/v2/get-documents", "5.354.0"},
	}
	body := []byte(`{"limit":5,"offset":0,"include_last_viewed_panel":false}`)
	for _, probe := range probes {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, probe.url, bytes.NewReader(body))
		if err != nil {
			continue
		}
		setAPIHeaders(req, token)
		req.Header.Set("User-Agent", "Granola/"+probe.version)
		req.Header.Set("X-Client-Version", probe.version)
		resp, err := client.Do(req)
		if err != nil {
			log.Warn("Documents probe failed", "url", probe.url, "client_version", probe.version, "err", err)
			continue
		}
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Info("Documents probe", "url", probe.url, "client_version", probe.version, "status", resp.StatusCode, "shape", describeJSONShape(responseBody))
	}
}

func describeJSONShape(data []byte) string {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return "invalid JSON"
	}
	return describeJSONValue(value, 0)
}

func describeJSONValue(value any, depth int) string {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			if depth >= 1 {
				parts = append(parts, key)
				continue
			}
			parts = append(parts, key+":"+describeJSONValue(typed[key], depth+1))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		if len(typed) == 0 {
			return "array[0]"
		}
		return fmt.Sprintf("array[%d]<%s>", len(typed), describeJSONValue(typed[0], depth+1))
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	default:
		return fmt.Sprintf("%T", value)
	}
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
	if len(transcript) == 0 {
		// Granola returns an empty list for meetings that were never recorded.
		// Treat it exactly like a 404 so the caller does not write an empty
		// sidecar or mark the note has_transcript: true.
		return "", nil, nil
	}
	formatted := formatTranscriptSegments(transcript)

	return formatted, transcript, nil
}

// fetchDocumentPanels retrieves a document's panels (summaries) from Granola. The
// endpoint returns the panels even when the meeting was never opened in the
// desktop app, which is why it can recover summaries that last_viewed_panel omits.
func fetchDocumentPanels(ctx context.Context, client *http.Client, token, docID string) ([]DocumentPanel, error) {
	data, _ := json.Marshal(map[string]string{"document_id": docID})

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.granola.ai/v1/get-document-panels", strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("could not create request: %v", err)
	}
	setAPIHeaders(req, token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var panels []DocumentPanel
	if err := json.NewDecoder(resp.Body).Decode(&panels); err != nil {
		return nil, fmt.Errorf("could not decode panels: %v", err)
	}
	return panels, nil
}

// selectSummaryPanel picks the best summary panel from a document's panels. It
// prefers Granola's meeting-summary templates, and among equally-ranked panels
// the most recently updated one with non-empty content.
func selectSummaryPanel(panels []DocumentPanel) *DocumentPanel {
	var best *DocumentPanel
	bestScore := -1

	for i := range panels {
		p := &panels[i]
		if p.DeletedAt != nil || strings.TrimSpace(string(p.Content)) == "" {
			continue
		}

		score := 1
		if strings.HasPrefix(p.TemplateSlug, "meeting-summary") {
			score = 2
		}

		if best == nil || score > bestScore ||
			(score == bestScore && panelUpdatedAt(p) > panelUpdatedAt(best)) {
			best = p
			bestScore = score
		}
	}

	return best
}

func panelUpdatedAt(p *DocumentPanel) string {
	if p.ContentUpdatedAt != "" {
		return p.ContentUpdatedAt
	}
	return p.UpdatedAt
}

// panelContentToMarkdown converts a panel's stored content to markdown. Granola
// serves panel content as HTML; ProseMirror JSON is handled as a fallback in case
// other panel types embed it.
func panelContentToMarkdown(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}

	if strings.HasPrefix(trimmed, "{") {
		var node map[string]any
		if err := json.Unmarshal([]byte(trimmed), &node); err == nil && node["type"] == "doc" {
			return strings.TrimSpace(convertProseMirrorToMarkdown(*parseProseMirrorContent(node)))
		}
	}

	return htmlToMarkdown(trimmed)
}

// htmlToMarkdown converts a fragment of HTML to markdown, covering the elements
// Granola uses in summaries: headings, paragraphs, nested lists, bold/italic,
// inline code, links, and blockquotes.
func htmlToMarkdown(htmlStr string) string {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return strings.TrimSpace(htmlStr)
	}

	var b strings.Builder
	renderHTMLBlock(doc, &b, 0)

	out := htmlBlankLines.ReplaceAllString(b.String(), "\n\n")
	return strings.TrimSpace(out)
}

func renderHTMLBlock(n *html.Node, b *strings.Builder, depth int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			if txt := strings.TrimSpace(c.Data); txt != "" {
				b.WriteString(htmlInlineSpace.ReplaceAllString(txt, " ") + "\n\n")
			}
		case html.ElementNode:
			switch c.Data {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				level := int(c.Data[1] - '0')
				if txt := strings.TrimSpace(renderHTMLInline(c)); txt != "" {
					b.WriteString(strings.Repeat("#", level) + " " + txt + "\n\n")
				}
			case "p":
				if txt := strings.TrimSpace(renderHTMLInline(c)); txt != "" {
					b.WriteString(txt + "\n\n")
				}
			case "ul", "ol":
				renderHTMLList(c, b, depth, c.Data == "ol")
				if depth == 0 {
					b.WriteString("\n")
				}
			case "blockquote":
				var inner strings.Builder
				renderHTMLBlock(c, &inner, depth)
				for _, line := range strings.Split(strings.TrimRight(inner.String(), "\n"), "\n") {
					b.WriteString("> " + line + "\n")
				}
				b.WriteString("\n")
			case "pre":
				if txt := strings.TrimRight(renderHTMLInline(c), "\n"); txt != "" {
					b.WriteString("```\n" + txt + "\n```\n\n")
				}
			case "br":
				// stray break at block level; ignore
			case "html", "head", "body", "div", "section", "article", "main", "header", "footer":
				renderHTMLBlock(c, b, depth)
			default:
				if txt := strings.TrimSpace(renderHTMLInline(c)); txt != "" {
					b.WriteString(txt + "\n\n")
				}
			}
		}
	}
}

func renderHTMLList(list *html.Node, b *strings.Builder, depth int, ordered bool) {
	idx := 0
	indent := strings.Repeat("  ", depth)

	for li := list.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode || li.Data != "li" {
			continue
		}
		idx++

		marker := "- "
		if ordered {
			marker = fmt.Sprintf("%d. ", idx)
		}

		if txt := strings.TrimSpace(renderHTMLListItemInline(li)); txt != "" {
			b.WriteString(indent + marker + txt + "\n")
		}

		for c := li.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (c.Data == "ul" || c.Data == "ol") {
				renderHTMLList(c, b, depth+1, c.Data == "ol")
			}
		}
	}
}

// renderHTMLListItemInline renders a list item's own content, skipping nested
// lists (those are emitted separately with deeper indentation).
func renderHTMLListItemInline(li *html.Node) string {
	var b strings.Builder
	for c := li.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.Data == "ul" || c.Data == "ol") {
			continue
		}
		b.WriteString(renderHTMLInlineNode(c))
	}
	return b.String()
}

func renderHTMLInline(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(renderHTMLInlineNode(c))
	}
	return b.String()
}

func renderHTMLInlineNode(n *html.Node) string {
	switch n.Type {
	case html.TextNode:
		return htmlInlineSpace.ReplaceAllString(n.Data, " ")
	case html.ElementNode:
		switch n.Data {
		case "strong", "b":
			return "**" + renderHTMLInline(n) + "**"
		case "em", "i":
			return "*" + renderHTMLInline(n) + "*"
		case "code":
			return "`" + renderHTMLInline(n) + "`"
		case "a":
			txt := renderHTMLInline(n)
			if href := htmlAttr(n, "href"); href != "" {
				return "[" + txt + "](" + href + ")"
			}
			return txt
		case "br":
			return "\n"
		default:
			return renderHTMLInline(n)
		}
	}
	return ""
}

func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// resolveNoteBody returns the note-body markdown for a document plus a short label
// describing its source. It first uses the ProseMirror content cached in
// last_viewed_panel (populated only after you open the meeting in the desktop
// app), then falls back to the get-document-panels endpoint, which holds the AI
// summary regardless of whether it was ever viewed.
func resolveNoteBody(ctx context.Context, client *http.Client, token string, doc GranolaDocument) (string, string) {
	if raw, ok := doc.LastViewedPanel["content"].(map[string]any); ok && raw["type"] == "doc" {
		if md := strings.TrimSpace(convertProseMirrorToMarkdown(*parseProseMirrorContent(raw))); md != "" {
			return md, "last_viewed_panel"
		}
	}

	if doc.ID == "" {
		return "", ""
	}

	panels, err := fetchDocumentPanels(ctx, client, token, doc.ID)
	if err != nil {
		log.Warnf("Could not fetch panels for document %s: %v", doc.ID, err)
		return "", ""
	}

	panel := selectSummaryPanel(panels)
	if panel == nil {
		return "", ""
	}

	md := panelContentToMarkdown(string(panel.Content))
	if md == "" {
		return "", ""
	}
	return md, fmt.Sprintf("get-document-panels (%s)", panel.TemplateSlug)
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
	return filepath.Join(yearDir, monthDir, filename)
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

		noteBody, bodySource := resolveNoteBody(ctx, client, token, doc)

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
			if noteBody == "" {
				var transcriptMarkdown string
				if docID != "" {
					transcriptMarkdown, _, _ = fetchDocumentTranscript(ctx, client, token, docID)
				}
				if transcriptMarkdown == "" {
					log.Warnf("[DRY RUN] Would SKIP document '%s' (ID: %s) - no note body found and no transcript available", title, displayDocID)
					skippedCount++
					continue
				}
			}

			action := "UPDATE"
			if !isUpdate {
				action = "CREATE"
			}

			if noteBody != "" {
				log.Infof("[DRY RUN] Would %s: %s (body from %s)", action, localRelativeFilename, bodySource)
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

		markdownContent := noteBody
		if noteBody != "" {
			log.Debugf("Note body for '%s' sourced from %s", title, bodySource)
		} else {
			log.Infof("No note body found for '%s' (ID: %s); attempting transcript-only export", title, displayDocID)
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

		if noteBody == "" && transcriptData == nil {
			log.Warnf("Skipping document '%s' (ID: %s) - no note body found and no transcript available", title, displayDocID)
			skippedCount++
			continue
		}

		var preservedFrontmatter map[string]string
		if isUpdate {
			preservedFrontmatter = readFrontmatterBlocks(filePath)
		}
		frontmatter := buildFrontmatter(doc, transcriptData != nil, preservedFrontmatter)
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
	typeStr, _ := data["type"].(string)
	content := ProseMirrorContent{
		Type: typeStr,
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

// buildFrontmatter renders the note frontmatter. Granola-owned fields are
// regenerated every time; the hand-maintained with: and tags: blocks are taken
// from preserved (see readFrontmatterBlocks) when the existing note has
// non-empty values, so updating a note never discards vault edits.
func buildFrontmatter(doc GranolaDocument, hasTranscript bool, preserved map[string]string) string {
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

	if block := preserved["with"]; frontmatterBlockHasValue(block) {
		b.WriteString(block + "\n")
	} else {
		b.WriteString("with: []\n")
	}

	if block := preserved["tags"]; frontmatterBlockHasValue(block) {
		b.WriteString(block + "\n")
	} else {
		b.WriteString("tags:\n  - meeting\n")
	}
	b.WriteString("---\n\n")
	return b.String()
}

// readFrontmatterBlocks returns the raw YAML text of each top-level key in a
// note's frontmatter: the "key:" line plus its indented or list continuation
// lines. It returns nil when the file cannot be read or has no frontmatter.
func readFrontmatterBlocks(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return nil
	}

	blocks := make(map[string]string)
	var currentKey string
	var current []string
	flush := func() {
		if currentKey != "" {
			blocks[currentKey] = strings.TrimRight(strings.Join(current, "\n"), "\n")
		}
	}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		isContinuation := line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "-")
		if !isContinuation {
			if idx := strings.Index(line, ":"); idx > 0 {
				flush()
				currentKey = strings.TrimSpace(line[:idx])
				current = []string{line}
				continue
			}
		}
		if currentKey != "" {
			current = append(current, line)
		}
	}
	flush()
	return blocks
}

// frontmatterBlockHasValue reports whether a raw frontmatter block carries a
// real value rather than an empty scalar or an empty list.
func frontmatterBlockHasValue(block string) bool {
	lines := strings.Split(block, "\n")
	if len(lines) == 0 {
		return false
	}
	if idx := strings.Index(lines[0], ":"); idx >= 0 {
		inline := strings.TrimSpace(lines[0][idx+1:])
		if inline != "" && inline != "[]" && inline != "{}" && inline != "null" && inline != "~" {
			return true
		}
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}
