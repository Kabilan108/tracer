package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const filterFixture = `---
session_id: fixture
---

_**User**_

Keep this.

<think><details><summary>Thought Process</summary>
private reasoning
</details></think>

<tool-use data-tool-type="shell" data-tool-name="exec"><details>
<summary>Run command</summary>
line one
line two
line three
</details></tool-use>

Between blocks.

<tool-use data-tool-type="file" data-tool-name="read"><details>
<summary>Read file</summary>
alpha
beta
</details></tool-use>

_**Agent**_

Keep that too.
`

func TestFilterTranscriptModes(t *testing.T) {
	tests := []struct {
		name        string
		opts        FilterOptions
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "full",
			opts:        FilterOptions{ToolOutput: ToolOutputFull},
			wantPresent: []string{"private reasoning", "line one", "line three", "alpha", "beta"},
		},
		{
			name:        "none",
			opts:        FilterOptions{ToolOutput: ToolOutputNone},
			wantPresent: []string{"private reasoning", "<summary>Run command</summary>", "<summary>Read file</summary>", "Between blocks."},
			wantAbsent:  []string{"line one", "line three", "alpha", "beta"},
		},
		{
			name:        "truncate",
			opts:        FilterOptions{ToolOutput: ToolOutputTruncate, TruncateLines: 2},
			wantPresent: []string{"private reasoning", "line one\nline two\n... (truncated)", "alpha\nbeta\n</details></tool-use>"},
			wantAbsent:  []string{"line three"},
		},
		{
			name:        "turns only",
			opts:        FilterOptions{ToolOutput: ToolOutputTruncate, TruncateLines: 2, TurnsOnly: true},
			wantPresent: []string{"Keep this.", "Between blocks.", "Keep that too."},
			wantAbsent:  []string{"<think", "private reasoning", "<tool-use", "Run command", "line one", "Read file", "alpha"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterTranscript(filterFixture, tt.opts)
			for _, want := range tt.wantPresent {
				if !strings.Contains(got, want) {
					t.Errorf("FilterTranscript() missing %q in:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.wantAbsent {
				if strings.Contains(got, unwanted) {
					t.Errorf("FilterTranscript() unexpectedly contains %q in:\n%s", unwanted, got)
				}
			}
		})
	}
}

func TestFilterTranscriptFullIsByteIdentical(t *testing.T) {
	input := "---\r\nsession_id: exact\r\n---\r\n\x00<tool-use data-x=\"y\">body</tool-use>\r\n"
	if got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputFull}); got != input {
		t.Errorf("FilterTranscript() changed full output:\ngot  %q\nwant %q", got, input)
	}
}

func TestFilterTranscriptPreservesFrontmatter(t *testing.T) {
	frontmatter := "---\nsession_id: fixture\nnote: \"<tool-use>frontmatter</tool-use>\"\n---\n"
	input := frontmatter + filterFixture[strings.Index(filterFixture, "\n\n")+2:]
	tests := []FilterOptions{
		{ToolOutput: ToolOutputFull},
		{ToolOutput: ToolOutputNone},
		{ToolOutput: ToolOutputTruncate, TruncateLines: 1},
		{ToolOutput: ToolOutputFull, TurnsOnly: true},
	}
	for _, opts := range tests {
		if got := FilterTranscript(input, opts); !strings.HasPrefix(got, frontmatter) {
			t.Errorf("FilterTranscript(%+v) did not preserve frontmatter:\n%s", opts, got)
		}
	}
}

func TestFilterTranscriptTruncateExactLines(t *testing.T) {
	input := `<tool-use data-tool-type="test" data-tool-name="lines"><details>
<summary>Lines</summary>
one
two
three
</details></tool-use>`
	want := `<tool-use data-tool-type="test" data-tool-name="lines"><details>
<summary>Lines</summary>
one
two
... (truncated)
</details></tool-use>`
	got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputTruncate, TruncateLines: 2})
	if got != want {
		t.Errorf("FilterTranscript() =\n%q\nwant\n%q", got, want)
	}
}

