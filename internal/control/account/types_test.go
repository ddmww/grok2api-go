package account

import "testing"

func TestNormalizeToken(t *testing.T) {
	raw := "  sso=abc123 \u200b "
	if got := normalizeToken(raw); got != "abc123" {
		t.Fatalf("normalizeToken mismatch: %q", got)
	}
}

func TestSupportedModes(t *testing.T) {
	modes := SupportedModes("heavy")
	if len(modes) != 4 || modes[3] != "heavy" {
		t.Fatalf("unexpected heavy modes: %#v", modes)
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
