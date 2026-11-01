//go:build !noweb

package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"masamong/internal/action"
	"masamong/internal/fsutil"
	"masamong/internal/secrets"
)

var secretID = regexp.MustCompile(`^[a-z0-9][a-z0-9_\-]{0,63}$`)

type secretTempFile interface {
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// secretTempWrite is replaceable in tests so write, permission and sync failures can be exercised
// without filling a filesystem or changing the host's ACLs.
var secretTempWrite = writeSecretTemp

func writeSecretTemp(tmp secretTempFile, value string) error {
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	body := []byte(value + "\n")
	n, err := tmp.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	return tmp.Sync()
}

// keychainAccount is <instance>/<id>, so two instances on one Mac do not share an item.
func keychainAccount(instance, id string) string {
	inst := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(instance, "-")
	if inst == "" || !secrets.ValidAccount(inst) {
		inst = "masamong"
	}
	return inst + "/" + id
}

// fileFallbackNote tells why a key goes to a file instead of the system credential store, and how that
// file is protected on this system: mode 0600, or on Windows an ACL for the current user only.
func fileFallbackNote(err error) string {
	where := "a private file (mode 0600)"
	if runtime.GOOS == "windows" {
		where = "a file only your Windows account can open"
	}
	if errors.Is(err, secrets.ErrKeychainValueTooLong) {
		return "this key is longer than the system credential store accepts; storing it in " + where + " in the data folder instead"
	}
	return "the system credential store is unavailable; storing the key in " + where + " in the data folder"
}

// storedSecret is a key written to its store, not yet committed to config. rollback puts the previous
// value back; commit drops the backup and removes an older console-written key file the new reference
// replaced.
type storedSecret struct {
	ref      string
	where    string // keychain | file
	note     string // why a key is in a file although a credential store exists (empty otherwise)
	rollback func()
	commit   func(replaceOld bool)
}

// storeSecret writes value to the system credential store when available (macOS keychain, Windows
// Credential Manager), otherwise to <data-root>/secrets/<id>.key, private to the owner.
func (s *Server) storeSecret(id, value, previousRef string) (storedSecret, error) {
	dir := filepath.Join(s.svc.DataRoot, "secrets")
	cleanupOld := func(newRef string) {
		// only files this console wrote are removed, and never the one the new reference uses
		if p, ok := strings.CutPrefix(previousRef, "file:"); ok && "file:"+p != newRef && filepath.Dir(p) == dir {
			_ = os.Remove(p)
		}
	}
	fallbackNote := ""
	if secrets.KeychainSupported() {
		acct := keychainAccount(s.svc.Doc().Instance.ID, id)
		old, readErr := secrets.KeychainRead(acct)
		if err := secrets.KeychainWrite(acct, value); err == nil {
			ref := "keychain:" + acct
			return storedSecret{ref: ref, where: "keychain",
				rollback: func() {
					if readErr == nil && old != "" {
						_ = secrets.KeychainWrite(acct, old)
					} else {
						_ = secrets.KeychainDelete(acct)
					}
				},
				commit: func(replaceOld bool) {
					if replaceOld {
						cleanupOld(ref)
					}
				}}, nil
		} else {
			note := fileFallbackNote(err)
			s.log.Warn(note, "err", err)
			fallbackNote = note
		}
	}
	if err := fsutil.PrivateDir(dir); err != nil {
		return storedSecret{}, err
	}
	path := filepath.Join(dir, id+".key")
	tmp, err := os.CreateTemp(dir, "."+id+"-*.tmp")
	if err != nil {
		return storedSecret{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	writeErr := secretTempWrite(tmp, value)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return storedSecret{}, err
	}
	if err := fsutil.PrivateFile(tmpPath); err != nil {
		return storedSecret{}, err
	}
	// Keep the previous credential until the replacement is fully written; the backup restores it on
	// every failure.
	backup := ""
	if _, err := os.Lstat(path); err == nil {
		b, err := os.CreateTemp(dir, "."+id+"-*.bak")
		if err != nil {
			return storedSecret{}, err
		}
		backup = b.Name()
		_ = b.Close()
		_ = os.Remove(backup)
		if err := os.Rename(path, backup); err != nil {
			return storedSecret{}, err
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if backup != "" {
			_ = os.Rename(backup, path)
		}
		return storedSecret{}, err
	}
	ref := "file:" + path
	return storedSecret{ref: ref, where: "file", note: fallbackNote,
		rollback: func() {
			_ = os.Remove(path)
			if backup != "" {
				_ = os.Rename(backup, path)
			}
		},
		commit: func(replaceOld bool) {
			if backup != "" {
				_ = os.Remove(backup)
			}
			if replaceOld {
				cleanupOld(ref)
			}
		}}, nil
}

// apiSecret stores a key the operator typed and points the provider (or the Discord token, or an HTTP
// tool) at it. The value never enters an Action, the change log or a response: Actions only receive
// the reference.
func (s *Server) apiSecret(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.requireAPI(w, r, true)
	if !ok {
		return
	}
	if r.Header.Get(practiceHeader) == "1" {
		apiError(w, http.StatusConflict, "INVALID_INPUT", "keys are not stored in practice mode")
		return
	}
	var body struct {
		Target string `json:"target"` // provider | discord | file (returns the reference only)
		ID     string `json:"id"`
		Value  string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_INPUT", "bad request")
		return
	}
	value := strings.TrimSpace(body.Value)
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_INPUT", "enter the key on one line")
		return
	}
	id := body.ID
	switch body.Target {
	case "discord":
		id = "discord"
	case "file":
		// operator HTTP tools reference the key themselves (tools.<id>.http.credential_ref)
		id = "tool-" + strings.NewReplacer(".", "-", "_", "-").Replace(body.ID)
	}
	if !secretID.MatchString(id) {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_INPUT", "invalid id")
		return
	}
	previous := ""
	switch body.Target {
	case "provider":
		p, ok := s.svc.Doc().Providers[id]
		if !ok {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_INPUT", "unknown provider "+id)
			return
		}
		previous = p.CredentialRef
	case "discord":
		previous = s.svc.Doc().Discord.TokenRef
	case "file":
		if t, ok := s.svc.Doc().Tools[body.ID]; ok && t.HTTP != nil {
			previous = t.HTTP.CredentialRef
		}
	default:
		apiError(w, http.StatusUnprocessableEntity, "INVALID_INPUT", "target must be provider, discord, or file")
		return
	}
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	st, err := s.storeSecret(id, value, previous)
	if err != nil {
		s.log.Error("store key", "err", err)
		apiError(w, http.StatusInternalServerError, "IO", "cannot store key")
		return
	}
	var req action.Request
	switch body.Target {
	case "provider":
		raw, _ := json.Marshal(map[string]any{"id": id, "credential_ref": st.ref})
		req = action.Request{Action: "providers.credentials", Input: raw}
	case "discord":
		raw, _ := json.Marshal(map[string]any{"ops": []any{map[string]any{"path": []any{"discord", "token_ref"}, "value": st.ref}}})
		req = action.Request{Action: "config.patch", Input: raw}
	case "file":
		// the tool wizard saves the reference with the tool; an older key file stays until then
		st.commit(false)
		out := map[string]any{"ok": true, "credential_ref": st.ref, "stored_in": st.where}
		if st.note != "" {
			out["warning"] = st.note
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	req.Locale, req.RequestID, req.Operator = s.langFor(r), "web", operator(sess)
	res := s.svc.Execute(r.Context(), req)
	if !res.OK {
		st.rollback()
	} else {
		st.commit(true)
		if res.Output == nil {
			res.Output = map[string]any{}
		}
		res.Output["stored_in"] = st.where
		if st.note != "" {
			res.Output["warning"] = st.note
		}
	}
	writeJSON(w, statusFor(res), res)
}
