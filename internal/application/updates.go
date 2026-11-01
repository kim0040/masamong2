package application

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/providers"
)

// Update check: once a day the daemon asks GitHub for the newest release and the console says when a
// newer version is out (Maintenance, and an info item in the notifications). The check downloads and
// installs nothing; installing is a separate step the operator starts (updates.apply, selfupdate.go).
// The request carries only the program's name and version; updates.check: false turns it off.
// Local builds (a version ending in -dev) never check. While the repository's releases are not public,
// GitHub answers 404 and the console shows that the check is not available.

// UpdateRepo is the GitHub repository whose releases are checked (set at build time with -X).
var UpdateRepo = "kim0040/masamong2"

// updateAPI is the releases endpoint; tests point it at a local server.
var updateAPI = "https://api.github.com/repos/%s/releases/latest"

const updateEvery = 24 * time.Hour

var updateChecking, updateApplying atomic.Bool

type updateState struct {
	CheckedAt string `json:"checked_at,omitempty"`
	Status    string `json:"status"` // ok | unavailable (no public release) | error
	Latest    string `json:"latest,omitempty"`
	URL       string `json:"url,omitempty"`
	Published string `json:"published_at,omitempty"`
	Error     string `json:"error,omitempty"`
	// this computer's archive and the release's checksum list, for updates.apply
	Asset   *updateAsset `json:"asset,omitempty"`
	SumsURL string       `json:"sums_url,omitempty"`
}

func devBuild(v string) bool { return v == "" || strings.Contains(v, "-dev") }

// updateDue reports whether the daily check should run now.
func (s *Service) updateDue() bool {
	if s.Store == nil || s.practice || !s.Doc().Updates.CheckOn() || devBuild(Version) {
		return false
	}
	var st updateState
	_, _ = s.kvGet("updates:state", &st)
	last, err := time.Parse(time.RFC3339, st.CheckedAt)
	return err != nil || s.Clock.Now().Sub(last) >= updateEvery
}

// checkUpdate asks GitHub for the newest release and records the answer.
func (s *Service) checkUpdate(ctx context.Context) updateState {
	st := updateState{CheckedAt: s.Clock.Now().UTC().Format(time.RFC3339)}
	client := providers.NewClient(15*time.Second, s.Log)
	client.MaxBody = 1 << 20
	client.UserAgent = "masamong/" + Version
	resp, err := client.Get(ctx, strings.Replace(updateAPI, "%s", UpdateRepo, 1), map[string]string{"Accept": "application/vnd.github+json"})
	var pe *providers.Error
	switch {
	case errors.As(err, &pe) && pe.Status == 404:
		st.Status = "unavailable"
	case err != nil:
		st.Status, st.Error = "error", clip(err.Error(), 200)
	default:
		var rel struct {
			Tag       string `json:"tag_name"`
			URL       string `json:"html_url"`
			Published string `json:"published_at"`
			Draft     bool   `json:"draft"`
			Pre       bool   `json:"prerelease"`
			Assets    []struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
				Size int64  `json:"size"`
			} `json:"assets"`
		}
		if json.Unmarshal(resp.Body, &rel) != nil || rel.Tag == "" || rel.Draft || rel.Pre {
			st.Status = "unavailable"
		} else {
			st.Status, st.Latest, st.Published = "ok", strings.TrimPrefix(rel.Tag, "v"), rel.Published
			if strings.HasPrefix(rel.URL, "https://github.com/") {
				st.URL = rel.URL
			}
			for _, a := range rel.Assets {
				switch a.Name {
				case updateAssetName():
					st.Asset = &updateAsset{Name: a.Name, URL: a.URL, Size: a.Size}
				case "SHA256SUMS":
					st.SumsURL = a.URL
				}
			}
		}
	}
	_ = s.kvSet("updates:state", st)
	return st
}

// describeSuffix is what `git describe --tags --dirty` adds to a tag for a source build: commits after
// the tag (-3-gabc1234) and uncommitted changes (-dirty).
var describeSuffix = regexp.MustCompile(`(-[0-9]+-g[0-9a-f]{7,40})?(-dirty)?$`)

// sourceBuild reports whether v is a source build stamped by git describe (v1.0.0-3-gabc1234,
// v1.0.0-dirty): it holds that tag and more, so it is never older than the tag and never self-updates.
func sourceBuild(v string) bool { return describeSuffix.FindString(v) != "" }

// newerVersion reports whether latest (1.2.3) is above current (1.2.2, 1.2.2-rc1…). A source build after
// a tag (1.2.2-3-gabc1234) counts as that tag and later, so only a higher release is newer.
func newerVersion(latest, current string) bool {
	after := sourceBuild(current)
	current = strings.TrimSuffix(current, describeSuffix.FindString(current))
	parse := func(v string) ([3]int, bool) {
		var out [3]int
		core, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
		parts := strings.Split(core, ".")
		if len(parts) != 3 {
			return out, false
		}
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	l, ok1 := parse(latest)
	c, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	// same numbers: a release is newer than a pre-release of it, but not than a build made after it
	if after && !strings.Contains(current, "-") {
		return false
	}
	return strings.Contains(current, "-") && !strings.Contains(latest, "-")
}

// updateAttention is the notification item for a newer release.
func (s *Service) updateAttention() []attention {
	if s.Store == nil || s.practice || !s.Doc().Updates.CheckOn() {
		return nil
	}
	var st updateState
	if ok, _ := s.kvGet("updates:state", &st); !ok || st.Status != "ok" || !newerVersion(st.Latest, Version) {
		return nil
	}
	return []attention{{"info", "update_available", map[string]any{"version": st.Latest, "current": Version}, "/maintenance"}}
}

// updatesAction: updates.status (the last answer), updates.check (ask now) and updates.apply (install).
func (s *Service) updatesAction(ctx context.Context, req action.Request) action.Result {
	if req.Action == "updates.apply" {
		return s.updateApply(ctx, req)
	}
	out := map[string]any{"current": Version, "check": s.Doc().Updates.CheckOn(), "dev_build": devBuild(Version), "repo": UpdateRepo}
	var st updateState
	if req.Action == "updates.check" {
		if s.practice {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "not available in practice mode", nil)
		}
		st = s.checkUpdate(ctx)
	} else {
		_, _ = s.kvGet("updates:state", &st)
	}
	out["state"] = st
	out["newer"] = st.Status == "ok" && newerVersion(st.Latest, Version)
	// whether the console can install it, or why the installer is needed instead
	if why := s.updateBlocked(st); why == "" {
		out["can_apply"] = true
	} else if out["newer"] == true {
		out["apply_blocked"] = why
	}
	var applied map[string]any
	if ok, _ := s.kvGet("updates:applied", &applied); ok {
		out["last_applied"] = applied
	}
	return action.OK(req, out)
}
