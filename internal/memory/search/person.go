package search

import (
	"sort"
	"strings"
	"unicode"

	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/lexical"
)

// Intent is the planner-derived retrieval intent (bot_plan.intent_from_plan). The zero value is the
// neutral intent the sandbox uses when no planner output exists (intent_fallback: none).
type Intent struct {
	Recent            bool      `json:"recent"`
	Intro             bool      `json:"intro"`
	Sensitive         bool      `json:"politics"`
	Kind              string    `json:"kind"`
	Targets           []string  `json:"targets"`
	Time              *TimeSpec `json:"time,omitempty"`
	SemanticQuery     string    `json:"semantic_query,omitempty"`
	KeyTerms          []string  `json:"key_terms,omitempty"`
	MemorySearchQuery string    `json:"memory_search_query,omitempty"`
	Unresolved        []string  `json:"unresolved,omitempty"`
	Source            string    `json:"source"` // planner | none
}

// textRules is the language-dependent part of person matching and lexical search.
type textRules struct {
	search    lang.Search
	particles map[rune]bool
	pronouns  map[string]bool
	tok       lexical.Tokenizer
}

func newTextRules(s *lang.Search) *textRules {
	if s == nil {
		d := lang.MergeSearch([]string{lang.Default}, nil)
		s = &d
	}
	r := &textRules{search: *s, particles: map[rune]bool{}, pronouns: map[string]bool{}, tok: lexical.FromLanguage(*s)}
	for _, c := range s.Particles {
		r.particles[c] = true
	}
	for _, w := range s.Pronouns {
		r.pronouns[strings.ToLower(w)] = true
	}
	return r
}

func (r *textRules) pronoun(name string) bool { return r.pronouns[strings.ToLower(name)] }

func isHangul(r rune) bool { return r >= '가' && r <= '힣' }

func allHangul(rs []rune) bool {
	for _, r := range rs {
		if !isHangul(r) {
			return false
		}
	}
	return true
}

func (r *textRules) stripParticles(word []rune) []rune {
	out := word
	for i := 0; i < 3; i++ {
		if len(out) > 0 && r.particles[out[len(out)-1]] {
			out = out[:len(out)-1]
		} else {
			break
		}
	}
	return out
}

