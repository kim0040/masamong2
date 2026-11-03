package fortune

import "testing"

func TestAnimalScores(t *testing.T) {
	// on a 자 day, by the rule in animals.go: 축 육합 75, 신·진 삼합 72, 자 itself 65, 오 충 45,
	// 묘 형 52, 미 해 54, 유 파 55, the rest 60
	want := [12]int{65, 75, 60, 52, 72, 60, 45, 54, 72, 55, 60, 60}
	if got := AnimalScores(0); got != want {
		t.Fatalf("자 day: %v", got)
	}
	// on a 오 day, 오 punishes itself: 60 + 5 − 9 = 56; 미 육합 75; 인·술 72; 자 충 45; 축 해 54; 묘 파 55
	got := AnimalScores(6)
	if got[6] != 56 || got[7] != 75 || got[2] != 72 || got[10] != 72 || got[0] != 45 || got[1] != 54 || got[3] != 55 {
		t.Fatalf("오 day: %v", got)
	}
	// 사 day with 신: 육합 +15, 형 −8, 파 −5 → 62
	if got := AnimalScores(5)[8]; got != 62 {
		t.Fatalf("사/신: %d", got)
	}
	r := AnimalRanking(0)
	if r[0] != 1 || r[1] != 4 || r[2] != 8 || r[11] != 6 {
		t.Fatalf("ranking %v", r)
	}
}

func TestAnimalHalfTriadNeedsTheCardinal(t *testing.T) {
	// on a 인 day: 오 is 반합 (+12) → 72; 술 shares 인오술 without the cardinal → no relation, 60
	got := AnimalScores(2)
	if got[6] != 72 || got[10] != 60 {
		t.Fatalf("인 day: 오 %d 술 %d", got[6], got[10])
	}
}