func TestFilterTranscriptTruncateByteCap(t *testing.T) {
	longLine := strings.Repeat("x", maxToolOutputBytes+100)
	input := "<tool-use data-tool-type=\"test\" data-tool-name=\"long\"><details>\n<summary>Long</summary>\n" + longLine + "\n</details></tool-use>"
	got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputTruncate, TruncateLines: 1})
	if strings.Count(got, "... (truncated)") != 1 {
		t.Errorf("FilterTranscript() marker count = %d, want 1", strings.Count(got, "... (truncated)"))
	}
	if strings.Count(got, "x") != maxToolOutputBytes {
		t.Errorf("FilterTranscript() retained %d body bytes, want %d", strings.Count(got, "x"), maxToolOutputBytes)
	}
}

func TestFilterTranscriptNestedToolUse(t *testing.T) {
	input := `<tool-use data-tool-type="test" data-tool-name="outer"><details>
<summary>Outer</summary>
before
<tool-use data-tool-type="test" data-tool-name="inner"><details>
<summary>Inner</summary>
inside
</details></tool-use>
after
</details></tool-use>
tail`
	want := `<tool-use data-tool-type="test" data-tool-name="outer"><details>
<summary>Outer</summary>
</details></tool-use>
tail`
	if got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputNone}); got != want {
		t.Errorf("FilterTranscript() =\n%s\nwant\n%s", got, want)
	}
}

func TestFilterTranscriptLineAnchoredAdversarialPayloads(t *testing.T) {
	input := `<tool-use data-tool-type="test" data-tool-name="outer"><details>
<summary>Summary contains stray </tool-use> text</summary>
payload before literals
stray <tool-use data-tool-type="test" data-tool-name="mid"><details>
` + "```text\n" + `"<tool-use data-tool-type=\"test\" data-tool-name=\"quoted\"><details>"
quoted fenced payload
` + "```\n" + `payload after literals
</details></tool-use>
tail`

	tests := []struct {
		name string
		opts FilterOptions
	}{
		{name: "none", opts: FilterOptions{ToolOutput: ToolOutputNone}},
		{name: "turns only", opts: FilterOptions{ToolOutput: ToolOutputFull, TurnsOnly: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterTranscript(input, tt.opts)
			for _, payload := range []string{"payload before literals", "quoted fenced payload", "payload after literals"} {
				if strings.Contains(got, payload) {
					t.Errorf("FilterTranscript() leaked %q in:\n%s", payload, got)
				}
			}
			if !strings.Contains(got, "tail") {
				t.Errorf("FilterTranscript() dropped content after block:\n%s", got)
			}
		})
	}
}

func TestFilterTranscriptUnclosedBlockFailsOpen(t *testing.T) {
	input := "before\n<tool-use data-tool-type=\"test\" data-tool-name=\"open\"><details>\n<summary>Open</summary>\npayload\n"
	if got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputNone}); got != input {
		t.Errorf("FilterTranscript() = %q, want fail-open input %q", got, input)
	}
}

func TestFilterTranscriptMalformedTagDoesNotPanic(t *testing.T) {
	input := "<tool-use\nfoo\n</tool-use>"
	if got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputNone}); got != input {
		t.Errorf("FilterTranscript() = %q, want unchanged %q", got, input)
	}
}

func TestFilterTranscriptNoMarkerWhenComplete(t *testing.T) {
	input := `<tool-use data-tool-type="test" data-tool-name="short"><details>
<summary>Short</summary>
one
two
</details></tool-use>`
	got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputTruncate, TruncateLines: 2})
	if strings.Contains(got, "... (truncated)") {
		t.Errorf("FilterTranscript() added a marker without dropping bytes:\n%s", got)
	}
}

func TestFilterTranscriptMultilineSummary(t *testing.T) {
	input := `<tool-use data-tool-type="test" data-tool-name="summary"><details>
<summary>First line
second line</summary>
secret payload
</details></tool-use>`
	want := `<tool-use data-tool-type="test" data-tool-name="summary"><details>
<summary>First line
second line</summary>
</details></tool-use>`
	if got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputNone}); got != want {
		t.Errorf("FilterTranscript() =\n%s\nwant\n%s", got, want)
	}
}

