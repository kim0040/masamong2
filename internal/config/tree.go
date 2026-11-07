package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The console edits the document by path. Tree/TreeSet/FromTree work on the YAML shape (field names
// as the file spells them), and FromTree re-parses the result strictly, so an edit can never produce
// a document the CLI would reject. Schema describes that shape for generated forms.

// Tree decodes a config file into nested maps.
func Tree(raw []byte) (map[string]any, error) {
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// TreeSet sets value at path, creating intermediate maps. A segment indexes into a list only when
// the list already exists (guild and channel IDs are numeric map keys); whole lists are set as values.
func TreeSet(tree map[string]any, path []string, value any) error {
	if len(path) == 0 {
		return fmt.Errorf("empty path")
	}
	_, err := setIn(tree, path, value, "")
	return err
}

func setIn(node any, path []string, value any, at string) (any, error) {
	if len(path) == 0 {
		return value, nil
	}
	seg, rest := path[0], path[1:]
	here := seg
	if at != "" {
		here = at + "." + seg
	}
	switch n := node.(type) {
	case nil:
		child, err := setIn(nil, rest, value, here)
		if err != nil {
			return nil, err
		}
		return map[string]any{seg: child}, nil
	case map[string]any:
		child, err := setIn(n[seg], rest, value, here)
		if err != nil {
			return nil, err
		}
		n[seg] = child
		return n, nil
	case []any:
		idx, err := strconv.Atoi(seg)
		if err != nil || idx < 0 || idx > len(n) {
			return nil, fmt.Errorf("%s: list index out of range", here)
		}
		if idx == len(n) {
			n = append(n, nil)
		}
		child, err := setIn(n[idx], rest, value, here)
		if err != nil {
			return nil, err
		}
		n[idx] = child
		return n, nil
	}
	return nil, fmt.Errorf("%s: not an object", at)
}

// TreeUnset removes the value at path (a missing path is not an error).
func TreeUnset(tree map[string]any, path []string) {
	var cur any = tree
	for i, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return
		}
		if i == len(path)-1 {
			delete(m, seg)
			return
		}
		cur = m[seg]
	}
}

// FromTree validates a tree as a config document and returns its canonical file bytes.
func FromTree(tree map[string]any) ([]byte, error) {
	raw, err := yaml.Marshal(tree)
	if err != nil {
		return nil, err
	}
	doc, err := ValidateCandidate(raw)
	if err != nil {
		return nil, err
	}
	return Marshal(doc)
}

