package plugin

import "strings"

type configuredModel struct {
	Provider string `yaml:"provider"`
	Name     string `yaml:"name"`
	Alias    string `yaml:"alias"`
}

func (m configuredModel) canonical() string { return "backboard/" + m.Provider + "/" + m.Name }
func (m configuredModel) id() string {
	if m.Alias != "" {
		return m.Alias
	}
	return m.canonical()
}
func validateModels(models []configuredModel) error {
	seen := make(map[string]bool)
	for _, m := range models {
		for _, value := range []string{m.Provider, m.Name, m.Alias} {
			if strings.ContainsAny(value, "*?") || strings.ContainsFunc(value, func(r rune) bool { return r <= 32 || r == 127 }) {
				return fail(400, "Model identifiers must be exact and contain no whitespace, control characters, or wildcards")
			}
		}
		if m.Provider == "" || m.Name == "" || strings.Contains(m.Provider, "/") {
			return fail(400, "Each configured model requires a provider and name")
		}
		for _, id := range []string{m.canonical(), m.id()} {
			if seen[id] {
				return fail(400, "Configured model identifiers must be unique")
			}
		}
		seen[m.canonical()], seen[m.id()] = true, true
	}
	return nil
}
func (a *App) resolveModel(id string) (string, string, error) {
	a.mu.Lock()
	configured, models := a.modelsConfigured, a.modelsConfig
	a.mu.Unlock()
	if configured {
		for _, m := range models {
			if id == m.id() || id == m.canonical() {
				return m.Provider, m.Name, nil
			}
		}
		return "", "", fail(400, "Model is not in the Backboard allowlist")
	}
	provider, name, ok := strings.Cut(strings.TrimPrefix(id, "backboard/"), "/")
	if !ok || provider == "" || name == "" {
		return "", "", fail(400, "Use a provider-qualified model ID from /v1/models")
	}
	return provider, name, nil
}
