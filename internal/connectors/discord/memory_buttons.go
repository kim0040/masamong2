package discord

// The confirm button of /forgetme (memory.mine.forget): mem:forget:<scope>:<token>, scope p (the items a
// chat "forget that" pointed at) or a (everything about the member in this server). The token is the
// preview's fingerprint; the Action deletes only when what it would delete is still what was shown, and
// always for the person pressing (featureButton makes them the invoker).
func init() {
	extraButtons["mem"] = memoryButton
}

func memoryButton(parts []string) (string, map[string]any, bool) {
	if len(parts) != 4 || parts[1] != "forget" || parts[3] == "" {
		return "", nil, false
	}
	scope := map[string]string{"p": "pending", "a": "all"}[parts[2]]
	if scope == "" {
		return "", nil, false
	}
	return "memory.mine.forget", map[string]any{"scope": scope, "confirm": parts[3]}, true
}
