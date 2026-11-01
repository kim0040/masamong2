package application

import "masamong/internal/tools"

// CommandHint is one chat command the Discord connector registers: its canonical name (the message
// catalogs hold discord.command.<name>.name/.description per language) and the Action it runs.
type CommandHint struct {
	Name, Action string
}

// SetCommandMenu records the commands the Discord connector offers, so "what can you do?" and a request
// for something only a command does (a fortune, a notice subscription) can name the command: the
// planner never runs those tools, and without the command's name the answer model made a fortune up or
// said the bot had no such feature (2026-10-14).
func (s *Service) SetCommandMenu(hints []CommandHint) {
	cp := append([]CommandHint(nil), hints...)
	s.commandMenu.Store(&cp)
}

// commandOnly lists the commands of the command-only tools a command may run here (byCommand: ready,
// the command surface on, the channel allows it) in the request's language as {"command": "!운세",
// "about": "..."}; a tool the conversation can call is already in help's tools.
func (s *Service) commandOnly(loc string, registry []tools.Resolved, byCommand map[string]bool) []any {
	menu := s.commandMenu.Load()
	if menu == nil {
		return nil
	}
	prefix := s.Doc().Discord.CommandPrefix
	if prefix == "" {
		prefix = "!"
	}
	var out []any
	seen := map[string]bool{}
	for _, h := range *menu {
		r, ok := tools.Find(registry, h.Action)
		if !ok || r.Chat || !byCommand[h.Action] || seen[h.Name] {
			continue
		}
		seen[h.Name] = true
		name := s.commandText(loc, h.Name, "name")
		if name == "" {
			name = h.Name
		}
		about := s.commandText(loc, h.Name, "description")
		out = append(out, map[string]any{"command": prefix + name, "about": about})
	}
	return out
}

// commandText is discord.command.<name>.<field> in loc, else in English, else "".
func (s *Service) commandText(loc, name, field string) string {
	key := "discord.command." + name + "." + field
	if v, ok := s.I18n.Lookup(loc, key); ok {
		return v
	}
	v, _ := s.I18n.Lookup("en", key)
	return v
}
