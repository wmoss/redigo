// Copyright 2012 Gary Burd
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package redis

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

func cannotConvert(d reflect.Value, s interface{}) error {
	return fmt.Errorf("redigo: Scan cannot convert from %s to %s",
		reflect.TypeOf(s), d.Type())
}

func convertAssignBytes(d reflect.Value, s []byte) (err error) {
	switch d.Type().Kind() {
	case reflect.Float32, reflect.Float64:
		var x float64
		x, err = strconv.ParseFloat(string(s), d.Type().Bits())
		d.SetFloat(x)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var x int64
		x, err = strconv.ParseInt(string(s), 10, d.Type().Bits())
		d.SetInt(x)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		var x uint64
		x, err = strconv.ParseUint(string(s), 10, d.Type().Bits())
		d.SetUint(x)
	case reflect.Bool:
		var x bool
		x, err = strconv.ParseBool(string(s))
		d.SetBool(x)
	case reflect.String:
		d.SetString(string(s))
	case reflect.Slice:
		if d.Type().Elem().Kind() != reflect.Uint8 {
			err = cannotConvert(d, s)
		} else {
			d.SetBytes(s)
		}
	default:
		err = cannotConvert(d, s)
	}
	return
}

func convertAssignInt(d reflect.Value, s int64) (err error) {
	switch d.Type().Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		d.SetInt(s)
		if d.Int() != s {
			err = strconv.ErrRange
			d.SetInt(0)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if s < 0 {
			err = strconv.ErrRange
		} else {
			x := uint64(s)
			d.SetUint(x)
			if d.Uint() != x {
				err = strconv.ErrRange
				d.SetUint(0)
			}
		}
	case reflect.Bool:
		d.SetBool(s != 0)
	default:
		err = cannotConvert(d, s)
	}
	return
}

func convertAssignValues(d reflect.Value, s []interface{}) (err error) {
	if d.Type().Kind() != reflect.Slice {
		return cannotConvert(d, s)
	}
	if len(s) > d.Cap() {
		d.Set(reflect.MakeSlice(d.Type(), len(s), len(s)))
	} else {
		d.SetLen(len(s))
	}
	for i := 0; i < len(s); i++ {
		switch s := s[i].(type) {
		case []byte:
			err = convertAssignBytes(d.Index(i), s)
		case int64:
			err = convertAssignInt(d.Index(i), s)
		default:
			err = cannotConvert(d, s)
		}
		if err != nil {
			break
		}
	}
	return
}

func convertAssign(d interface{}, s interface{}) (err error) {
	// Handle the most common destination types using type switches and
	// fall back to reflection for all other types.
	switch s := s.(type) {
	case nil:
		// ingore
	case []byte:
		switch d := d.(type) {
		case *string:
			*d = string(s)
		case *int:
			*d, err = strconv.Atoi(string(s))
		case *bool:
			*d, err = strconv.ParseBool(string(s))
		case *[]byte:
			*d = s
		case *interface{}:
			*d = s
		case nil:
			// skip value
		default:
			if d := reflect.ValueOf(d); d.Type().Kind() != reflect.Ptr {
				err = cannotConvert(d, s)
			} else {
				err = convertAssignBytes(d.Elem(), s)
			}
		}
	case int64:
		switch d := d.(type) {
		case *int:
			x := int(s)
			if int64(x) != s {
				err = strconv.ErrRange
				x = 0
			}
			*d = x
		case *bool:
			*d = s != 0
		case *interface{}:
			*d = s
		case nil:
			// skip value
		default:
			if d := reflect.ValueOf(d); d.Type().Kind() != reflect.Ptr {
				err = cannotConvert(d, s)
			} else {
				err = convertAssignInt(d.Elem(), s)
			}
		}
	case []interface{}:
		switch d := d.(type) {
		case *[]interface{}:
			*d = s
		case *interface{}:
			*d = s
		case nil:
			// skip value
		default:
			if d := reflect.ValueOf(d); d.Type().Kind() != reflect.Ptr {
				err = cannotConvert(d, s)
			} else {
				err = convertAssignValues(d.Elem(), s)
			}
		}
	case Error:
		err = s
	default:
		err = cannotConvert(reflect.ValueOf(d), s)
	}
	return
}

// Scan copies from the multi-bulk src to the values pointed at by dest.
//
// The values pointed at by dest must be a numeric type, boolean, string,
// []byte, interface{} or a slice of these types. Scan uses the standard
// strconv package to convert bulk values to numeric and boolean types.
//
// If a dest value is nil, then the corresponding src value is skipped.
//
// If the multi-bulk value is nil, then the corresponding dest value is not
// modified.
//
// To enable easy use of Scan in a loop, Scan returns the slice of src
// following the copied values.
func Scan(src []interface{}, dest ...interface{}) ([]interface{}, error) {
	if len(src) < len(dest) {
		return nil, errors.New("redigo: Scan multibulk short")
	}
	var err error
	for i, d := range dest {
		err = convertAssign(d, src[i])
		if err != nil {
			break
		}
	}
	return src[len(dest):], err
}

type fieldSpec struct {
	name  string
	index []int
	//omitEmpty bool
}

