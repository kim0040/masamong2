package application_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A place search with words Kakao cannot match ("분위기 좋은") is tried once more with the place and
// the kind of shop only, and the result says what was searched.
func TestPlacesSearchRetriesWithoutTheWordsInBetween(t *testing.T) {
	svc := setupSvc(t)
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "KakaoAK kakao-secret" {
			w.WriteHeader(401)
			return
		}
		q := r.URL.Query().Get("query")
		asked = append(asked, q)
		w.Header().Set("Content-Type", "application/json")
		famous := `{"place_name":"어느 도시 국밥","place_url":"http://place.map.kakao.com/2","category_name":"음식점 > 한식","address_name":"부산 사상구 1","road_address_name":"부산 사상구 어딘가로 1"}`
		switch q {
		case "근처 맛집":
			_, _ = w.Write([]byte(`{"meta":{"total_count":1,"same_name":{"selected_region":""}},"documents":[` + famous + `]}`))
			return
		case "맛있는 카페":
			_, _ = w.Write([]byte(`{"meta":{"total_count":1,"same_name":{"selected_region":""}},"documents":[` + famous + `]}`))
			return
		case "동네 카페":
			// nationwide picks, one of which happens to carry the word in its name
			_, _ = w.Write([]byte(`{"meta":{"total_count":3,"same_name":{"selected_region":""}},"documents":[` + famous + `,{"place_name":"동네커피","address_name":"대구 중구 1"},` + famous + `]}`))
			return
		case "골목 카페":
			_, _ = w.Write([]byte(`{"meta":{"total_count":1,"same_name":{"selected_region":""}},"documents":[{"place_name":"골목 카페 본점","address_name":"서울 1"}]}`))
			return
		case "홍대 카페":
		default:
			_, _ = w.Write([]byte(`{"meta":{"total_count":0},"documents":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"meta":{"total_count":1,"same_name":{"selected_region":"서울 마포구 홍대"}},"documents":[{"place_name":"골목 카페","place_url":"http://place.map.kakao.com/1","category_name":"음식점 > 카페","address_name":"서울 마포구 서교동 1","road_address_name":"서울 마포구 와우산로 1","phone":"02-000-0000"}]}`))
	}))
	defer srv.Close()
	t.Setenv("KAKAO_TEST", "kakao-secret")
	yaml := exec(svc, "config.show", nil).Output["yaml"].(string)
	yaml = strings.Replace(yaml, "base_url: https://dapi.kakao.com", "base_url: "+srv.URL, 1)
	yaml = strings.Replace(yaml, "credential_ref: env:MASAMONG_KAKAO_KEY", "credential_ref: env:KAKAO_TEST", 1)
	if ap := exec(svc, "config.apply", map[string]any{"yaml": yaml, "base_revision": 1}); !ap.OK {
		t.Fatalf("apply %+v", ap)
	}
	r := exec(svc, "places.search", map[string]any{"query": "홍대 분위기 좋은 카페"})
	places, _ := r.Output["places"].([]map[string]any)
	if !r.OK || r.Output["status"] != "ok" || r.Output["searched_as"] != "홍대 카페" || len(places) != 1 || places[0]["address"] != "서울 마포구 와우산로 1" {
		t.Fatalf("looser retry: %+v", r.Output)
	}
	if strings.Join(asked, "|") != "홍대 분위기 좋은 카페|홍대 카페" {
		t.Fatalf("asked %v", asked)
	}
	// the area need not come first: a try only counts when Kakao reads an area from it
	asked = nil
	r = exec(svc, "places.search", map[string]any{"query": "맛있는 홍대 카페"})
	if r.Output["searched_as"] != "홍대 카페" || strings.Join(asked, "|") != "맛있는 홍대 카페|맛있는 카페|홍대 카페" {
		t.Fatalf("area in the middle: %+v asked %v", r.Output, asked)
	}
	asked = nil
	if r := exec(svc, "places.search", map[string]any{"query": "강남역 맛집"}); r.Output["status"] != "no_results" || len(asked) != 1 {
		t.Fatalf("two words are searched once: %+v %v", r.Output, asked)
	}
	// "근처 맛집" names no area: Kakao's nationwide picks are not passed on, the answer asks where
	if r := exec(svc, "places.search", map[string]any{"query": "근처 맛집"}); r.Output["status"] != "no_area" || len(r.Output["places"].([]map[string]any)) != 0 || r.Output["message"] == nil {
		t.Fatalf("no area: %+v", r.Output)
	}
	// one result carrying the word by chance does not make it a place's name
	if r := exec(svc, "places.search", map[string]any{"query": "동네 카페"}); r.Output["status"] != "no_area" {
		t.Fatalf("a word in one name: %+v", r.Output)
	}
	// a shop's own name is a search of its own, wherever it is
	if r := exec(svc, "places.search", map[string]any{"query": "골목 카페"}); r.Output["status"] != "ok" {
		t.Fatalf("a name: %+v", r.Output)
	}
	if r := exec(svc, "places.search", map[string]any{"query": "홍대 카페"}); r.Output["status"] != "ok" || r.Output["area"] != "서울 마포구 홍대" {
		t.Fatalf("an area: %+v", r.Output)
	}
}
