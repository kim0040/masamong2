package application

import (
	"slices"
	"strings"

	"masamong/internal/tools"
)

// ToolCommandReady reports whether a configured Discord command has the provider/model it needs.
// It is separate from ToolCommandEnabled, which only reports the operator's command switch.
func (s *Service) ToolCommandReady(action string) bool {
	r, ok := tools.Find(s.toolRegistry(), action)
	if !ok {
		return true // application commands without a matching tool have their own readiness gates
	}
	if !r.Enabled || !r.Implemented {
		return false
	}
	if len(r.Providers) > 0 {
		for _, provider := range s.Registry().Statuses() {
			if slices.Contains(r.Providers, provider.Kind) && provider.Configured && !provider.Disabled {
				return true
			}
		}
		return false
	}
	if strings.HasPrefix(action, "notices.") || strings.HasPrefix(action, "transfer.") {
		_, _, err := s.noticeCatalog()
		return err == nil
	}
	if r.Role != "" {
		adapter, err := s.llmAdapter(r.Role)
		return err == nil && adapter.Configured()
	}
	return true
}