func runeIndex(text, tok []rune, from int) int {
	for i := from; i+len(tok) <= len(text); i++ {
		match := true
		for k := range tok {
			if text[i+k] != tok[k] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// tokenInText is engine._token_in_text: a name matches with particles attached (철수가), inside
// surname+given name (김철수 ⊃ 철수), or as part of a longer alias label, but not inside another word
// (철수영 ⊅ 철수).
func (r *textRules) tokenInText(text, tok string, labels map[string]bool) bool {
	if tok == "" || text == "" || !strings.Contains(text, tok) {
		return false
	}
	t, k := []rune(text), []rune(tok)
	if r.search.LatinWords && allLatin(k) {
		return latinWordIn(t, k)
	}
	n := len(k)
	for start := 0; ; {
		i := runeIndex(t, k, start)
		if i < 0 {
			return false
		}
		hb := i > 0 && isHangul(t[i-1])
		ha := i+n < len(t) && isHangul(t[i+n])
		if !hb && !ha {
			return true
		}
		left, right := i, i+n
		for left > 0 && isHangul(t[left-1]) {
			left--
		}
		for right < len(t) && isHangul(t[right]) {
			right++
		}
		word := t[left:right]
		core := string(r.stripParticles(word))
		if core == tok || labels[core] {
			return true
		}
		for lab := range labels {
			if lab != "" && lab != tok && len([]rune(lab)) > n && strings.Contains(core, lab) {
				return true
			}
		}
		if strings.HasSuffix(core, tok) {
			prefix := []rune(strings.TrimSuffix(core, tok))
			if len(prefix) >= 1 && len(prefix) <= 2 && allHangul(prefix) {
				return true
			}
		}
		if hb {
			start = i + 1
			continue
		}
		if ha {
			rest := word[n:]
			ok := true
			for _, c := range rest {
				if !r.particles[c] {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		start = i + 1
	}
}

func allLatin(rs []rune) bool {
	for _, c := range rs {
		if !unicode.Is(unicode.Latin, c) {
			return false
		}
	}
	return len(rs) > 0
}

// latinWordIn reports whether tok appears in text as a whole word (case-insensitive): "Kim" matches
// "Kim said" but not "Kimberly".
func latinWordIn(text, tok []rune) bool {
	lt, lk := []rune(strings.ToLower(string(text))), []rune(strings.ToLower(string(tok)))
	for start := 0; ; {
		i := runeIndex(lt, lk, start)
		if i < 0 {
			return false
		}
		before := i > 0 && unicode.IsLetter(lt[i-1])
		after := i+len(lk) < len(lt) && unicode.IsLetter(lt[i+len(lk)])
		if !before && !after {
			return true
		}
		start = i + 1
	}
}

// cleanTargets is engine._clean_planner_targets. Pronouns (and the bot's own names) never become
// person targets (planner noise); trailing particles are stripped from names in scripts that use them.
func (r *textRules) cleanTargets(raw []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range raw {
		name := strings.TrimSpace(item)
		if name == "" || r.pronoun(name) {
			continue
		}
		rs := []rune(name)
		if allHangul(rs) {
			if st := r.stripParticles(rs); len(st) >= 2 {
				name = string(st)
			}
		}
		if l := len([]rune(name)); l >= 2 && l <= 40 && !r.pronoun(name) && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// speakerKey is who spoke at the granularity the platform confirms: a native account (Discord) or,
// without one (Kakao), the per-platform label. Never a person ID; never merged across platforms.
type speakerKey struct {
	Kind, Platform, Value string
}

func (k speakerKey) linked() bool { return k.Kind == "account" }

func keyForMessage(m corpus.Message) speakerKey {
	room := m.RoomOriginal
	if room == "" {
		room = m.Room
	}
	platform := corpus.PlatformOfRoom(room)
	if strings.TrimSpace(m.ActorAccount) != "" {
		return speakerKey{"account", platform, strings.TrimSpace(m.ActorAccount)}
	}
	label := m.ActorLabel
	if label == "" {
		label = "?"
	}
	return speakerKey{"label", platform, label}
}

type identityTable struct {
	labelsByKey map[speakerKey]map[string]bool
	keysByLabel map[string]map[speakerKey]bool
	leavesByKey map[speakerKey]Set
}

func buildIdentityTable(ix *Index) *identityTable {
	return collectIdentityTable(ix, true)
}

func collectIdentityTable(ix *Index, keepLeaves bool) *identityTable {
	t := &identityTable{labelsByKey: map[speakerKey]map[string]bool{}, keysByLabel: map[string]map[speakerKey]bool{}, leavesByKey: map[speakerKey]Set{}}
	for _, l := range ix.Leaves {
		if !(l.Start >= 0 && l.Start < l.End && l.End <= len(ix.Messages)) {
			continue
		}
		for _, m := range ix.Messages[l.Start:l.End] {
			k := keyForMessage(m)
			label := m.ActorLabel
			if label == "" {
				label = "?"
			}
			if t.labelsByKey[k] == nil {
				t.labelsByKey[k] = map[string]bool{}
				if keepLeaves {
					t.leavesByKey[k] = Set{}
				}
			}
			t.labelsByKey[k][label] = true
			if t.keysByLabel[label] == nil {
				t.keysByLabel[label] = map[speakerKey]bool{}
			}
			t.keysByLabel[label][k] = true
			if keepLeaves {
				t.leavesByKey[k][l.DocID] = struct{}{}
			}
		}
	}
	return t
}

func collectIdentityLeaves(ix *Index) map[speakerKey]Set {
	leavesByKey := map[speakerKey]Set{}
	for _, l := range ix.Leaves {
		if !(l.Start >= 0 && l.Start < l.End && l.End <= len(ix.Messages)) {
			continue
		}
		for _, m := range ix.Messages[l.Start:l.End] {
			k := keyForMessage(m)
			if leavesByKey[k] == nil {
				leavesByKey[k] = Set{}
			}
			leavesByKey[k][l.DocID] = struct{}{}
		}
	}
	return leavesByKey
}

// aliasTable is engine._alias_table_for: label → labels of the same person. `observed` derives it
// from native accounts only (one Discord account under several display names); Kakao labels stay
// unlinked. `config` uses person_aliases; `both` merges.
func (ix *Index) aliasTable() map[string]map[string]bool {
	ix.lazy()
	mode := ix.Cfg.personIdentity()
	out := map[string]map[string]bool{}
	add := func(k string, labels map[string]bool) {
		if out[k] == nil {
			out[k] = map[string]bool{}
		}
		for l := range labels {
			out[k][l] = true
		}
	}
	if mode == "config" || mode == "both" {
		for k, vals := range ix.Cfg.PersonAliases {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			labels := map[string]bool{k: true}
			for _, v := range vals {
				if v = strings.TrimSpace(v); v != "" {
					labels[v] = true
				}
			}
			add(k, labels)
		}
	}
	if mode == "observed" || mode == "both" {
		for key, labels := range ix.identity.labelsByKey {
			if !key.linked() || len(labels) < 2 {
				continue
			}
			for l := range labels {
				add(l, labels)
			}
		}
	}
	return out
}

// sortedByLenDesc orders labels longest first (ties alphabetical for determinism).
func sortedByLenDesc(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool {
		la, lb := len([]rune(out[a])), len([]rune(out[b]))
		if la != lb {
			return la > lb
		}
		return out[a] < out[b]
	})
	return out
}

// lexicalQueryExpanded appends the target's other labels to the lexical query (not to dense).
func (ix *Index) lexicalQueryExpanded(query string, it Intent) (string, []string) {
	if !boolOr(ix.Cfg.AliasExpandLexical, true) {
		return query, nil
	}
	aliases := ix.aliasTable()
	labels := map[string]bool{}
	for _, key := range ix.rules.cleanTargets(it.Targets) {
		if a, ok := aliases[key]; ok && len(a) > 0 {
			for l := range a {
				labels[l] = true
			}
		} else {
			labels[key] = true
		}
		labels[key] = true
	}
	var extra []string
	for _, l := range sortedByLenDesc(labels) {
		if l != "" && !strings.Contains(query, l) {
			extra = append(extra, l)
		}
	}
	if len(extra) == 0 {
		return query, nil
	}
	return query + " " + strings.Join(extra, " "), extra
}

// PersonResolution is who the planner's person targets resolved to inside the allowed scope.
type PersonResolution struct {
	Targets        []string            `json:"targets"`
	Labels         map[string]bool     `json:"-"`
	Speakers       map[string]bool     `json:"-"`
	PerTarget      map[string][]string `json:"-"`
	Ambiguous      []string            `json:"ambiguous"`
	Unresolved     []string            `json:"unresolved"`
	ScopeMode      string              `json:"-"`
	ScopeSpeakersN int                 `json:"-"`
	keys           map[speakerKey]bool
	identityUsed   bool
}

// Resolved reports whether any unambiguous speaker matched.
func (r PersonResolution) Resolved() bool { return len(r.Speakers) > 0 }

// Trace mirrors PersonResolution.to_trace (labels only; native account IDs never leave).
func (r PersonResolution) Trace() map[string]any {
	speakers := make([]string, 0, len(r.Speakers))
	for s := range r.Speakers {
		speakers = append(speakers, s)
	}
	sort.Strings(speakers)
	labels := make([]string, 0, len(r.Labels))
	for s := range r.Labels {
		labels = append(labels, s)
	}
	sort.Strings(labels)
	out := map[string]any{
		"targets": r.Targets, "labels": labels[:min(32, len(labels))], "speakers": speakers[:min(16, len(speakers))],
		"ambiguous": nonNil(r.Ambiguous), "unresolved": nonNil(r.Unresolved),
		"scope": map[string]any{"mode": r.ScopeMode, "speakers_n": r.ScopeSpeakersN},
	}
	if r.identityUsed {
		linked, labelOnly := 0, 0
		for k := range r.keys {
			if k.linked() {
				linked++
			} else {
				labelOnly++
			}
		}
		out["speaker_keys"] = map[string]any{"account_linked_n": linked, "label_only_n": labelOnly}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func labelsFor(targets []string, aliases map[string]map[string]bool) map[string]bool {
	labels := map[string]bool{}
	for _, key := range targets {
		labels[key] = true
		for l := range aliases[key] {
			labels[l] = true
		}
		for canon, vals := range aliases {
			if vals[key] {
				labels[canon] = true
				for l := range vals {
					labels[l] = true
				}
			}
		}
	}
	delete(labels, "")
	return labels
}

// speakersInScope is label → leaf ids inside allowed, in first-appearance order.
func (ix *Index) speakersInScope(allowed Set) ([]string, map[string][]string) {
	ix.lazy()
	if allowed != nil && len(allowed) == 0 {
		return nil, nil
	}
	var order []string
	out := map[string][]string{}
	for _, sp := range ix.spkOrder {
		ids := ix.spkLeaves[sp]
		if allowed != nil {
			var kept []string
			for _, id := range ids {
				if allowed.Has(id) {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		if len(ids) > 0 {
			order = append(order, sp)
			out[sp] = ids
		}
	}
	return order, out
}

func (t *identityTable) keysInScope(label string, allowed Set) map[speakerKey]bool {
	out := map[speakerKey]bool{}
	for k := range t.keysByLabel[label] {
		leaves := t.leavesByKey[k]
		if allowed == nil {
			if len(leaves) > 0 {
				out[k] = true
			}
			continue
		}
		for id := range leaves {
			if allowed.Has(id) {
				out[k] = true
				break
			}
		}
	}
	return out
}

// resolvePersons is person_retrieval.resolve_persons with an account-aware identity table. Only
// speakers inside allowed count; a target that matches too many distinct speakers is ambiguous.
func (ix *Index) resolvePersons(targets []string, allowed Set, useIdentity bool) PersonResolution {
	ix.lazy()
	res := PersonResolution{Targets: targets, PerTarget: map[string][]string{}, Speakers: map[string]bool{}, keys: map[speakerKey]bool{}, identityUsed: useIdentity}
	switch {
	case allowed == nil:
		res.ScopeMode = "none"
	case len(allowed) == 0:
		res.ScopeMode = "empty"
	default:
		res.ScopeMode = "set"
	}
	if len(targets) == 0 {
		return res
	}
	aliases := ix.aliasTable()
	order, inScope := ix.speakersInScope(allowed)
	res.ScopeSpeakersN = len(order)
	res.Labels = labelsFor(targets, aliases)
	if len(order) == 0 {
		res.Unresolved = append([]string(nil), targets...)
		return res
	}
	pos := map[string]int{}
	for i, s := range order {
		pos[s] = i
	}
	maxSp := intOr(ix.Cfg.Retrieval.Person.MaxSpeakersPerTarget, 3)
	for _, target := range targets {
		lt := labelsFor([]string{target}, aliases)
		toks := map[string]bool{}
		for l := range lt {
			if n := len([]rune(l)); n >= 2 && n <= 12 {
				toks[l] = true
			}
		}
		if n := len([]rune(target)); n >= 2 && n <= 12 {
			toks[target] = true
		}
		var matched []string
		for _, sp := range order {
			hit := lt[sp]
			if !hit {
				for tok := range toks {
					if ix.rules.tokenInText(sp, tok, lt) {
						hit = true
						break
					}
				}
			}
			if hit {
				matched = append(matched, sp)
			}
		}
		distinct := len(matched)
		var tkeys map[speakerKey]bool
		if useIdentity {
			tkeys = map[speakerKey]bool{}
			for _, sp := range matched {
				for k := range ix.identity.keysInScope(sp, allowed) {
					tkeys[k] = true
				}
			}
			all := map[string]bool{}
			for _, sp := range matched {
				all[sp] = true
			}
			for k := range tkeys {
				for lab := range ix.identity.labelsByKey[k] {
					if _, ok := inScope[lab]; ok {
						all[lab] = true
					}
				}
			}
			matched = matched[:0:0]
			for lab := range all {
				matched = append(matched, lab)
			}
			sort.Slice(matched, func(a, b int) bool { return pos[matched[a]] < pos[matched[b]] })
			distinct = len(tkeys)
		}
		res.PerTarget[target] = matched
		switch {
		case len(matched) == 0:
			res.Unresolved = append(res.Unresolved, target)
		case distinct > maxSp:
			res.Ambiguous = append(res.Ambiguous, target)
		default:
			for _, sp := range matched {
				res.Speakers[sp] = true
			}
			for k := range tkeys {
				res.keys[k] = true
			}
		}
	}
	return res
}

func (ix *Index) speakerUniverse(res PersonResolution, allowed Set) Set {
	out := Set{}
	if res.identityUsed && len(res.keys) > 0 {
		for k := range res.keys {
			for id := range ix.identity.leavesByKey[k] {
				out[id] = struct{}{}
			}
		}
	} else {
		for sp := range res.Speakers {
			for _, id := range ix.spkLeaves[sp] {
				out[id] = struct{}{}
			}
		}
	}
	if allowed != nil {
		out = intersect(out, allowed)
	}
	return out
}

// speakerFusion runs dense + lexical over the resolved speakers' own leaves (scope, not a bonus).
func (ix *Index) speakerFusion(res PersonResolution, query string, q []float32, allowed Set, limit int, w [2]float64) (*Fusion, Set) {
	universe := ix.speakerUniverse(res, allowed)
	if len(universe) == 0 {
		return fuse([]Run{{"speaker_lexical", nil, w[0]}, {"speaker_dense", nil, w[1]}}, nil, nil), universe
	}
	dense := denseTop(ix.LeafVecs, ix.leafIDs, q, universe, limit)
	var lexical []Scored
	for _, h := range ix.leafLexicalSearch(query, max(limit, len(ix.leafIDs))) {
		if universe.Has(h.DocID) {
			lexical = append(lexical, Scored{h.DocID, h.Score})
			if len(lexical) >= limit {
				break
			}
		}
	}
	return fuse([]Run{{"speaker_lexical", ids(lexical), w[0]}, {"speaker_dense", ids(dense), w[1]}}, nil,
		map[string]map[string]float64{"speaker_lexical": scoreMap(lexical), "speaker_dense": scoreMap(dense)}), universe
}

// aliasRun finds leaves that mention the person (any label) — lexical only.
func (ix *Index) aliasRun(res PersonResolution, allowed Set, limit int) []Scored {
	var labels []string
	for _, l := range sortedByLenDesc(res.Labels) {
		if len([]rune(l)) >= 2 {
			labels = append(labels, l)
		}
	}
	if len(labels) == 0 {
		return nil
	}
	lim := limit
	if allowed != nil {
		lim = max(limit, len(ix.leafIDs))
	}
	var out []Scored
	for _, h := range ix.leafLexicalSearch(strings.Join(labels[:min(8, len(labels))], " "), lim) {
		if allowed == nil || allowed.Has(h.DocID) {
			out = append(out, Scored{h.DocID, h.Score})
		}
	}
	return out[:min(limit, len(out))]
}

func ids(s []Scored) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.ID
	}
	return out
}

func scoreMap(s []Scored) map[string]float64 {
	out := make(map[string]float64, len(s))
	for _, x := range s {
		out[x.ID] = x.Score
	}
	return out
}

// IdentityInputs is a compact global label/account table, built only from eligible
// leaf intervals. It owns no message bodies, positions, vectors or global leaf IDs.
// Construct it before publishing indexes; it is immutable and may be shared.
type IdentityInputs struct{ table *identityTable }

func NewIdentityInputs(msgs []corpus.Message, lv []leaves.Leaf) *IdentityInputs {
	t := collectIdentityTable(&Index{Messages: msgs, Leaves: lv}, false)
	t.leavesByKey = nil
	return &IdentityInputs{table: t}
}

// BuildWithIdentity preserves Build's local leaves while injecting global labels.
// With nil inputs it has precisely Build's behavior.
func BuildWithIdentity(cfg Config, msgs []corpus.Message, lv []leaves.Leaf, leafVecs Matrix, summaries []Summary, summaryVecs Matrix, inputs *IdentityInputs) (*Index, error) {
	ix, err := Build(cfg, msgs, lv, leafVecs, summaries, summaryVecs)
	if err != nil || inputs == nil {
		return ix, err
	}
	ix.identity = &identityTable{labelsByKey: inputs.table.labelsByKey, keysByLabel: inputs.table.keysByLabel, leavesByKey: collectIdentityLeaves(ix)}
	return ix, nil
}