type structSpec struct {
	m map[string]*fieldSpec
	l []*fieldSpec
}

func (ss *structSpec) fieldSpec(name []byte) *fieldSpec {
	return ss.m[string(name)]
}

func compileStructSpec(t reflect.Type, depth map[string]int, index []int, ss *structSpec) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		switch {
		case f.PkgPath != "":
			// Ignore unexported fields.
		case f.Anonymous:
			// TODO: Handle pointers. Requires change to decoder and
			// protection against infinite recursion.
			if f.Type.Kind() == reflect.Struct {
				compileStructSpec(f.Type, depth, append(index, i), ss)
			}
		default:
			fs := &fieldSpec{name: f.Name}
			tag := f.Tag.Get("redis")
			p := strings.Split(tag, ",")
			if len(p) > 0 {
				if p[0] == "-" {
					continue
				}
				if len(p[0]) > 0 {
					fs.name = p[0]
				}
				for _, s := range p[1:] {
					switch s {
					//case "omitempty":
					//  fs.omitempty = true
					default:
						panic(errors.New("redigo: unknown field flag " + s + " for type " + t.Name()))
					}
				}
			}
			d, found := depth[fs.name]
			if !found {
				d = 1 << 30
			}
			switch {
			case len(index) == d:
				// At same depth, remove from result.
				delete(ss.m, fs.name)
				j := 0
				for i := 0; i < len(ss.l); i++ {
					if fs.name != ss.l[i].name {
						ss.l[j] = ss.l[i]
						j += 1
					}
				}
				ss.l = ss.l[:j]
			case len(index) < d:
				fs.index = make([]int, len(index)+1)
				copy(fs.index, index)
				fs.index[len(index)] = i
				depth[fs.name] = len(index)
				ss.m[fs.name] = fs
				ss.l = append(ss.l, fs)
			}
		}
	}
}

var (
	structSpecMutex  sync.RWMutex
	structSpecCache  = make(map[reflect.Type]*structSpec)
	defaultFieldSpec = &fieldSpec{}
)

func structSpecForType(t reflect.Type) *structSpec {

	structSpecMutex.RLock()
	ss, found := structSpecCache[t]
	structSpecMutex.RUnlock()
	if found {
		return ss
	}

	structSpecMutex.Lock()
	defer structSpecMutex.Unlock()
	ss, found = structSpecCache[t]
	if found {
		return ss
	}

	ss = &structSpec{m: make(map[string]*fieldSpec)}
	compileStructSpec(t, make(map[string]int), nil, ss)
	structSpecCache[t] = ss
	return ss
}

// ScanStruct scans a multi-bulk src containing alternating names and values to
// a struct. The HGETALL and CONFIG GET commands return replies in this format.
//
// ScanStruct uses the struct field name to match values in the response. Use
// 'redis' field tag to override the name:
//
//      Field int `redis:"myName"`
//
// Fields with the tag redis:"-" are ignored.
func ScanStruct(src []interface{}, dest interface{}) error {
	d := reflect.ValueOf(dest)
	if d.Kind() != reflect.Ptr || d.IsNil() {
		return errors.New("redigo: ScanStruct value must be non-nil pointer")
	}
	d = d.Elem()
	ss := structSpecForType(d.Type())

	if len(src)%2 != 0 {
		return errors.New("redigo: ScanStruct expects even number of values in values")
	}

	for i := 0; i < len(src); i += 2 {
		name, ok := src[i].([]byte)
		if !ok {
			return errors.New("redigo: ScanStruct key not a bulk value")
		}
		fs := ss.fieldSpec(name)
		if fs == nil {
			continue
		}
		f := d.FieldByIndex(fs.index)
		var err error
		switch s := src[i+1].(type) {
		case nil:
			// ignore
		case []byte:
			err = convertAssignBytes(f, s)
		case int64:
			err = convertAssignInt(f, s)
		default:
			err = cannotConvert(f, s)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// AppendStruct scans a struct containing values and turns then into alternating
// key and value pairs. The HMSET and CONFIG SET commands take arguments of this
// type. AppendStruct is often used in conjuction with ScanStruct for saving
// back the modified values.
//
// AppendStruct uses the struct field name to match values in the response. Use
// 'redis' field tag to override the name:
//
//      Field int `redis:"myName"`
//
// Fields with the tag redis:"-" are ignored.
func AppendStruct(args []interface{}, src interface{}) ([]interface{}, error) {
	v := reflect.ValueOf(src)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil, errors.New("redigo: AppendStruct argument must not be nil")
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil, errors.New("redigo: AppendStruct argument must be a struct or pointer to a struct")
	}
	ss := structSpecForType(v.Type())
	for _, fs := range ss.l {
		fv := v.FieldByIndex(fs.index)
		args = append(args, fs.name, fv.Interface())
	}
	return args, nil
}

// FlattenStruct is the same as AppendStruct, but it panics on errors.
// See AppendStruct for full explanation.
func FlattenStruct(args []interface{}, src interface{}) []interface{} {
	res, err := AppendStruct(args, src)
	if err != nil {
		panic(err)
	}
	return res
}
