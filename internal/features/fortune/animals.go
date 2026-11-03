package fortune

import "sort"

// The twelve animals (띠) of a day, for a public line that needs no birth data: each animal is its year
// branch (0 쥐 자 … 11 돼지 해), scored by how that branch meets today's day branch. Rule: from
// ScoreBase, 육합 +15, 반합 +12 (only with the cardinal 자·오·묘·유, as Relate gives it), the same branch +5 (or −4 when it punishes itself: 진 오 유 해),
// 충 −15, 형 −8, 해 −6, 파 −5; clamped like the personal scores.
var animalRel = [Relations]int{15, 12, -15, -8, -5, -6}

// AnimalScores are the twelve animals' scores on a day whose branch is dayBranch.
func AnimalScores(dayBranch int) [12]int {
	var out [12]int
	for b := range out {
		s := ScoreBase
		if b == dayBranch {
			s += 5
		}
		for _, r := range Relate(dayBranch, b) {
			if b == dayBranch && r == RelPunish {
				s -= 9 // the +5 above turns into −4 for a self-punishing branch
				continue
			}
			s += animalRel[r]
		}
		out[b] = min(ScoreMax, max(ScoreMin, s))
	}
	return out
}

// AnimalRanking are the twelve branches from the best day to the quietest; a tie keeps the 자…해 order.
func AnimalRanking(dayBranch int) []int {
	scores := AnimalScores(dayBranch)
	out := make([]int, 12)
	for i := range out {
		out[i] = i
	}
	sort.SliceStable(out, func(a, b int) bool { return scores[out[a]] > scores[out[b]] })
	return out
}
