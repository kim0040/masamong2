package application

import (
	"testing"
	"time"

	"masamong/internal/config"
)

func TestBackupSlots(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	// Wednesday 2026-09-23 10:00 in Seoul
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, seoul)
	daily := config.Backup{Schedule: "daily", Hour: 4, Keep: 3}
	if s, _ := lastBackupSlot(now, daily, seoul); !s.Equal(time.Date(2026, 9, 23, 4, 0, 0, 0, seoul)) {
		t.Fatalf("daily last %v", s)
	}
	if s, _ := nextBackup(now, daily, seoul); !s.Equal(time.Date(2026, 9, 24, 4, 0, 0, 0, seoul)) {
		t.Fatalf("daily next %v", s)
	}
	early := time.Date(2026, 9, 23, 3, 0, 0, 0, seoul)
	if s, _ := lastBackupSlot(early, daily, seoul); !s.Equal(time.Date(2026, 9, 22, 4, 0, 0, 0, seoul)) {
		t.Fatalf("before the hour %v", s)
	}
	weekly := config.Backup{Schedule: "weekly", Hour: 4, Weekday: 0, Keep: 3}
	if s, _ := lastBackupSlot(now, weekly, seoul); !s.Equal(time.Date(2026, 9, 20, 4, 0, 0, 0, seoul)) {
		t.Fatalf("weekly last %v", s)
	}
	if s, _ := nextBackup(now, weekly, seoul); !s.Equal(time.Date(2026, 9, 27, 4, 0, 0, 0, seoul)) {
		t.Fatalf("weekly next %v", s)
	}
	if _, ok := lastBackupSlot(now, config.Backup{Schedule: "off"}, seoul); ok {
		t.Fatal("off has no slot")
	}
}
