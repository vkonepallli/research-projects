// Package inject provides utilities for mapping and injecting dependencies in various ways.
package inject

import (
	"fmt"
	"reflect"
)

// Injector represents an interface for mapping and injecting dependencies into structs
// and function arguments.
type Injector interface {
	Applicator
	Invoker
	TypeMapper
	// SetParent sets the parent of the injector. If the injector cannot find a
	// dependency in its Type map it will check its parent before returning an
	// error.
	SetParent(Injector)
}

// Applicator represents an interface for mapping dependencies to a struct.
type Applicator interface {
	// Maps dependencies in the Type map to each field in the struct
	// that is tagged with 'inject'. Returns an error if the injection
	// fails.
	Apply(interface{}) error
}

// Invoker represents an interface for calling functions via reflection.
type Invoker interface {
	// Invoke attempts to call the interface{} provided as a function,
	// providing dependencies for function arguments based on Type. Returns
	// a slice of reflect.Value representing the returned values of the function.
	// Returns an error if the injection fails.
	Invoke(interface{}) ([]reflect.Value, error)
}

// TypeMapper represents an interface for mapping interface{} values based on type.
type TypeMapper interface {
	// Maps the interface{} value based on its immediate type from reflect.TypeOf.
	Map(interface{}) TypeMapper
	// Maps the interface{} value based on the pointer of an Interface provided.
	// This is really only useful for mapping a value as an interface, as interfaces
	// cannot at this time be referenced directly without a pointer.
	MapTo(interface{}, interface{}) TypeMapper
	// Provides a possibility to directly insert a mapping based on type and value.
	// This makes it possible to directly map type arguments not possible to instantiate
	// with reflect like unidirectional channels.
	Set(reflect.Type, reflect.Value) TypeMapper
	// Returns the Value that is mapped to the current type. Resolves factory methods
	// that return the wanted type. Returns a zeroed Value if the Type has not been mapped.
	Get(reflect.Type) reflect.Value
	// Returns the Value that is mapped to the current type. Does not resolve factory
	// methods. Returns a zeroed Value if the Type has not been mapped.
	GetRaw(reflect.Type) reflect.Value
}

type injector struct {
	factories map[reflect.Type]reflect.Value
	values    map[reflect.Type]reflect.Value
	parent    Injector
}

// InterfaceOf dereferences a pointer to an Interface type.
// It panics if value is not an pointer to an interface.
func InterfaceOf(value interface{}) reflect.Type {
	t := reflect.TypeOf(value)

	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Interface {
		panic("Called inject.InterfaceOf with a value that is not a pointer to an interface. (*MyInterface)(nil)")
	}

	return t
}

// New returns a new Injector.
func New() Injector {
	return &injector{
		values:    make(map[reflect.Type]reflect.Value),
		factories: make(map[reflect.Type]reflect.Value),
	}
}

// Invoke attempts to call the interface{} provided as a function,
// providing dependencies for function arguments based on Type.
// Returns a slice of reflect.Value representing the returned values of the function.
// Returns an error if the injection fails.
// It panics if f is not a function
func (inj *injector) Invoke(f interface{}) (ret []reflect.Value, err error) {
	defer recoverResolvePanic(&err)

	t := reflect.TypeOf(f)
	var args = make([]reflect.Value, t.NumIn()) //Panic if t is not kind of Func
	for i, arg := range args {
		argType := t.In(i)
		arg = inj.Get(argType)
		if !arg.IsValid() {
			return nil, fmt.Errorf("value not found for type %v", argType)
		}

		args[i] = arg
	}

	ret = reflect.ValueOf(f).Call(args)
	return
}

// Maps dependencies in the Type map to each field in the struct
// that is tagged with 'inject'.
// Returns an error if the injection fails.
func (inj *injector) Apply(val interface{}) (err error) {
	defer recoverResolvePanic(&err)

	v := reflect.ValueOf(val)

	for v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return nil // Should not panic here ?
	}

	t := v.Type()

	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		structField := t.Field(i)
		if f.CanSet() && (structField.Tag == "inject" || structField.Tag.Get("inject") != "") {
			ft := f.Type()
			v := inj.Get(ft)
			if !v.IsValid() {
				return fmt.Errorf("value not found for type %v", ft)
			}

			f.Set(v)
		}

	}

	return
}

// Maps the concrete value of val to its dynamic type using reflect.TypeOf,
// It returns the TypeMapper registered in.
func (inj *injector) Map(val interface{}) TypeMapper {
	return inj.Set(reflect.TypeOf(val), reflect.ValueOf(val))
}

func (inj *injector) MapTo(val interface{}, ifacePtr interface{}) TypeMapper {
	return inj.Set(InterfaceOf(ifacePtr), reflect.ValueOf(val))
}

// Maps the given reflect.Type to the given reflect.Value and returns
// the TypeMapper the mapping has been registered in.
func (inj *injector) Set(typ reflect.Type, val reflect.Value) TypeMapper {
	if val.Kind() == reflect.Func {
		if typ.Kind() == reflect.Func {
			typ = val.Type().Out(0)
		}
		inj.factories[typ] = val
	} else {
		inj.values[typ] = val
	}
	return inj
}

func (inj *injector) Get(typ reflect.Type) reflect.Value {
	val := inj.GetRaw(typ)

	if val.IsValid() && val.Kind() == reflect.Func {
		val = inj.resolve(val, []reflect.Type{})
	}

	return val
}

func (inj *injector) GetRaw(typ reflect.Type) reflect.Value {
	val := inj.values[typ]
	if val.IsValid() {
		return val
	}

	// no concrete types found, try to find a factory
	if !val.IsValid() {
		val = inj.factories[typ]
	}

	// no concrete type or factory found, try to find implementors
	// if t is an interface
	if typ.Kind() == reflect.Interface {
		for k, v := range inj.values {
			if k.Implements(typ) {
				val = v
				break
			}
		}
	}

	// still no type found, try to look it up on the parent
	if !val.IsValid() && inj.parent != nil {
		val = inj.parent.GetRaw(typ)
	}

	return val
}

func (inj *injector) resolve(fac reflect.Value, chain []reflect.Type) reflect.Value {
	chainLen := len(chain)
	if chainLen > 1 {
		want := chain[chainLen-1]
		for _, t := range chain[:chainLen-1] {
			if want == t {
				panic(resolveError{chain: chain, message: "dependency loop"})
			}
		}
	}

	facType := fac.Type()
	args := make([]reflect.Value, facType.NumIn())
	for i := range args {
		argType := facType.In(i)

		if cachedVal, ok := inj.values[argType]; ok {
			args[i] = cachedVal
			continue
		}

		args[i] = inj.GetRaw(argType)
		if !args[i].IsValid() {
			panic(resolveError{chain: chain, fac: fac})
		}

		if args[i].Kind() == reflect.Func {
			args[i] = inj.resolve(args[i], append(chain, facType, argType))
		}
		inj.values[argType] = args[i]
	}

	return fac.Call(args)[0]
}

func (inj *injector) SetParent(parent Injector) {
	inj.parent = parent
}
