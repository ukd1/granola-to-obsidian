package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTMLToMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "granola summary shape",
			in:   "<h3>DIU / CSW Update</h3>\n<ul>\n<li>No official word back yet, but a separate DIU contact confirmed the same positive signal</li>\n<li>Intel suggests this week or next for a decision</li>\n</ul>",
			want: "### DIU / CSW Update\n\n- No official word back yet, but a separate DIU contact confirmed the same positive signal\n- Intel suggests this week or next for a decision",
		},
		{
			name: "paragraph with inline marks",
			in:   "<p>Deal is <strong>closing</strong> with <em>strong</em> terms, see <a href=\"https://x.test\">notes</a>.</p>",
			want: "Deal is **closing** with *strong* terms, see [notes](https://x.test).",
		},
		{
			name: "nested list",
			in:   "<ul><li>Parent<ul><li>Child A</li><li>Child B</li></ul></li></ul>",
			want: "- Parent\n  - Child A\n  - Child B",
		},
		{
			name: "headings and multiple blocks",
			in:   "<h2>Section</h2><p>Intro line.</p><ol><li>First</li><li>Second</li></ol>",
			want: "## Section\n\nIntro line.\n\n1. First\n2. Second",
		},
		{
			name: "empty",
			in:   "   ",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := htmlToMarkdown(tt.in)
			if got != tt.want {
				t.Errorf("htmlToMarkdown() mismatch\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestPanelContentToMarkdownProseMirrorFallback(t *testing.T) {
	// A panel whose content is stringified ProseMirror JSON should still convert.
	in := `{"type":"doc","content":[{"type":"heading","attrs":{"level":1},"content":[{"type":"text","text":"Title"}]},{"type":"paragraph","content":[{"type":"text","text":"Body."}]}]}`
	want := "# Title\n\nBody."
	if got := panelContentToMarkdown(in); got != want {
		t.Errorf("panelContentToMarkdown() mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestSelectSummaryPanel(t *testing.T) {
	t.Run("prefers meeting-summary over custom", func(t *testing.T) {
		panels := []DocumentPanel{
			{TemplateSlug: "custom-notes", Content: "<p>custom</p>", ContentUpdatedAt: "2026-07-01T00:00:00Z"},
			{TemplateSlug: "meeting-summary-consolidated", Content: "<p>summary</p>", ContentUpdatedAt: "2026-06-01T00:00:00Z"},
		}
		got := selectSummaryPanel(panels)
		if got == nil || got.TemplateSlug != "meeting-summary-consolidated" {
			t.Fatalf("expected meeting-summary panel, got %+v", got)
		}
	})

	t.Run("skips deleted and empty", func(t *testing.T) {
		panels := []DocumentPanel{
			{TemplateSlug: "meeting-summary-consolidated", Content: "<p>x</p>", DeletedAt: "2026-07-01T00:00:00Z"},
			{TemplateSlug: "meeting-summary-consolidated", Content: "   "},
			{TemplateSlug: "custom", Content: "<p>only real one</p>"},
		}
		got := selectSummaryPanel(panels)
		if got == nil || got.TemplateSlug != "custom" {
			t.Fatalf("expected the non-deleted non-empty panel, got %+v", got)
		}
	})

	t.Run("newest content wins among equals", func(t *testing.T) {
		panels := []DocumentPanel{
			{TemplateSlug: "meeting-summary-a", Content: "<p>old</p>", ContentUpdatedAt: "2026-06-01T00:00:00Z"},
			{TemplateSlug: "meeting-summary-b", Content: "<p>new</p>", ContentUpdatedAt: "2026-07-01T00:00:00Z"},
		}
		got := selectSummaryPanel(panels)
		if got == nil || got.TemplateSlug != "meeting-summary-b" {
			t.Fatalf("expected newest panel, got %+v", got)
		}
	})

	t.Run("no usable panels", func(t *testing.T) {
		if got := selectSummaryPanel(nil); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})
}

func TestBuildGranolaAuthURLUsesAppPKCEFlow(t *testing.T) {
	verifier := "test-verifier"
	signInClickID := "12345678-1234-4234-8234-123456789abc"
	rawURL, err := buildGranolaAuthURL("google", verifier, signInClickID, "active@example.com")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Host != "api.granola.ai" || parsed.Path != "/v1/auth" {
		t.Fatalf("unexpected auth endpoint: %s", parsed.String())
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	query := parsed.Query()
	checks := map[string]string{
		"provider":         "google",
		"code_challenge":   wantChallenge,
		"sign_in_click_id": signInClickID,
		"platform":         "macOS",
		"redirect":         granolaAuthWebRedirect,
		"login_hint":       "active@example.com",
		"select_account":   "true",
	}
	for key, want := range checks {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestGranolaTokenExternalID(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"external_id": "active-user", "exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	if got := granolaTokenExternalID(&GranolaTokens{AccessToken: token}); got != "active-user" {
		t.Fatalf("external ID = %q, want active-user", got)
	}
	if got := granolaTokenExternalID(&GranolaTokens{AccessToken: token, ExternalID: "response-user"}); got != "response-user" {
		t.Fatalf("response external ID = %q, want response-user", got)
	}
}

func TestWaitForGranolaAuthRedirectUsesMatchingBrowserCallback(t *testing.T) {
	const signInClickID = "12345678-1234-4234-8234-123456789abc"
	callback := "https://www.granola.ai/app-redirect?code=one-time-code&signInClickId=" + signInClickID
	calls := 0
	finder := func(gotSignInClickID string) (string, error) {
		calls++
		if gotSignInClickID != signInClickID {
			t.Fatalf("finder received sign-in click ID %q", gotSignInClickID)
		}
		return callback, nil
	}

	got, err := waitForGranolaAuthRedirect(t.Context(), nil, time.Second, signInClickID, finder)
	if err != nil {
		t.Fatal(err)
	}
	if got != callback {
		t.Fatalf("got callback %q, want %q", got, callback)
	}
	if calls != 1 {
		t.Fatalf("finder called %d times, want 1", calls)
	}
}

func TestFindGranolaAuthRedirectInBreadcrumbFile(t *testing.T) {
	const signInClickID = "12345678-1234-4234-8234-123456789abc"
	want := "granola://login-complete?code=one-time-code&signInClickId=" + signInClickID
	state := map[string]any{
		"scope": map[string]any{
			"breadcrumbs": []any{
				map[string]any{"data": map[string]any{"arguments": []any{`{"url":"granola://login-complete?code=stale&signInClickId=old-attempt"}`}}},
				map[string]any{"data": map[string]any{"arguments": []any{"timestamp", "handling-granola-url", `{"url":"` + want + `"}`}}},
			},
		},
	}
	contents, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scope_v3.json")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}

	got, err := findGranolaAuthRedirectInBreadcrumbFile(path, signInClickID)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got callback %q, want %q", got, want)
	}
	if got, err := findGranolaAuthRedirectInBreadcrumbFile(path, "unknown-attempt"); err != nil || got != "" {
		t.Fatalf("unexpected callback for unknown attempt: %q, err=%v", got, err)
	}
}

func TestParseGranolaAuthCallbackValidatesState(t *testing.T) {
	const signInClickID = "12345678-1234-4234-8234-123456789abc"
	callbacks := []string{
		"https://www.granola.ai/app-redirect?code=one-time-code&platform=macos&signInClickId=" + signInClickID,
		"granola://login-complete?code=one-time-code&signInClickId=" + signInClickID,
	}
	for _, callback := range callbacks {
		code, err := parseGranolaAuthCallback(callback, signInClickID)
		if err != nil {
			t.Fatal(err)
		}
		if code != "one-time-code" {
			t.Fatalf("got code %q", code)
		}
	}
	if _, err := parseGranolaAuthCallback(callbacks[0], "different-attempt"); err == nil {
		t.Fatal("expected mismatched-state error")
	}
}

func TestGranolaTokensNeedRefresh(t *testing.T) {
	now := time.Now()
	if granolaTokensNeedRefresh(&GranolaTokens{AccessToken: testJWT(t, now.Add(10*time.Minute))}, now) {
		t.Fatal("fresh token should not need refresh")
	}
	if !granolaTokensNeedRefresh(&GranolaTokens{AccessToken: testJWT(t, now.Add(time.Minute))}, now) {
		t.Fatal("token expiring inside app refresh window should need refresh")
	}
}

func TestLoadAccessTokenFromDirReportsSourcesAccurately(t *testing.T) {
	dir := t.TempDir()
	expiredToken := testJWT(t, time.Now().Add(-time.Hour))
	writePlainCredentials(t, dir, expiredToken)

	_, err := loadAccessTokenFromDir(dir)
	if err == nil {
		t.Fatal("expected expired-token error")
	}
	message := err.Error()
	if !strings.Contains(message, "encrypted: not present") || !strings.Contains(message, "plaintext: access token expired") {
		t.Fatalf("error did not preserve per-source failures: %s", message)
	}
	if strings.Contains(message, "both") {
		t.Fatalf("error incorrectly claims both tokens expired: %s", message)
	}
}

func TestLoadAccessTokenFromDirUsesLivePlaintextFallback(t *testing.T) {
	dir := t.TempDir()
	want := testJWT(t, time.Now().Add(time.Hour))
	writePlainCredentials(t, dir, want)

	got, err := loadAccessTokenFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got token %q, want %q", got, want)
	}
}

func TestDecodeDocumentsPageSupportsCurrentBulkAndTargetedShapes(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantArray bool
	}{
		{name: "v1 bulk array", input: `[{"id":"bulk-doc"}]`, wantArray: true},
		{name: "v2 targeted object", input: `{"docs":[{"id":"targeted-doc"}],"deleted":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, arrayResponse, err := decodeDocumentsPage([]byte(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			if arrayResponse != tt.wantArray {
				t.Fatalf("array response = %v, want %v", arrayResponse, tt.wantArray)
			}
			if len(response.Docs) != 1 || response.Docs[0].ID == "" {
				t.Fatalf("unexpected decoded documents: %+v", response.Docs)
			}
		})
	}
}

func TestSessionAccountMismatchPrefersEmail(t *testing.T) {
	idToken := testJWTWithClaims(t, map[string]any{"email": "Russ@Example.com"})
	access := testJWTWithClaims(t, map[string]any{"external_id": "workos-user"})
	tokens := &GranolaTokens{AccessToken: access, IDToken: idToken}

	if got := sessionAccountMismatch(tokens, granolaAppIdentity{ID: "granola-uuid", Email: "russ@example.com"}); got != "" {
		t.Fatalf("matching email should override an ID namespace difference, got %q", got)
	}
	if got := sessionAccountMismatch(tokens, granolaAppIdentity{ID: "granola-uuid", Email: "other@example.com"}); got == "" {
		t.Fatal("different email should be reported")
	}
	if got := sessionAccountMismatch(&GranolaTokens{AccessToken: access}, granolaAppIdentity{ID: "granola-uuid"}); got == "" {
		t.Fatal("ID mismatch without emails should be reported")
	}
	if got := sessionAccountMismatch(&GranolaTokens{AccessToken: access}, granolaAppIdentity{}); got != "" {
		t.Fatalf("nothing to compare should not be reported, got %q", got)
	}
	if got := granolaTokenEmail(tokens); got != "Russ@Example.com" {
		t.Fatalf("email = %q", got)
	}
}

func TestPanelContentAcceptsObjectOrString(t *testing.T) {
	raw := `[
		{"template_slug":"meeting-summary","content":"<p>html body</p>","content_updated_at":"2026-06-01T00:00:00Z"},
		{"template_slug":"meeting-summary-v2","content":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"structured body"}]}]},"content_updated_at":"2026-07-01T00:00:00Z"},
		{"template_slug":"empty","content":null}
	]`
	var panels []DocumentPanel
	if err := json.Unmarshal([]byte(raw), &panels); err != nil {
		t.Fatal(err)
	}
	if got := panelContentToMarkdown(string(panels[0].Content)); !strings.Contains(got, "html body") {
		t.Fatalf("html panel markdown = %q", got)
	}
	if got := panelContentToMarkdown(string(panels[1].Content)); !strings.Contains(got, "structured body") {
		t.Fatalf("object panel markdown = %q", got)
	}
	if panels[2].Content != "" {
		t.Fatalf("null content should be empty, got %q", panels[2].Content)
	}
	if got := selectSummaryPanel(panels); got == nil || got.TemplateSlug != "meeting-summary-v2" {
		t.Fatalf("expected newest meeting-summary panel, got %+v", got)
	}
}

func TestBuildFrontmatterPreservesHandEditedBlocks(t *testing.T) {
	doc := GranolaDocument{ID: "doc-1", Title: "Sync", CreatedAt: "2026-07-01T21:00:39.015Z", UpdatedAt: "2026-07-06T14:59:16.594Z"}
	path := filepath.Join(t.TempDir(), "note.md")
	existing := "---\ngranola_id: doc-1\ntitle: \"Sync\"\nupdated_at: 2026-07-01T22:29:01.481Z\nhas_transcript: true\nwith:\n  - \"[[Logan Harris]]\"\n  - \"[[Spotter Global]]\"\ntags:\n  - meeting\n  - portfolio\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	got := buildFrontmatter(doc, true, readFrontmatterBlocks(path))
	for _, want := range []string{"granola_id: doc-1", "updated_at: 2026-07-06T14:59:16.594Z", "  - \"[[Logan Harris]]\"", "  - \"[[Spotter Global]]\"", "  - portfolio"} {
		if !strings.Contains(got, want) {
			t.Fatalf("frontmatter missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "with: []") || strings.Contains(got, "2026-07-01T22:29:01.481Z") {
		t.Fatalf("hand-edited list replaced or stale field kept:\n%s", got)
	}
	if strings.Count(got, "with:") != 1 || strings.Count(got, "tags:") != 1 {
		t.Fatalf("duplicated keys:\n%s", got)
	}

	fresh := buildFrontmatter(doc, false, nil)
	if !strings.Contains(fresh, "with: []\n") || !strings.Contains(fresh, "tags:\n  - meeting\n") {
		t.Fatalf("defaults missing:\n%s", fresh)
	}

	emptyPath := filepath.Join(t.TempDir(), "empty.md")
	if err := os.WriteFile(emptyPath, []byte("---\ngranola_id: doc-1\nwith: []\ntags:\n  - meeting\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := buildFrontmatter(doc, false, readFrontmatterBlocks(emptyPath)); !strings.Contains(got, "with: []\n") || strings.Count(got, "tags:") != 1 {
		t.Fatalf("empty with should fall back to default without duplication:\n%s", got)
	}
	if readFrontmatterBlocks(filepath.Join(t.TempDir(), "missing.md")) != nil {
		t.Fatal("missing file should yield nil blocks")
	}
}

func testJWT(t *testing.T, expiry time.Time) string {
	t.Helper()
	return testJWTWithClaims(t, map[string]any{"exp": expiry.Unix()})
}

func testJWTWithClaims(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func writePlainCredentials(t *testing.T, dir, token string) {
	t.Helper()
	tokenPayload, err := json.Marshal(map[string]string{"access_token": token})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := json.Marshal(map[string]string{"workos_tokens": string(tokenPayload)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "supabase.json"), credentials, 0600); err != nil {
		t.Fatal(err)
	}
}
