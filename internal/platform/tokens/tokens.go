package tokens

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"unicode"
)

type provider string

const (
	providerOpenAI provider = "openai"
	providerGemini provider = "gemini"
	providerClaude provider = "claude"
)

type multipliers struct {
	Word       float64
	Number     float64
	CJK        float64
	Symbol     float64
	MathSymbol float64
	URLDelim   float64
	AtSign     float64
	Emoji      float64
	Newline    float64
	Space      float64
	BasePad    int
}

var (
	providerMultipliers = map[provider]multipliers{
		providerGemini: {
			Word: 1.15, Number: 2.8, CJK: 0.68, Symbol: 0.38, MathSymbol: 1.05, URLDelim: 1.2, AtSign: 2.5, Emoji: 1.08, Newline: 1.15, Space: 0.2,
		},
		providerClaude: {
			Word: 1.13, Number: 1.63, CJK: 1.21, Symbol: 0.4, MathSymbol: 4.52, URLDelim: 1.26, AtSign: 2.82, Emoji: 2.6, Newline: 0.89, Space: 0.39,
		},
		providerOpenAI: {
			Word: 1.02, Number: 1.55, CJK: 0.85, Symbol: 0.4, MathSymbol: 2.68, URLDelim: 1.0, AtSign: 2.0, Emoji: 2.12, Newline: 0.5, Space: 0.42,
		},
	}
	providerMultipliersMu sync.RWMutex
)

func EstimateText(text string) int {
	return EstimateTextByModel("", text)
}

func EstimateTextByModel(modelName, text string) int {
	if text == "" {
		return 0
	}
	return estimateTokenForProvider(providerForModel(modelName), text)
}

func EstimateAny(value any) int {
	return EstimateAnyByModel("", value)
}

func EstimateAnyByModel(modelName string, value any) int {
	data, _ := json.Marshal(value)
	return EstimateTextByModel(modelName, string(data))
}

func providerForModel(modelName string) provider {
	name := strings.ToLower(strings.TrimSpace(modelName))
	switch {
	case strings.Contains(name, "gemini"):
		return providerGemini
	case strings.Contains(name, "claude"):
		return providerClaude
	default:
		return providerOpenAI
	}
}

func estimateTokenForProvider(kind provider, text string) int {
	m := getMultipliers(kind)
	var count float64

	type wordType int
	const (
		wordNone wordType = iota
		wordLatin
		wordNumber
	)

	currentWordType := wordNone
	for _, r := range text {
		if unicode.IsSpace(r) {
			currentWordType = wordNone
			if r == '\n' || r == '\t' {
				count += m.Newline
			} else {
				count += m.Space
			}
			continue
		}

		if isCJK(r) {
			currentWordType = wordNone
			count += m.CJK
			continue
		}

		if isEmoji(r) {
			currentWordType = wordNone
			count += m.Emoji
			continue
		}

		if isLatinOrNumber(r) {
			nextType := wordLatin
			if unicode.IsNumber(r) {
				nextType = wordNumber
			}
			if currentWordType == wordNone || currentWordType != nextType {
				if nextType == wordNumber {
					count += m.Number
				} else {
					count += m.Word
				}
				currentWordType = nextType
			}
			continue
		}

		currentWordType = wordNone
		switch {
		case isMathSymbol(r):
			count += m.MathSymbol
		case r == '@':
			count += m.AtSign
		case isURLDelim(r):
			count += m.URLDelim
		default:
			count += m.Symbol
		}
	}

	return int(math.Ceil(count)) + m.BasePad
}

func getMultipliers(kind provider) multipliers {
	providerMultipliersMu.RLock()
	defer providerMultipliersMu.RUnlock()
	if value, ok := providerMultipliers[kind]; ok {
		return value
	}
	return providerMultipliers[providerOpenAI]
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		(r >= 0x3040 && r <= 0x30FF) ||
		(r >= 0xAC00 && r <= 0xD7A3)
}

func isLatinOrNumber(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func isEmoji(r rune) bool {
	return (r >= 0x1F300 && r <= 0x1F9FF) ||
		(r >= 0x2600 && r <= 0x26FF) ||
		(r >= 0x2700 && r <= 0x27BF) ||
		(r >= 0x1F600 && r <= 0x1F64F) ||
		(r >= 0x1F900 && r <= 0x1F9FF) ||
		(r >= 0x1FA00 && r <= 0x1FAFF)
}

func isMathSymbol(r rune) bool {
	mathSymbols := "∑∫∂√∞≤≥≠≈±×÷∈∉∋∌⊂⊃⊆⊇∪∩∧∨¬∀∃∄∅∆∇∝∟∠∡∢°′″‴⁺⁻⁼⁽⁾ⁿ₀₁₂₃₄₅₆₇₈₉₊₋₌₍₎²³¹⁴⁵⁶⁷⁸⁹⁰"
	for _, item := range mathSymbols {
		if r == item {
			return true
		}
	}
	return (r >= 0x2200 && r <= 0x22FF) ||
		(r >= 0x2A00 && r <= 0x2AFF) ||
		(r >= 0x1D400 && r <= 0x1D7FF)
}

func isURLDelim(r rune) bool {
	for _, item := range "/:?&=;#%" {
		if r == item {
			return true
		}
	}
	return false
}
