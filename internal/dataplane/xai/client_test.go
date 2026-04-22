package xai

import (
	"testing"

	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/testutil"
)

func TestNewChatSessionUsesFreshClient(t *testing.T) {
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"egress": map[string]any{
				"mode": "direct",
			},
		},
	})
	runtime := proxy.NewRuntime(cfg)
	client := NewClient(cfg, runtime)

	first, err := client.NewChatSession()
	if err != nil {
		t.Fatalf("new first session: %v", err)
	}
	defer first.Close()

	second, err := client.NewChatSession()
	if err != nil {
		t.Fatalf("new second session: %v", err)
	}
	defer second.Close()

	if first.currentClient() == nil || second.currentClient() == nil {
		t.Fatal("expected chat sessions to have active clients")
	}
	if first.currentClient() == second.currentClient() {
		t.Fatal("expected independent clients per chat session")
	}
}

func TestChatSessionResetReplacesClient(t *testing.T) {
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"egress": map[string]any{
				"mode": "direct",
			},
		},
	})
	runtime := proxy.NewRuntime(cfg)
	client := NewClient(cfg, runtime)

	session, err := client.NewChatSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

	before := session.currentClient()
	if before == nil {
		t.Fatal("expected initial client")
	}
	if err := session.reset(); err != nil {
		t.Fatalf("reset session: %v", err)
	}
	after := session.currentClient()
	if after == nil {
		t.Fatal("expected client after reset")
	}
	if before == after {
		t.Fatal("expected reset to replace the underlying client")
	}
}

func TestSharedUsageSessionReusesClientUntilClosed(t *testing.T) {
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"egress": map[string]any{
				"mode": "direct",
			},
		},
	})
	runtime := proxy.NewRuntime(cfg)
	client := NewClient(cfg, runtime)

	first, err := client.SharedUsageSession()
	if err != nil {
		t.Fatalf("first shared usage session: %v", err)
	}
	second, err := client.SharedUsageSession()
	if err != nil {
		t.Fatalf("second shared usage session: %v", err)
	}
	if first != second {
		t.Fatal("expected shared usage session to be reused")
	}

	client.CloseSharedUsageSession()

	third, err := client.SharedUsageSession()
	if err != nil {
		t.Fatalf("third shared usage session: %v", err)
	}
	defer client.CloseSharedUsageSession()
	if third == first {
		t.Fatal("expected new shared usage session after close")
	}
}

func TestNewRequestSessionUsesFreshClient(t *testing.T) {
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"egress": map[string]any{
				"mode": "direct",
			},
		},
	})
	runtime := proxy.NewRuntime(cfg)
	client := NewClient(cfg, runtime)

	first, err := client.NewRequestSession(true)
	if err != nil {
		t.Fatalf("new first request session: %v", err)
	}
	defer first.Close()

	second, err := client.NewRequestSession(true)
	if err != nil {
		t.Fatalf("new second request session: %v", err)
	}
	defer second.Close()

	if first.currentClient() == nil || second.currentClient() == nil {
		t.Fatal("expected request sessions to have active clients")
	}
	if first.currentClient() == second.currentClient() {
		t.Fatal("expected independent clients per request session")
	}
}
