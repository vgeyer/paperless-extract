package extract

import "unicode/utf8"

const (
	ChatOverheadTokens = 128
	MinTextBudget      = 256
	TruncationMarker   = "\n\n[... document truncated ...]\n\n"
	HeadPercent        = 30
)

// EstimateTokens = ceil(runeCount/3).
func EstimateTokens(s string) int {
	return (utf8.RuneCountInString(s) + 2) / 3
}

// TextBudget is the number of tokens left for the document text (may be negative).
func TextBudget(numCtx, numPredict int, system string) int {
	return numCtx - EstimateTokens(system) - numPredict - ChatOverheadTokens
}

// FitText shortens text to the budget, keeping head and tail.
func FitText(text string, budget int) (string, bool) {
	if EstimateTokens(text) <= budget {
		return text, false
	}
	maxRunes := budget*3 - utf8.RuneCountInString(TruncationMarker)
	if maxRunes <= 0 {
		return "", true
	}
	rs := []rune(text)
	head := maxRunes * HeadPercent / 100
	tail := maxRunes - head
	return string(rs[:head]) + TruncationMarker + string(rs[len(rs)-tail:]), true
}

// NearContextLimit reports whether the prompt used at least 95% of the context.
func NearContextLimit(promptEvalCount, numCtx int) bool {
	return promptEvalCount >= numCtx*95/100
}
