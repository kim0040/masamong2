package discord

import (
	"reflect"
	"testing"
)

// The slash command's region option is a place: words that are verbs when typed after !내설정 ("초기화",
// "clear", "timezone …") are not read as a reset or another setting.
func TestMySettingsSlashRegionIsAPlace(t *testing.T) {
	cmd, ok := ByName("mysettings")
	if !ok {
		t.Fatal("mysettings command missing")
	}
	for _, v := range []string{"초기화", "reset", "시간대 Asia/Tokyo", "Seoul"} {
		_, in := cmd.Build(map[string]string{"region": v}, InvokeContext{})
		if want := map[string]any{"weather_region": v}; !reflect.DeepEqual(in, want) {
			t.Errorf("/mysettings region:%q -> %v, want %v", v, in, want)
		}
	}
	// typed words still carry verbs
	typed := InvokeContext{Typed: true}
	for v, want := range map[string]map[string]any{
		"초기화":            {"reset": true},
		"시간대 Asia/Tokyo": {"timezone": "Asia/Tokyo"},
		"서울":             {"weather_region": "서울"},
		"지역 부산":          {"weather_region": "부산"},
	} {
		if _, in := cmd.Build(map[string]string{"region": v}, typed); !reflect.DeepEqual(in, want) {
			t.Errorf("!내설정 %s -> %v, want %v", v, in, want)
		}
	}
}

// The period words after !랭킹 come from every language's catalog; anything else is the default week.
func TestRankingPeriodWordsFromTheCatalogs(t *testing.T) {
	for v, want := range map[string]string{
		"오늘": "today", "today": "today", "今日": "today", "hoy": "today",
		"이번 주": "week", "week": "week", "今週": "week",
		"이번달": "month", "This Month": "month", "本月": "month", "mes": "month",
		"전체": "all", "ALL": "all", "累計": "all", "todo": "all",
		"": "week", "어제": "week", "yesterday": "week",
	} {
		if got := rankingPeriod(v); got != want {
			t.Errorf("rankingPeriod(%q) = %q, want %q", v, got, want)
		}
	}
}
