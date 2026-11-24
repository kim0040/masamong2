package config

import (
	"reflect"
	"testing"
)

// fill sets every reachable field of v to a non-zero value: one element in each map and slice, a value
// behind each pointer, a nested map behind each interface. Clone is then checked on a document with no
// empty corner, so a field or a nested reference it forgets fails the test.
func fill(t *testing.T, v reflect.Value, depth int) {
	if depth > 12 {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(t, v.Elem(), depth+1)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fill(t, k, depth+1)
		e := reflect.New(v.Type().Elem()).Elem()
		fill(t, e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(t, s.Index(0), depth+1)
		v.Set(s)
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fill(t, v.Index(i), depth+1)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				fill(t, v.Field(i), depth+1)
			}
		}
	case reflect.Interface:
		v.Set(reflect.ValueOf(map[string]any{"k": []any{"v"}}))
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
}

// shared reports a map, slice or pointer of b that is the same storage as the one at the same place in a.
func shared(a, b reflect.Value, path string) string {
	switch a.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if !a.IsNil() && a.UnsafePointer() == b.UnsafePointer() && (a.Kind() != reflect.Slice || a.Len() > 0) {
			return path
		}
	}
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !a.IsNil() {
			return shared(a.Elem(), b.Elem(), path)
		}
	case reflect.Map:
		for it := a.MapRange(); it.Next(); {
			if p := shared(it.Value(), b.MapIndex(it.Key()), path+"[k]"); p != "" {
				return p
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < a.Len(); i++ {
			if p := shared(a.Index(i), b.Index(i), path+"[i]"); p != "" {
				return p
			}
		}
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if a.Type().Field(i).IsExported() {
				if p := shared(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name); p != "" {
					return p
				}
			}
		}
	}
	return ""
}

func TestCloneKeepsEveryFieldAndSharesNothing(t *testing.T) {
	var d Document
	fill(t, reflect.ValueOf(&d).Elem(), 0)
	c := d.Clone()
	if !reflect.DeepEqual(c, d) {
		t.Fatal("the clone differs from the document")
	}
	if p := shared(reflect.ValueOf(d), reflect.ValueOf(c), "Document"); p != "" {
		t.Fatalf("the clone shares %s with the document", p)
	}
}
