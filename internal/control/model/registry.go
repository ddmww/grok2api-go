package model

type Capability uint32

const (
	CapabilityChat Capability = 1 << iota
	CapabilityImage
	CapabilityImageEdit
	CapabilityVideo
	CapabilityVoice
)

type Spec struct {
	Name       string
	Mode       string
	Pool       string
	Capability Capability
	PublicName string
	Enabled    bool
	PreferBest bool
}

func (s Spec) IsChat() bool      { return s.Capability&CapabilityChat != 0 }
func (s Spec) IsImage() bool     { return s.Capability&CapabilityImage != 0 }
func (s Spec) IsImageEdit() bool { return s.Capability&CapabilityImageEdit != 0 }
func (s Spec) IsVideo() bool     { return s.Capability&CapabilityVideo != 0 }

func (s Spec) WithMode(mode string) Spec {
	next := s
	next.Mode = mode
	return next
}

func (s Spec) PoolCandidates() []string {
	if s.PreferBest && s.Pool != "heavy" {
		return []string{"heavy", "super", "basic"}
	}
	switch s.Pool {
	case "heavy":
		return []string{"heavy"}
	case "super":
		return []string{"super", "heavy"}
	default:
		return []string{"basic", "super", "heavy"}
	}
}

func ModeCandidates(spec Spec, autoFallback bool) []string {
	if spec.IsChat() && spec.Mode == "auto" && autoFallback {
		return []string{"auto", "fast", "expert"}
	}
	return []string{spec.Mode}
}

var models = []Spec{
	{Name: "grok-4.20-0309-non-reasoning", Mode: "fast", Pool: "basic", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning"},
	{Name: "grok-4.20-0309", Mode: "auto", Pool: "super", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 0309"},
	{Name: "grok-4.20-0309-reasoning", Mode: "expert", Pool: "super", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning"},
	{Name: "grok-4.20-0309-non-reasoning-super", Mode: "fast", Pool: "super", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning Super"},
	{Name: "grok-4.20-0309-super", Mode: "auto", Pool: "super", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 0309 Super"},
	{Name: "grok-4.20-0309-reasoning-super", Mode: "expert", Pool: "super", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning Super"},
	{Name: "grok-4.20-0309-non-reasoning-heavy", Mode: "fast", Pool: "heavy", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning Heavy"},
	{Name: "grok-4.20-0309-heavy", Mode: "auto", Pool: "heavy", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 0309 Heavy"},
	{Name: "grok-4.20-0309-reasoning-heavy", Mode: "expert", Pool: "heavy", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning Heavy"},
	{Name: "grok-4.20-multi-agent-0309", Mode: "heavy", Pool: "heavy", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent 0309"},
	{Name: "grok-4.20-fast", Mode: "fast", Pool: "basic", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 Fast", PreferBest: true},
	{Name: "grok-4.20-auto", Mode: "auto", Pool: "super", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 Auto", PreferBest: true},
	{Name: "grok-4.20-expert", Mode: "expert", Pool: "super", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 Expert", PreferBest: true},
	{Name: "grok-4.20-heavy", Mode: "heavy", Pool: "heavy", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.20 Heavy", PreferBest: true},
	{Name: "grok-4.3-beta", Mode: "grok-420-computer-use-sa", Pool: "super", Capability: CapabilityChat, Enabled: true, PublicName: "Grok 4.3 Beta"},
	{Name: "grok-imagine-image-lite", Mode: "fast", Pool: "basic", Capability: CapabilityImage, Enabled: true, PublicName: "Grok Imagine Image Lite"},
	{Name: "grok-imagine-image", Mode: "auto", Pool: "super", Capability: CapabilityImage, Enabled: true, PublicName: "Grok Imagine Image"},
	{Name: "grok-imagine-image-pro", Mode: "auto", Pool: "super", Capability: CapabilityImage, Enabled: true, PublicName: "Grok Imagine Image Pro"},
	{Name: "grok-imagine-image-edit", Mode: "auto", Pool: "super", Capability: CapabilityImageEdit, Enabled: true, PublicName: "Grok Imagine Image Edit"},
	{Name: "grok-imagine-video", Mode: "auto", Pool: "super", Capability: CapabilityVideo, Enabled: true, PublicName: "Grok Imagine Video"},
}

func All() []Spec {
	out := make([]Spec, 0, len(models))
	for _, item := range models {
		if item.Enabled {
			out = append(out, item)
		}
	}
	return out
}

func Get(name string) (Spec, bool) {
	for _, item := range models {
		if item.Name == name {
			return item, true
		}
	}
	return Spec{}, false
}
