package websearch

import "testing"

func TestSearchFeeCoversEveryRequest(t *testing.T) {
	cases := []struct {
		kind, depth string
		extra       int
		want        float64
	}{
		{"exa", DepthDeep, 3, 0.015},
		{"linkup", DepthDeep, 2, 0.055},
		{"linkup", DepthStandard, 0, 0.0055},
		{"tavily", DepthDeep, 3, 0.016 * 3}, // two reworded queries at most
		{"kakao_search", DepthDeep, 1, 0},
		{"tinyfish", DepthDeep, 2, 0},
		{"brave", DepthQuick, 2, 0.005}, // reworded queries only count for deep
		{"newkind", DepthStandard, 0, 0.01},
	}
	for _, c := range cases {
		if got := SearchFee(c.kind, c.depth, c.extra); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("SearchFee(%s,%s,%d) = %v, want %v", c.kind, c.depth, c.extra, got, c.want)
		}
	}
	if Requests("kakao_search", DepthDeep, 2) != 4 || Requests("serper", DepthDeep, 5) != 3 {
		t.Fatal("requests")
	}
}
