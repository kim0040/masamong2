package kakao

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	body := `{"documents":[{"title":"<b>마사몽</b> 봇","contents":"디스코드 <b>봇</b>","url":"https://example.com/a","datetime":"2026-09-15T00:00:00.000+09:00"},{"title":"no url","contents":"x","url":""}],"meta":{"total_count":2,"pageable_count":2,"is_end":true}}`
	r, err := Parse("application/json;charset=UTF-8", []byte(body))
	if err != nil || len(r.Documents) != 1 || r.Documents[0].Title != "마사몽 봇" || !r.IsEnd {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := Parse("application/json", []byte(`{"errorType":"AccessDeniedError","message":"wrong appKey"}`)); err == nil {
		t.Fatal("error envelope")
	}
	if _, err := Parse("application/json", []byte(`{"meta":{}}`)); err == nil {
		t.Fatal("missing documents")
	}
}

// The keyword search of Kakao Local (Kakao Map), in the shape its guide shows (read 2026-10-01).
func TestParsePlaces(t *testing.T) {
	body := []byte(`{"meta":{"same_name":{"region":[],"keyword":"맛집","selected_region":"서울 강남구 강남역"},"pageable_count":45,"total_count":4975,"is_end":false},"documents":[{"place_name":"알부자 강남본점","distance":"","place_url":"http://place.map.kakao.com/11463001","category_name":"음식점 > 한식 > 해물,생선","address_name":"서울 강남구 역삼동 819-3","road_address_name":"서울 강남구 강남대로98길 14","id":"11463001","phone":"02-508-4629","category_group_code":"FD6","category_group_name":"음식점","x":"127.03","y":"37.50"},{"place_name":""}]}`)
	ps, err := ParsePlaces("application/json", body)
	if err != nil || len(ps.Places) != 1 || ps.Region != "서울 강남구 강남역" {
		t.Fatalf("%+v %v", ps, err)
	}
	if p := ps.Places[0]; p.Name != "알부자 강남본점" || p.RoadAddress != "서울 강남구 강남대로98길 14" || p.Phone != "02-508-4629" || p.URL != "http://place.map.kakao.com/11463001" {
		t.Fatalf("%+v", p)
	}
	if ps, err := ParsePlaces("application/json", []byte(`{"meta":{"same_name":{"region":[],"keyword":"맛집","selected_region":""}},"documents":[{"place_name":"어딘가"}]}`)); err != nil || ps.Region != "" || len(ps.Places) != 1 {
		t.Fatalf("a query without an area: %+v %v", ps, err)
	}
	if _, err := ParsePlaces("application/json", []byte(`{"errorType":"AccessDeniedError","message":"App(…) disabled OPEN_MAP_AND_LOCAL service."}`)); err == nil || !strings.Contains(err.Error(), "OPEN_MAP_AND_LOCAL") {
		t.Fatalf("a disabled Local service is an error: %v", err)
	}
}
