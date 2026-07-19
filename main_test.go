package main

import "testing"

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