func TestFilterTranscriptAbsentSummaryKeepsOnlyOpenLine(t *testing.T) {
	input := `<tool-use data-tool-type="test" data-tool-name="no-summary"><details>
secret payload
</details></tool-use>`
	want := `<tool-use data-tool-type="test" data-tool-name="no-summary"><details>
</details></tool-use>`
	if got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputNone}); got != want {
		t.Errorf("FilterTranscript() =\n%s\nwant\n%s", got, want)
	}
}

func TestFilterTranscriptTruncateClosesMarkdownFence(t *testing.T) {
	input := `<tool-use data-tool-type="test" data-tool-name="fence"><details>
<summary>Fence</summary>
` + "```text\n" + `line one
line two
` + "```\n" + `
</details></tool-use>`
	wantPart := "```text\nline one\n```\n... (truncated)\n</details></tool-use>"
	got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputTruncate, TruncateLines: 2})
	if !strings.Contains(got, wantPart) {
		t.Errorf("FilterTranscript() did not close truncated fence:\n%s", got)
	}
}

func TestFilterTranscriptByteCapPreservesUTF8(t *testing.T) {
	longLine := strings.Repeat("€", maxToolOutputBytes)
	input := "<tool-use data-tool-type=\"test\" data-tool-name=\"utf8\"><details>\n<summary>UTF-8</summary>\n" + longLine + "\n</details></tool-use>"
	got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputTruncate, TruncateLines: 1})
	if !utf8.ValidString(got) {
		t.Fatal("FilterTranscript() split a UTF-8 rune at the byte cap")
	}
}

func TestFilterTranscriptTurnsOnlyConsumesOneTrailingNewline(t *testing.T) {
	input := "before\n<tool-use data-tool-type=\"test\" data-tool-name=\"drop\"><details>\n<summary>Drop</summary>\npayload\n</details></tool-use>\n\nafter"
	want := "before\nafter"
	if got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputFull, TurnsOnly: true}); got != want {
		t.Errorf("FilterTranscript() = %q, want %q", got, want)
	}
}

func TestFilterTranscriptEmptyAndNoBlocks(t *testing.T) {
	tests := []string{"", "---\nsession_id: plain\n---\n\nNo blocks here.\n"}
	for _, input := range tests {
		if got := FilterTranscript(input, FilterOptions{ToolOutput: ToolOutputNone}); got != input {
			t.Errorf("FilterTranscript(%q) = %q", input, got)
		}
	}
}

func TestParseToolOutputFlag(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantMode  ToolOutputMode
		wantLines int
		wantError bool
	}{
		{name: "full", value: "full", wantMode: ToolOutputFull},
		{name: "none", value: "none", wantMode: ToolOutputNone},
		{name: "truncate", value: "truncate:12", wantMode: ToolOutputTruncate, wantLines: 12},
		{name: "explicit plus", value: "truncate:+5", wantError: true},
		{name: "zero", value: "truncate:0", wantError: true},
		{name: "negative", value: "truncate:-1", wantError: true},
		{name: "not integer", value: "truncate:many", wantError: true},
		{name: "missing count", value: "truncate:", wantError: true},
		{name: "unknown", value: "brief", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, lines, err := ParseToolOutputFlag(tt.value)
			if (err != nil) != tt.wantError {
				t.Fatalf("ParseToolOutputFlag(%q) error = %v", tt.value, err)
			}
			if tt.wantError {
				if !strings.Contains(err.Error(), "full, none, or truncate:N") {
					t.Errorf("error does not show accepted forms: %v", err)
				}
				return
			}
			if mode != tt.wantMode || lines != tt.wantLines {
				t.Errorf("ParseToolOutputFlag(%q) = %q, %d; want %q, %d", tt.value, mode, lines, tt.wantMode, tt.wantLines)
			}
		})
	}
}
