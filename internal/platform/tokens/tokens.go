package tokens

import "encoding/json"

func EstimateText(text string) int {
	if text == "" {
		return 0
	}
	runes := len([]rune(text))
	if runes <= 4 {
		return 1
	}
	return (runes + 3) / 4
}

func EstimateAny(value any) int {
	data, _ := json.Marshal(value)
	return EstimateText(string(data))
}
