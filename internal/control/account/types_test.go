package account

import "testing"

func TestNormalizeToken(t *testing.T) {
	raw := "  sso=abc123 \u200b "
	if got := NormalizeToken(raw); got != "abc123" {
		t.Fatalf("normalizeToken mismatch: %q", got)
	}
}

func TestNormalizeTokenUnicodeDashesAndWhitespace(t *testing.T) {
	raw := " sso=eyJ0eXAiOiJKV1Qi\u2014abc \u200d \n"
	if got := NormalizeToken(raw); got != "eyJ0eXAiOiJKV1Qi-abc" {
		t.Fatalf("normalizeToken unicode mismatch: %q", got)
	}
}

func TestSupportedModes(t *testing.T) {
	modes := SupportedModes("heavy")
	if len(modes) != 5 || modes[3] != "heavy" || modes[4] != "grok-420-computer-use-sa" {
		t.Fatalf("unexpected heavy modes: %#v", modes)
	}
	superModes := SupportedModes("super")
	if len(superModes) != 4 || superModes[3] != "grok-420-computer-use-sa" {
		t.Fatalf("unexpected super modes: %#v", superModes)
	}
}

func TestInferPool(t *testing.T) {
	if got := InferPool(map[string]QuotaWindow{"auto": {Total: 150}}); got != "heavy" {
		t.Fatalf("expected heavy, got %q", got)
	}
	if got := InferPool(map[string]QuotaWindow{"auto": {Total: 50}}); got != "super" {
		t.Fatalf("expected super, got %q", got)
	}
	if got := InferPool(map[string]QuotaWindow{"auto": {Total: 20}}); got != "basic" {
		t.Fatalf("expected basic, got %q", got)
	}
}
