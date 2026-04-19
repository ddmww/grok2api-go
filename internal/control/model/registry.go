package model

type Spec struct {
	Name       string
	Mode       string
	Pool       string
	PublicName string
	Enabled    bool
	PreferBest bool
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

var models = []Spec{
	{Name: "grok-4.20-0309-non-reasoning", Mode: "fast", Pool: "basic", Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning"},
	{Name: "grok-4.20-0309", Mode: "auto", Pool: "basic", Enabled: true, PublicName: "Grok 4.20 0309"},
	{Name: "grok-4.20-0309-reasoning", Mode: "expert", Pool: "basic", Enabled: true, PublicName: "Grok 4.20 0309 Reasoning"},
	{Name: "grok-4.20-0309-non-reasoning-super", Mode: "fast", Pool: "super", Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning Super"},
	{Name: "grok-4.20-0309-super", Mode: "auto", Pool: "super", Enabled: true, PublicName: "Grok 4.20 0309 Super"},
	{Name: "grok-4.20-0309-reasoning-super", Mode: "expert", Pool: "super", Enabled: true, PublicName: "Grok 4.20 0309 Reasoning Super"},
	{Name: "grok-4.20-0309-non-reasoning-heavy", Mode: "fast", Pool: "heavy", Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning Heavy"},
	{Name: "grok-4.20-0309-heavy", Mode: "auto", Pool: "heavy", Enabled: true, PublicName: "Grok 4.20 0309 Heavy"},
	{Name: "grok-4.20-0309-reasoning-heavy", Mode: "expert", Pool: "heavy", Enabled: true, PublicName: "Grok 4.20 0309 Reasoning Heavy"},
	{Name: "grok-4.20-multi-agent-0309", Mode: "heavy", Pool: "heavy", Enabled: true, PublicName: "Grok 4.20 Multi-Agent 0309"},
	{Name: "grok-4.20-fast", Mode: "fast", Pool: "basic", Enabled: true, PublicName: "Grok 4.20 Fast", PreferBest: true},
	{Name: "grok-4.20-auto", Mode: "auto", Pool: "basic", Enabled: true, PublicName: "Grok 4.20 Auto", PreferBest: true},
	{Name: "grok-4.20-expert", Mode: "expert", Pool: "basic", Enabled: true, PublicName: "Grok 4.20 Expert", PreferBest: true},
	{Name: "grok-4.20-heavy", Mode: "heavy", Pool: "heavy", Enabled: true, PublicName: "Grok 4.20 Heavy", PreferBest: true},
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
