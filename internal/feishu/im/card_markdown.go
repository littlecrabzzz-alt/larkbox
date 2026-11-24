package im

import (
	"regexp"
	"strings"
)

type codeRange struct{ start, end int }

var backtickRuns = regexp.MustCompile("\x60+")

// markdownCodeRanges identifies literal code without changing the source text.
func markdownCodeRanges(text string) []codeRange {
	if !strings.ContainsAny(text, "\x60~") {
		return nil
	}
	var ranges []codeRange
	var fence byte
	var fenceWidth, fenceStart, inlineStart, offset int
	for line := range strings.SplitAfterSeq(text, "\n") {
		lineStart := offset
		offset += len(line)
		marker, width, rest := markdownFence(line)
		switch {
		case fenceWidth > 0:
			if marker == fence && width >= fenceWidth && strings.Trim(rest, " \t") == "" {
				ranges = append(ranges, codeRange{fenceStart, offset})
				fenceWidth, inlineStart = 0, offset
			}
		case width >= 3 && (marker != '\x60' || !strings.ContainsRune(rest, '\x60')):
			ranges = appendCodeSpans(ranges, text[inlineStart:lineStart], inlineStart)
			fence, fenceWidth, fenceStart = marker, width, lineStart
		case strings.TrimSpace(line) == "":
			ranges = appendCodeSpans(ranges, text[inlineStart:lineStart], inlineStart)
			inlineStart = offset
		}
	}
	if fenceWidth > 0 {
		return append(ranges, codeRange{fenceStart, len(text)})
	}
	return appendCodeSpans(ranges, text[inlineStart:], inlineStart)
}

func markdownFence(line string) (marker byte, width int, rest string) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || (trimmed[0] != '\x60' && trimmed[0] != '~') {
		return 0, 0, ""
	}
	marker = trimmed[0]
	rest = strings.TrimLeft(trimmed, string(marker))
	return marker, len(trimmed) - len(rest), strings.TrimRight(rest, "\r\n")
}

func appendCodeSpans(ranges []codeRange, text string, offset int) []codeRange {
	runs := backtickRuns.FindAllStringIndex(text, -1)
	if len(runs) < 2 {
		return ranges
	}
	// Index the next run of equal width for each potential opener.
	next := make([]int, len(runs))
	last := make(map[int]int)
	for i, run := range runs {
		width := run[1] - run[0]
		if previous, ok := last[width]; ok {
			next[previous] = i
		}
		last[width] = i
	}
	for i := 0; i < len(runs); i++ {
		if closeIndex := next[i]; closeIndex != 0 && !escapedMarkdown(text, runs[i][0]) {
			ranges = append(ranges, codeRange{offset + runs[i][0], offset + runs[closeIndex][1]})
			i = closeIndex
		}
	}
	return ranges
}

func escapedMarkdown(text string, index int) bool {
	start := index
	for start > 0 && text[start-1] == '\\' {
		start--
	}
	return (index-start)%2 != 0
}
