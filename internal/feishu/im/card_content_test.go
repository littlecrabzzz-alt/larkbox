package im

import (
	"reflect"
	"testing"
)

func TestRenderedCardMarkdownCodeAndLinks(t *testing.T) {
	for _, tc := range []struct {
		name, tag, literal string
	}{
		{"single and repeated backticks", "markdown", "literal `[single](https://example.test/single)` and `` `[double](https://example.test/double) ``"},
		{"lark markdown", "lark_md", "literal `[code](https://example.test/code)`"},
		{"backtick fence", "markdown", "\n```go\n[backtick](https://example.test/backtick)\n```\n"},
		{"tilde fence", "markdown", "\n   ~~~~ text\n[tilde](https://example.test/tilde)\n~~~\n[still code](https://example.test/still)\n~~~~~ \r\n"},
		{"unmatched backtick", "markdown", "unmatched ` followed by "},
		{"escaped backtick", "markdown", "escaped \\` followed by "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "Read [first](https://example.test/wiki/Card_(Lark)) around " + tc.literal + " [second](https://example.test/second)."
			got := renderedTextNodes(fields{"tag": tc.tag, "content": text})
			want := []fields{
				{"tag": "text", "text": "Read "},
				{"tag": "a", "text": "first", "href": "https://example.test/wiki/Card_(Lark)"},
				{"tag": "text", "text": " around " + tc.literal + " "},
				{"tag": "a", "text": "second", "href": "https://example.test/second"},
				{"tag": "text", "text": "."},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("nodes = %#v, want %#v", got, want)
			}
		})
	}
}

func TestRenderedCardMarkdownUnclosedFence(t *testing.T) {
	text := "~~~\n[literal](https://example.test/literal)"
	got := renderedTextNodes(fields{"tag": "markdown", "content": text})
	want := []fields{{"tag": "text", "text": text}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nodes = %#v, want %#v", got, want)
	}
}
