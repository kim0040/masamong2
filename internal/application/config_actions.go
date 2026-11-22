package application

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/tools"
)

func (s *Service) configShow(req action.Request) action.Result {
	raw, _ := config.Marshal(redactDirectSecrets(s.Doc()))
	response := s.Doc().Models["response"]
	return action.OK(req, map[string]any{
		"revision":               s.Revision(),
		"yaml":                   string(raw),
		"redacted":               true,
		"web":                    s.Doc().Admin.Web,
		"instance":               s.Doc().Instance.ID,
		"profile":                s.Doc().Instance.Profile,
		"monthly_budget_usd":     s.Doc().Limits.MonthlyBudgetUSD,
		"per_request_budget_usd": s.Doc().Limits.PerRequestBudgetUSD,
		"max_external_attempts":  s.Doc().Limits.MaxExternalAttempts,
		"response_provider":      response.Provider,
		"response_model":         response.Model,
		"default_locale":         s.Doc().I18n.DefaultLocale,
		"timezone":               s.Doc().I18n.Timezone,
		"features":               s.Doc().Features,
	})
}

// redactedValue replaces a key typed directly into an HTTP tool in the exported settings. It is not a
// key, so a re-imported export does not send it anywhere as one.
const redactedValue = "<redacted: use credential_ref>"

// redactDirectSecrets returns a copy of d without keys typed directly into an HTTP tool's headers or
// URL query (where {credential} with credential_ref belongs). config.show says the settings it returns
// are redacted, and the console's export downloads them; the stored settings keep the values.
func redactDirectSecrets(d config.Document) config.Document {
	if len(d.Tools) == 0 {
		return d
	}
	out := make(map[string]tools.Settings, len(d.Tools))
	for id, t := range d.Tools {
		if t.HTTP != nil {
			h := *t.HTTP
			h.URL = redactSecretQuery(h.URL)
			if len(h.Headers) > 0 {
				h.Headers = make(map[string]string, len(t.HTTP.Headers))
				for name, v := range t.HTTP.Headers {
					if directSecretHeader(name, v) {
						v = redactedValue
					}
					h.Headers[name] = v
				}
			}
			t.HTTP = &h
		}
		out[id] = t
	}
	d.Tools = out
	return d
}

// hasDirectSecrets reports whether d holds a key typed directly into an HTTP tool (a backup that
// copies these settings then contains a secret).
func hasDirectSecrets(d config.Document) bool {
	for _, t := range d.Tools {
		if t.HTTP == nil {
			continue
		}
		if redactSecretQuery(t.HTTP.URL) != t.HTTP.URL {
			return true
		}
		for name, v := range t.HTTP.Headers {
			if directSecretHeader(name, v) {
				return true
			}
		}
	}
	return false
}

// directSecretHeader reports whether a header carries a key itself (tools.SecretHeaderName names one,
// tools.DirectHeaderValue a value that is not only the {credential} reference): validation, the doctor
// and this masking share the same classification.
func directSecretHeader(name, value string) bool {
	return tools.SecretHeaderName(name) && tools.DirectHeaderValue(value)
}

