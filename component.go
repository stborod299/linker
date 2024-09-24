package linker

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
)

type dependency struct {
	Dependency
	fieldIdx int
}

// Component provides instance itself
type Component struct {
	// Name contains the component name
	Name string

	// Value contains the component which is registered in the Injector
	Value any
}

// FComponent defines factory method to produce component.
// It acts as constructor and marks all incoming function arguments as mandatory dependencies.
// Type of component is resolved via first return argument of the function.
// Function can have error as second argument to specify if some dependencies wasn't resolved properly
// Component may implement PostConstruct to complete it's creation
type FComponent struct {
	Name     string
	Function any
}

// Function wrappers around components

func Instance(name string, value any) Component {
	return Component{Name: name, Value: value}
}

func Constructor(name string, value any) FComponent {
	return FComponent{Name: name, Function: value}
}

func (c Component) GetName() string {
	return c.Name
}

func (c Component) Produce(in []any) any {
	val := reflect.ValueOf(c.Value)
	if val.Kind() == reflect.Pointer {
		val = val.Elem()
	}

	deps, err := c.processTags()
	if err != nil {
		panic(err.Error())
	}

	for idx, dep := range deps {
		fieldVal := in[idx]
		if dep.Optional && fieldVal == nil {
			continue
		}

		val.Field(dep.fieldIdx).Set(reflect.ValueOf(fieldVal))
	}

	return c.Value
}

func (c Component) Identity() any {
	return reflect.ValueOf(c.Value)
}

func (c Component) In() []Dependency {
	deps, err := c.processTags()
	if err != nil {
		panic(err.Error())
	}

	outDeps := make([]Dependency, len(deps))
	for idx := range deps {
		outDeps[idx] = deps[idx].Dependency
	}

	return outDeps
}

func (c Component) processTags() ([]dependency, error) {
	val := reflect.ValueOf(c.Value)
	if !isStructPtr(val.Type()) {
		return []dependency{}, nil
	}

	// dereference pointer
	val = val.Elem()

	deps := make([]dependency, 0)
	for fi := 0; fi < val.NumField(); fi++ {
		f := val.Field(fi)
		fType := f.Type()
		fTag := string(val.Type().Field(fi).Tag)
		fName := val.Type().Field(fi).Name

		tagInfo, err := parseTag("inject", fTag)
		if err == errTagNotFound {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("could not parse tag for field %s of %s: %w", fName, val.Type(), err)
		}

		if !f.CanSet() {
			return nil, fmt.Errorf("could not set field %s valued of %s, cause it is unexported", fName, val.Type())
		}

		var defVal any
		if tagInfo.defVal != "" {
			var err error
			if defVal, err = produceDefaultValue(f, tagInfo.defVal); err != nil {
				return nil, fmt.Errorf("could not assign the default value=\"%s\" to the field %s", tagInfo.defVal, fName)
			}
		}

		deps = append(deps, dependency{
			Dependency: Dependency{Name: tagInfo.val, Type: fType, Optional: tagInfo.optional, Default: defVal},
			fieldIdx:   fi,
		})
	}
	return deps, nil
}

func (c Component) Out() reflect.Type {
	return reflect.TypeOf(c.Value)
}

func (f FComponent) GetName() string {
	return f.Name
}

func (f FComponent) Produce(vals []any) any {
	in := make([]reflect.Value, len(vals))
	for i := 0; i < len(vals); i++ {
		in[i] = reflect.ValueOf(vals[i])
	}

	out := reflect.ValueOf(f.Function).Call(in)

	// Allowed return values are (instance) and (instance, error)
	var err error
	if len(out) > 1 && out[1].Type().AssignableTo(reflect.TypeOf(err)) && !out[1].IsNil() {
		panic(fmt.Sprintf("constructor function returned error=%s", out[1].Interface()))
	}

	return out[0].Interface()
}

func (f FComponent) Identity() any {
	return reflect.ValueOf(f.Function)
}

func (f FComponent) In() []Dependency {
	t := reflect.ValueOf(f.Function).Type()
	if t.Kind() != reflect.Func {
		panic("FComponent must be a function")
	}

	in := make([]Dependency, t.NumIn())
	for i := 0; i < t.NumIn(); i++ {
		// All factory components are nameless and mandatory
		in[i] = Dependency{Type: t.In(i)}
	}
	return in
}

func (f FComponent) Out() reflect.Type {
	t := reflect.ValueOf(f.Function).Type()
	// TODO: catch Kind != Func
	return t.Out(0)
}

// produceDefaultValue converts default value string into instance of proper type.
func produceDefaultValue(field reflect.Value, s string) (any, error) {
	if len(s) == 0 {
		return reflect.New(field.Type()).Elem().Interface(), nil
	}

	obj := reflect.New(field.Type()).Interface()
	if t := reflect.TypeOf(obj); t.Kind() == reflect.Ptr &&
		t.Elem().Kind() == reflect.String {
		s = strconv.Quote(s)
	}

	err := json.Unmarshal([]byte(s), obj)
	if err != nil {
		return nil, err
	}

	return reflect.ValueOf(obj).Elem().Interface(), nil
}
