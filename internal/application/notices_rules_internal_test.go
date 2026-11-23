package application

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"masamong/internal/features/notices"
)

func TestNoticeRules(t *testing.T) {
	c := &notices.Catalog{Sources: []notices.Source{
		{ID: "a1", Kind: notices.KindSchool, School: "a"},
		{ID: "a2", Kind: notices.KindSchool, School: "a", Manual: true},
		{ID: "b1", Kind: notices.KindSchool, School: "b"},
		{ID: "t1", Kind: notices.KindTransfer, University: "가대학교"},
	}}
	school := noticeSub{Kind: notices.KindSchool, Profile: notices.Profile{School: "a"}}
	transfer := noticeSub{Kind: notices.KindTransfer, Sources: []string{"t1"}}
	if got := noticeSourceIDs(c, school); !reflect.DeepEqual(got, []string{"a1", "a2"}) {
		t.Fatalf("school boards: %v", got)
	}
	if got := noticeSourceIDs(c, transfer); !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("transfer boards: %v", got)
	}
	if !boardIsMine(school, c.Sources[1]) || boardIsMine(school, c.Sources[2]) || !boardIsMine(transfer, c.Sources[3]) || boardIsMine(transfer, c.Sources[0]) {
		t.Fatal("boardIsMine")
	}

	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	week := "2026-11-03T09:00:00Z"
	if got := noticesSince(now, 7, noticeSub{CreatedAt: "2026-11-05T00:00:00Z"}, false); got != week {
		t.Fatalf("before the first delivery: %s", got)
	}
	delivered := noticeSub{LastDelivery: "2026-11-09", CreatedAt: "2026-11-05T00:00:00Z"}
	if got := noticesSince(now, 7, delivered, false); got != delivered.CreatedAt {
		t.Fatalf("after the first delivery: %s", got)
	}
	if got := noticesSince(now, 7, delivered, true); got != week {
		t.Fatalf("with the sent ones: %s", got)
	}

	today := time.Date(2026, 11, 10, 23, 0, 0, 0, time.FixedZone("x", 9*3600))
	for date, want := range map[string]int{"2026-11-10": 0, "2026-11-13": 3, "2026-11-09": -1} {
		d, _ := time.Parse("2006-01-02", date)
		if got := daysUntil(d, today); got != want {
			t.Errorf("days until %s: %d, want %d", date, got, want)
		}
	}

	n := func(univ, band string) rankedNotice {
		return rankedNotice{University: univ, Title: univ + band, Score: notices.Scored{Band: band}}
	}
	groups := groupNotices(notices.KindTransfer, []rankedNotice{n("나", ""), n("가", ""), n("나", "")})
	if len(groups) != 2 || len(groups[0].items) != 2 || groups[0].items[0].University != "나" || groups[1].items[0].University != "가" {
		t.Fatalf("transfer groups: %+v", groups)
	}
	groups = groupNotices(notices.KindSchool, []rankedNotice{n("", "reference"), n("", "odd"), n("", "action"), n("", "reference")})
	var bands []string
	for _, g := range groups {
		bands = append(bands, fmt.Sprintf("%s:%d", g.band, len(g.items)))
	}
	if !reflect.DeepEqual(bands, []string{"action:1", "reference:2", ":1"}) {
		t.Fatalf("school groups: %v", bands)
	}

	for _, tc := range []struct {
		err             error
		state           string
		settle, disable bool
	}{
		{fmt.Errorf("x: %w", ErrNotDelivered), "failed", false, false},
		{fmt.Errorf("x: %w", ErrDMClosed), "closed", true, true},
		{errors.New("timeout"), "unknown", true, false},
	} {
		state, settle, disable := noticeSendFailure(tc.err)
		if state != tc.state || settle != tc.settle || disable != tc.disable {
			t.Errorf("%v: %s %v %v", tc.err, state, settle, disable)
		}
	}
}