// SchemaNode describes one value of the document.
type SchemaNode struct {
	Type     string        `json:"t"` // object | map | array | string | int | float | bool | any
	Fields   []SchemaField `json:"fields,omitempty"`
	Elem     *SchemaNode   `json:"elem,omitempty"`
	Nullable bool          `json:"nullable,omitempty"` // pointer: unset keeps the built-in default
	Enum     []string      `json:"enum,omitempty"`
	// Implied is what an empty (zero or unset) value means, for fields whose default lives in code.
	Implied any `json:"implied,omitempty"`
	// Format asks for a specific control: time (HH:MM), weekday (0 = Sunday), hour (0–23), timezone
	// (IANA name), locale, usd (a dollar amount kept as a string) or multiline (long text).
	Format   string   `json:"format,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
	ReadOnly bool     `json:"readonly,omitempty"` // shown, not edited in the console form
	// Legacy fields are kept for old config files; the console form hides them while they are unset.
	Legacy bool `json:"legacy,omitempty"`
}

// SchemaField is a named field of an object.
type SchemaField struct {
	Key       string      `json:"k"`
	OmitEmpty bool        `json:"omitempty,omitempty"`
	Node      *SchemaNode `json:"s"`
}

// enums are the closed value sets the console offers as choices.
var enums = map[string][]string{
	"memory.retrieve":     RetrieveModes,
	"chat.tool_mode":      ToolModes,
	"chat.tool_model":     ToolModels,
	"chat.loop_final":     LoopFinals,
	"chat.vision":         VisionModes,
	"memory.leaf_split":   {"time_gap", "fixed"},
	"backup.schedule":     BackupSchedules,
	"participation.mode":  ParticipationModes,
	"dm.allow":            DMAllowModes,
	"alerts.min_severity": AlertSeverities,
	"memory.episodes":     OnOff,
	"memory.state":        OnOff,
	"memory.cards":        OnOff,
}

// implied is what an empty value of these fields means; it comes from the same accessors the
// service uses, so the console shows the value that actually applies.
func implied() map[string]any {
	var l Limits
	var m MemoryFlags
	var p Participation
	var a Access
	var al Alerts
	var up Updates
	r := Retention{}.Days()
	return map[string]any{
		"dm.allow": DMSettings{}.AllowMode(), "dm.per_user_per_day": DMSettings{}.UserDaily(), "dm.per_day": DMSettings{}.Daily(),
		"limits.budget_alert_percent": l.AlertPercent(), "limits.max_concurrent_turns": l.ConcurrentTurns(),
		"memory.backfill_days": int(m.Backfill().Hours() / 24), "memory.backfill_max_messages": m.BackfillMax(), "memory.index_workers": m.Workers(),
		"memory.leaf_split": "time_gap", "chat.tool_mode": ChatSettings{}.Mode(), "chat.tool_model": ChatSettings{}.LoopRole(),
		"chat.loop_final":           ChatSettings{}.LoopFinalMode(),
		"chat.vision":               ChatSettings{}.VisionMode(),
		"memory.derived_budget_usd": "3.00", "memory.day_boundary": "05:00",
		"memory.episodes": "on", "memory.state": "on", "memory.cards": "off", "memory.message_vectors": m.MessageVectorsOn(),
		"participation.wait_sec": int(p.Wait().Seconds()), "participation.cooldown_sec": int(p.Cooldown().Seconds()),
		"participation.per_hour": p.Hourly(), "participation.per_day": p.Daily(), "participation.judge_per_hour": p.Judges(),
		"participation.min_score": p.Threshold(), "participation.shadow_drafts": p.Drafts(),
		"access.guild_admins": a.GuildAdminsAllowed(), "access.server_approval": a.ApprovalRequired(),
		"alerts.discord": al.DiscordOn(), "alerts.min_severity": al.Least(), "updates.check": up.CheckOn(),
		"retention.turn_days": r["turns"], "retention.turn_detail_days": r["turn_detail"], "retention.trace_days": r["traces"],
		"retention.audit_days": r["audit"], "retention.usage_days": r["usage"], "retention.query_cache_days": r["query_cache"],
		"retention.image_days": r["images"],
	}
}

func bound(lo, hi float64) [2]*float64 { return [2]*float64{&lo, &hi} }

var (
	formats = map[string]string{
		"participation.quiet_start": "time", "participation.quiet_end": "time",
		"backup.weekday": "weekday", "backup.hour": "hour",
		"i18n.timezone": "timezone", "memory.naive_timezone": "timezone",
		"i18n.default_locale": "locale", "i18n.fallback_locale": "locale",
		"limits.monthly_budget_usd": "usd", "limits.per_request_budget_usd": "usd",
		"memory.index_budget_usd": "usd", "memory.turn_budget_usd": "usd", "memory.derived_budget_usd": "usd",
		"memory.day_boundary": "time",
		"memory.agent_prompt": "multiline",
	}
	// legacy: memory.chat_tools is superseded by tools.<id>.chat; memory.extract is not built.
	legacy = map[string]bool{"memory.chat_tools": true, "memory.extract": true}
	bounds = map[string][2]*float64{
		"backup.hour": bound(0, 23), "backup.weekday": bound(0, 6), "backup.keep": bound(1, 365),
		"limits.budget_alert_percent": bound(1, 100), "limits.max_concurrent_turns": bound(1, 64),
		"limits.max_external_attempts": bound(1, 20),
		"memory.backfill_days":         bound(-1, 30), "memory.index_interval_min": bound(1, 1440), "memory.index_workers": bound(1, MaxIndexWorkers),
		"participation.min_score": bound(0, 1),
		"dm.per_user_per_day":     bound(-1, 10000), "dm.per_day": bound(-1, 100000),
	}
	// readOnly fields describe the installation; changing them from a form would break it.
	readOnly = map[string]bool{"instance.data_dir": true, "instance.profile": true, "limits.profile": true}
)

// Schema describes Document.
func Schema() *SchemaNode {
	n := describe(reflect.TypeOf(Document{}), "")
	annotate(n, "", implied())
	return n
}

func annotate(n *SchemaNode, path string, imp map[string]any) {
	for i := range n.Fields {
		f := &n.Fields[i]
		p := f.Key
		if path != "" {
			p = path + "." + f.Key
		}
		annotate(f.Node, p, imp)
	}
	if path == "" {
		return
	}
	n.Implied, n.Format, n.ReadOnly, n.Legacy = imp[path], formats[path], readOnly[path], legacy[path]
	if b, ok := bounds[path]; ok {
		n.Min, n.Max = b[0], b[1]
	}
}

func describe(t reflect.Type, path string) *SchemaNode {
	switch t.Kind() {
	case reflect.Pointer:
		n := describe(t.Elem(), path)
		n.Nullable = true
		return n
	case reflect.Struct:
		n := &SchemaNode{Type: "object"}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("yaml")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			p := name
			if path != "" {
				p = path + "." + name
			}
			n.Fields = append(n.Fields, SchemaField{Key: name, OmitEmpty: strings.Contains(opts, "omitempty"), Node: describe(f.Type, p)})
		}
		return n
	case reflect.Map:
		return &SchemaNode{Type: "map", Elem: describe(t.Elem(), path+".*")}
	case reflect.Slice, reflect.Array:
		return &SchemaNode{Type: "array", Elem: describe(t.Elem(), path+".[]")}
	case reflect.String:
		return &SchemaNode{Type: "string", Enum: enums[path]}
	case reflect.Bool:
		return &SchemaNode{Type: "bool"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &SchemaNode{Type: "int"}
	case reflect.Float32, reflect.Float64:
		return &SchemaNode{Type: "float"}
	}
	return &SchemaNode{Type: "any"}
}