// redactSecretQuery masks the values of key-carrying query parameters that are written directly
// (not a {placeholder}). A URL it cannot read is returned unchanged.
func redactSecretQuery(raw string) string {
	base, query, ok := strings.Cut(raw, "?")
	if !ok || query == "" {
		return raw
	}
	frag := ""
	if i := strings.IndexByte(query, '#'); i >= 0 {
		query, frag = query[:i], query[i:]
	}
	parts := strings.Split(query, "&")
	changed := false
	for i, p := range parts {
		name, value, has := strings.Cut(p, "=")
		if !has || value == "" || (strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}")) {
			continue
		}
		// the name itself can be percent-encoded (api%5Fkey); classify what it decodes to, not the
		// raw spelling. A name that does not decode stays as written.
		decoded := name
		if unescaped, err := url.QueryUnescape(name); err == nil {
			decoded = unescaped
		}
		if tools.SecretQueryName(decoded) {
			parts[i] = name + "=" + url.QueryEscape(redactedValue)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	return base + "?" + strings.Join(parts, "&") + frag
}

func (s *Service) configDiff(req action.Request) action.Result {
	in := inputMap(req)
	b, res := s.candidateBytes(req, in)
	if res != nil {
		return *res
	}
	cur, _ := config.Marshal(s.Doc())
	out := map[string]any{
		"base_revision":  s.Revision(),
		"current_hash":   config.Hash(cur),
		"candidate_hash": config.Hash(b),
		"changed":        config.Hash(cur) != config.Hash(b),
	}
	if _, err := config.ValidateCandidate(b); err != nil {
		out["valid"] = false
		out["error"] = err.Error()
		return s.configFail(req, errcode.ConfigInvalid, err, out)
	}
	out["valid"] = true
	out["diff"] = lineDiff(string(cur), string(b))
	if changes, err := configChanges(cur, b); err == nil {
		out["changes"] = changes
	}
	return action.OK(req, out)
}

// lineDiff is a small LCS-free diff good enough for YAML settings: lines only in current are "-",
// only in candidate are "+". Order-preserving detail is the GUI's job when it needs more.
func lineDiff(cur, cand string) []string {
	cur = strings.ReplaceAll(strings.ReplaceAll(cur, "\r\n", "\n"), "\r", "\n")
	cand = strings.ReplaceAll(strings.ReplaceAll(cand, "\r\n", "\n"), "\r", "\n")
	curSet := map[string]int{}
	for _, l := range strings.Split(cur, "\n") {
		curSet[l]++
	}
	candSet := map[string]int{}
	for _, l := range strings.Split(cand, "\n") {
		candSet[l]++
	}
	var out []string
	for _, l := range strings.Split(cur, "\n") {
		if candSet[l] == 0 && strings.TrimSpace(l) != "" {
			out = append(out, "- "+l)
		}
	}
	for _, l := range strings.Split(cand, "\n") {
		if curSet[l] == 0 && strings.TrimSpace(l) != "" {
			out = append(out, "+ "+l)
		}
	}
	return out
}

func (s *Service) configApply(req action.Request) action.Result {
	in := inputMap(req)
	base := intVal(in, "base_revision", req.BaseRevision)
	candidate, res := s.candidateBytes(req, in)
	if res != nil {
		return *res
	}
	return s.applyCandidate(req, candidate, base)
}

// candidateBytes reads the replacement document from "yaml" (the console's editor and import, and the
// CLI, which reads --file itself) or the operator form fields (limits / model / locale / feature flags).
// It never opens a file: a path in the input would let the console read any file the service can.
func (s *Service) candidateBytes(req action.Request, in map[string]any) ([]byte, *action.Result) {
	if _, ok := in["file"]; ok {
		r := action.Fail(req, errcode.InvalidInput, "generic.error", "file is read by the masamong command; send the document as yaml", nil)
		return nil, &r
	}
	if y, ok := in["yaml"].(string); ok && strings.TrimSpace(y) != "" {
		return []byte(y), nil // preserve CRLF and the final newline; Hash canonicalizes line endings
	}
	if strVal(in, "form", "") == "operator" {
		raw, err := s.marshalOperatorForm(in)
		if err != nil {
			r := action.Fail(req, errcode.ConfigInvalid, "generic.error", err.Error(), nil)
			return nil, &r
		}
		return raw, nil
	}
	r := action.Fail(req, errcode.InvalidInput, "generic.error", "yaml required", nil)
	return nil, &r
}

func (s *Service) marshalOperatorForm(in map[string]any) ([]byte, error) {
	doc := s.Doc()
	doc.Providers = map[string]config.Provider{}
	for k, v := range s.Doc().Providers {
		doc.Providers[k] = v
	}
	doc.Models = map[string]config.Model{}
	for k, v := range s.Doc().Models {
		doc.Models[k] = v
	}
	doc.Features = map[string]bool{}
	for k, v := range s.Doc().Features {
		doc.Features[k] = v
	}
	doc.Guilds = cloneGuilds(s.Doc().Guilds)
	doc.Personas = map[string]config.Persona{}
	for k, v := range s.Doc().Personas {
		doc.Personas[k] = v
	}
	if v := strVal(in, "monthly_budget_usd", ""); v != "" {
		doc.Limits.MonthlyBudgetUSD = v
	}
	if v := strVal(in, "per_request_budget_usd", ""); v != "" {
		doc.Limits.PerRequestBudgetUSD = v
	}
	if n := intVal(in, "max_external_attempts", 0); n > 0 {
		doc.Limits.MaxExternalAttempts = n
	}
	if v := strVal(in, "default_locale", ""); v != "" {
		doc.I18n.DefaultLocale = v
	}
	if v := strVal(in, "timezone", ""); v != "" {
		doc.I18n.Timezone = v
	}
	provider := strVal(in, "response_provider", doc.Models["response"].Provider)
	model := strVal(in, "response_model", doc.Models["response"].Model)
	if provider != "" || model != "" {
		m := doc.Models["response"]
		if provider != "" {
			m.Provider = provider
		}
		m.Model = model
		doc.Models["response"] = m
	}
	for name := range doc.Features {
		key := "feature_" + name
		if _, ok := in[key]; ok {
			doc.Features[name] = formOn(in, key)
		}
	}
	raw, err := config.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if _, err := config.Parse(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func formOn(in map[string]any, key string) bool {
	v, ok := in[key]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case []any:
		if len(t) == 0 {
			return false
		}
		return formOn(map[string]any{key: t[len(t)-1]}, key)
	case []string:
		if len(t) == 0 {
			return false
		}
		return t[len(t)-1] == "on" || t[len(t)-1] == "true" || t[len(t)-1] == "1"
	case string:
		return t == "on" || t == "true" || t == "1"
	case bool:
		return t
	default:
		return false
	}
}

func (s *Service) configRollback(req action.Request) action.Result {
	in := inputMap(req)
	target := intVal(in, "revision", s.Revision()-1)
	if target < 1 || target >= s.Revision() {
		return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("revision must be between 1 and %d", s.Revision()-1), nil)
	}
	var content string
	if err := s.Store.DB.QueryRow(`SELECT content FROM config_revisions WHERE revision=? AND status='committed'`, target).Scan(&content); err != nil {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "revision not found", nil)
	}
	res := s.applyCandidate(req, []byte(content), s.Revision())
	if res.OK {
		res.Output["rolled_back_to"] = target
	}
	return res
}

