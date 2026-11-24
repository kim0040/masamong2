package config

import "reflect"

// Clone is a deep copy of the document: every field is kept, and no map, slice, pointer or interface
// value of the copy is shared with d. A typed config change is built on a clone (mutateConfig), so a
// change that is refused (a failed check, a stale revision) never reaches the live document, and no
// field can be left out of the saved file the way a hand-written copy once left out the per-server
// blocked users, privacy options and log channel.
func (d Document) Clone() Document {
	return deepCopy(reflect.ValueOf(d)).Interface().(Document)
}

func deepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.New(v.Type().Elem())
		c.Elem().Set(deepCopy(v.Elem()))
		return c
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			c.SetMapIndex(deepCopy(it.Key()), deepCopy(it.Value()))
		}
		return c
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			c.Index(i).Set(deepCopy(v.Index(i)))
		}
		return c
	case reflect.Array:
		c := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			c.Index(i).Set(deepCopy(v.Index(i)))
		}
		return c
	case reflect.Struct:
		c := reflect.New(v.Type()).Elem()
		c.Set(v) // every field, unexported ones too (they hold plain values only)
		for i := 0; i < v.NumField(); i++ {
			if c.Field(i).CanSet() {
				c.Field(i).Set(deepCopy(v.Field(i)))
			}
		}
		return c
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.New(v.Type()).Elem()
		c.Set(deepCopy(v.Elem()))
		return c
	}
	return v
}
