package application

import (
	"errors"
	"slices"

	"masamong/internal/action"
	"masamong/internal/tools"
)

// toolsWithDirectKeys are the ids of the operator tools whose headers or web address hold a key written
// into the configuration (tools.DirectSecret), sorted. Never the value.
func (s *Service) toolsWithDirectKeys() []string {
	var ids []string
	for id, t := range s.Doc().Tools {
		if t.HTTP != nil && tools.DirectSecret(id, t.HTTP) != nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// directKeyOf is the "key written into a tool" refusal inside err, if that is what err is.
func directKeyOf(err error) *tools.DirectSecretError {
	var direct *tools.DirectSecretError
	if errors.As(err, &direct) {
		return direct
	}
	return nil
}

// configFail is the failure of a change the configuration refused. A key written into an operator tool is
// told in the person's language (the tool and the field, never the value); any other reason keeps the
// parser's own text, kept in details["detail"] for the operator and out of the sentence. details may be nil.
func (s *Service) configFail(req action.Request, code string, err error, details map[string]any) action.Result {
	if direct := directKeyOf(err); direct != nil {
		if details == nil {
			details = map[string]any{}
		}
		details["tool"], details["field"] = direct.Tool, direct.Where()
		return action.Fail(req, code, "tool.direct_key", s.msg(req, "tool.direct_key", "tool", direct.Tool, "field", direct.Where()), details)
	}
	if details == nil {
		details = map[string]any{}
	}
	if _, kept := details["detail"]; !kept {
		details["detail"] = err.Error()
	}
	return s.failErr(req, code, "config.invalid", err, details)
}
