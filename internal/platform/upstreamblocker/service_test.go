package upstreamblocker

import "testing"

func TestNormalizeKeywords(t *testing.T) {
	got := NormalizeKeywords(" foo \n\nbar\nfoo\n")
	if len(got) != 2 || got[0] != "foo" || got[1] != "bar" {
		t.Fatalf("unexpected normalized keywords: %#v", got)
	}
}

func TestFindBlockedKeyword(t *testing.T) {
	cfg := Config{Enabled: true, CaseSensitive: false, Keywords: []string{"Blocked Word"}, Message: DefaultMessage}
	if got := FindBlockedKeyword(cfg, "this contains blocked word inside"); got != "Blocked Word" {
		t.Fatalf("unexpected match: %q", got)
	}
	if got := FindBlockedKeyword(cfg, ""); got != "" {
		t.Fatalf("expected no match for empty text, got %q", got)
	}
}

func TestFindBlockedKeywordCaseSensitive(t *testing.T) {
	cfg := Config{Enabled: true, CaseSensitive: true, Keywords: []string{"Blocked"}}
	if got := FindBlockedKeyword(cfg, "blocked"); got != "" {
		t.Fatalf("expected no match, got %q", got)
	}
	if got := FindBlockedKeyword(cfg, "Blocked"); got != "Blocked" {
		t.Fatalf("expected exact-case match, got %q", got)
	}
}

func TestAssertResponseAllowed(t *testing.T) {
	cfg := Config{Enabled: true, Keywords: []string{"blocked"}, Message: "blocked message"}
	err := AssertResponseAllowed(cfg, "blocked content", "/v1/chat/completions")
	if err == nil {
		t.Fatal("expected blocker error")
	}
	blocked, ok := err.(*Error)
	if !ok || blocked.MatchedKeyword != "blocked" || blocked.Error() != "blocked message" {
		t.Fatalf("unexpected blocker error: %#v", err)
	}
}
