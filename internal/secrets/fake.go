package secrets

import (
	"runtime"
	"strings"
)

// UseFakeKeychain swaps security(1) for an in-memory map so other packages' tests can exercise
// keychain: references without touching a real keychain. It returns a function that restores the
// real one. Not for production use.
func UseFakeKeychain(store map[string]string) (restore func()) {
	oldExec, oldSup := keychainExec, keychainSupported
	oldWindowsRead, oldWindowsWrite, oldWindowsDelete := windowsCredentialRead, windowsCredentialWrite, windowsCredentialDelete
	keychainSupported = func() bool { return true }
	keychainExec = func(stdin string, args ...string) ([]byte, int, error) {
		switch args[0] {
		case "find-generic-password":
			if v, ok := store[args[4]]; ok {
				return []byte(v + "\n"), 0, nil
			}
			return nil, secItemNotFound, nil
		case "delete-generic-password":
			if _, ok := store[args[4]]; !ok {
				return nil, secItemNotFound, nil
			}
			delete(store, args[4])
			return nil, 0, nil
		case "-i":
			toks := splitQuoted(stdin)
			var acct, val string
			for i := 0; i+1 < len(toks); i++ {
				switch toks[i] {
				case "-a":
					acct = toks[i+1]
				case "-w":
					val = toks[i+1]
				}
			}
			store[acct] = val
			return nil, 0, nil
		}
		return nil, 1, nil
	}
	if runtime.GOOS == "windows" {
		windowsCredentialRead = func(account string) (string, error) { return store[account], nil }
		windowsCredentialWrite = func(account, value string) error { store[account] = value; return nil }
		windowsCredentialDelete = func(account string) error { delete(store, account); return nil }
	}
	return func() {
		keychainExec, keychainSupported = oldExec, oldSup
		windowsCredentialRead, windowsCredentialWrite, windowsCredentialDelete = oldWindowsRead, oldWindowsWrite, oldWindowsDelete
		cacheMu.Lock()
		cache = map[string]cached{}
		cacheMu.Unlock()
	}
}

// splitQuoted reads security -i's tokens: double quotes group, backslash escapes inside quotes.
func splitQuoted(line string) []string {
	var out []string
	var cur strings.Builder
	in, esc, have := false, false, false
	for _, r := range strings.TrimSpace(line) {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && in:
			esc = true
		case r == '"':
			in, have = !in, true
		case r == ' ' && !in:
			if have || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if have || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
