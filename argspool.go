package xrpc

import (
	"reflect"
	"sync"
)

type argsPool struct {
	pools *sync.Pool
}

func (p *argsPool) New(t reflect.Type) interface{} {
	var argv reflect.Value

	if t.Kind() == reflect.Ptr { // reply must be ptr
		argv = reflect.New(t.Elem())
	} else {
		argv = reflect.New(t)
	}

	return argv.Interface()
}

func (p *argsPool) GenArgsForFunc(fn reflect.Value) (ins []interface{}, outs []interface{}, hasCtx, ok bool) {
	if fn.Kind() != reflect.Func {
		return
	}
	t := fn.Type()
	if t.NumIn() > 0 {
		ss := t.In(0).String()
		if ss == "*xrpc.XContext" {
			hasCtx = true
		}
	}
	for i := 0; i < t.NumIn(); i++ {
		ins = append(ins, p.New(t.In(i)))
	}
	for i := 0; i < t.NumOut(); i++ {
		outs = append(outs, p.New(t.Out(i)))
	}
	ok = true
	return
}

func Dispatch(r interface{}, vs ...interface{}) {
	if r == nil {
		return
	}
	var res []interface{}
	var ok bool
	if res, ok = r.([]interface{}); !ok {
		return
	}
	l := 0
	if len(res) > len(vs) {
		l = len(vs)
	} else {
		l = len(res)
	}
	if l <= 0 {
		return
	}
	for i := 0; i < l; i++ {
		if vs[i] == nil {
			continue
		}
		t := reflect.ValueOf(vs[i])
		if t.Type().Kind() != reflect.Ptr {
			continue
		}
		rt := reflect.ValueOf(res[i])
		if rt.Kind() == reflect.Ptr {
			rt = rt.Elem()
		}
		if t.Kind() == reflect.Ptr {
			t.Elem().Set(reflect.Indirect(rt))
		} else {
			t.Set(reflect.Indirect(rt))
		}
	}
}