// applyCandidate commits candidate if base is still the current revision. Config writers are
// serialized, so the revision check and the commit cannot interleave with another change.
func (s *Service) applyCandidate(req action.Request, candidate []byte, base int) action.Result {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.applyCandidateLocked(req, candidate, base)
}

// applyCandidateLocked is applyCandidate for a caller that already holds cfgMu.
func (s *Service) applyCandidateLocked(req action.Request, candidate []byte, base int) action.Result {
	st, err := loadCASState(s.Store, s.CfgPath)
	if err != nil {
		return action.Fail(req, "IO", "generic.error", err.Error(), nil)
	}
	prep, err := config.BeginApply(st, base, candidate)
	if err != nil {
		code := errcode.ConfigConflict
		if containsFold(err.Error(), errcode.ConfigInvalid) || containsFold(err.Error(), "parse") || containsFold(err.Error(), "unsupported") {
			code = errcode.ConfigInvalid
		}
		details := map[string]any{"expected": s.Revision(), "actual": base, "detail": err.Error()}
		if code == errcode.ConfigInvalid {
			// not a conflict with another change: the operator needs the rule that failed (BeginApply keeps
			// only the text of its cause, so a key written into a tool is looked for in the candidate itself)
			if _, perr := config.ValidateCandidate(candidate); directKeyOf(perr) != nil {
				return s.configFail(req, code, perr, details)
			}
			return action.Fail(req, code, "generic.error", err.Error(), details)
		}
		return action.Fail(req, code, "config.conflict", s.msg(req, "config.conflict"), details)
	}
	if st.Current != nil && config.Hash(st.Current.Content) == config.Hash(candidate) {
		res := action.OK(req, map[string]any{
			"revision": st.Current.Revision, "message": s.msg(req, "config.applied"), "hash": prep.ContentHash,
			"applied": "unchanged", "changed": false,
		})
		res.Revision = st.Current.Revision
		return res
	}
	if err := persistRevisionAt(s.Store, prep, s.Clock.Now()); err != nil {
		return action.Fail(req, "IO", "generic.error", err.Error(), nil)
	}
	if err := atomicWriteFile(s.CfgPath, candidate); err != nil {
		return action.Fail(req, "IO", "generic.error", err.Error(), nil)
	}
	prep.Status = config.StatusCommitted
	if err := persistRevisionAt(s.Store, prep, s.Clock.Now()); err != nil {
		return action.Fail(req, "IO", "generic.error", err.Error(), nil)
	}
	doc, err := config.Parse(candidate)
	if err != nil {
		return action.Fail(req, errcode.ConfigInvalid, "generic.error", err.Error(), nil)
	}
	s.setDoc(doc, prep.Revision)
	s.emit("config", "", map[string]any{"revision": prep.Revision})
	res := action.OK(req, map[string]any{
		"revision": prep.Revision,
		"message":  s.msg(req, "config.applied"),
		"hash":     prep.ContentHash,
		"applied":  "applied", // hot reload; restart/reindex-gated fields are not in this schema yet
	})
	res.Revision = prep.Revision
	return res
}

