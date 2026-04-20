package xai

import "testing"

func TestSelectFinalOrPartialImages(t *testing.T) {
	t.Run("prefers final over partial", func(t *testing.T) {
		selected, stage := selectFinalOrPartialImages([]GeneratedImage{
			{URL: "https://example.com/preview.png", ImageID: "a", Stage: "preview", Progress: 20},
			{URL: "https://example.com/final.png", ImageID: "b", Stage: "final", Progress: 100, IsFinal: true},
		}, 1)
		if len(selected) != 1 || selected[0].URL != "https://example.com/final.png" || stage != "final" {
			t.Fatalf("unexpected selection: %#v stage=%s", selected, stage)
		}
	})

	t.Run("falls back to best partial", func(t *testing.T) {
		selected, stage := selectFinalOrPartialImages([]GeneratedImage{
			{URL: "https://example.com/preview.png", ImageID: "a", Stage: "preview", Progress: 20},
			{URL: "https://example.com/medium.png", ImageID: "b", Stage: "medium", Progress: 60},
		}, 1)
		if len(selected) != 1 || selected[0].URL != "https://example.com/medium.png" || stage != "medium" {
			t.Fatalf("unexpected selection: %#v stage=%s", selected, stage)
		}
	})
}