// configChange is one setting that differs between two config documents, by path.
type configChange struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// configChanges compares two YAML documents setting by setting (a.b.c paths; lists compare whole),
// which reads far better than a line diff of nested YAML.
func configChanges(before, after []byte) ([]configChange, error) {
	flat := func(raw []byte) (map[string]any, error) {
		var doc any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		out := map[string]any{}
		var walk func(prefix string, v any)
		walk = func(prefix string, v any) {
			if m, ok := v.(map[string]any); ok && len(m) > 0 {
				for k, x := range m {
					p := k
					if prefix != "" {
						p = prefix + "." + k
					}
					walk(p, x)
				}
				return
			}
			if prefix != "" {
				out[prefix] = v
			}
		}
		walk("", doc)
		return out, nil
	}
	a, err := flat(before)
	if err != nil {
		return nil, err
	}
	b, err := flat(after)
	if err != nil {
		return nil, err
	}
	var out []configChange
	for p, av := range a {
		bv, ok := b[p]
		if !ok {
			out = append(out, configChange{Path: p, Before: av})
			continue
		}
		if fmt.Sprint(av) != fmt.Sprint(bv) {
			out = append(out, configChange{Path: p, Before: av, After: bv})
		}
	}
	for p, bv := range b {
		if _, ok := a[p]; !ok {
			out = append(out, configChange{Path: p, After: bv})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *Service) revisionContent(rev int) ([]byte, bool) {
	var content string
	if err := s.Store.DB.QueryRow(`SELECT content FROM config_revisions WHERE revision=? AND status='committed'`, rev).Scan(&content); err != nil {
		return nil, false
	}
	return []byte(content), true
}

// configChangesAction lists what changed between two committed revisions (default: to = from+1).
func (s *Service) configChangesAction(req action.Request) action.Result {
	in := inputMap(req)
	from := intVal(in, "from", 0)
	to := intVal(in, "to", from+1)
	if from < 1 || to < 1 || from == to {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "from and to must be two different revisions (1 or more)", nil)
	}
	a, ok := s.revisionContent(from)
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("revision %d not found", from), nil)
	}
	b, ok := s.revisionContent(to)
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("revision %d not found", to), nil)
	}
	changes, err := configChanges(a, b)
	if err != nil {
		return action.Fail(req, errcode.ConfigInvalid, "generic.error", err.Error(), nil)
	}
	return action.OK(req, map[string]any{"from": from, "to": to, "changes": changes})
}
