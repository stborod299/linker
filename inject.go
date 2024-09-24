// Copyright 2018 The logrange Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package linker

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"
)

type (
	// Injector struct keeps the list of a program components and controls their life-cycle.
	// Injector's life-cycle consists of the following phases, which are executed
	// sequentually:
	//
	// 1. Registration phase. Components are added to the injector, or registered
	// there via Register() function.
	//
	// 2. Construct phase. In the phase, the Injector walks over fields
	// of every registered component and it adds appropriate dependency found in
	// between other components. The phase is started by Init() call.
	//
	// 3. Initialization phase. On the phase the Injector builds components
	// dependencies graph and initialize each component in an order. Dependant
	// components must be initialize after their dependencies. This phase is
	// done in context of Init() call.
	//
	// 4. Shutdown phase. On the phase components are de-initialied or being
	// shutdowned. Components are shutdowned in a reverse of their
	// initialization order. The phase is performed by Shutdown() call
	//
	// Injector doesn't allow to have cycles in the component dependency graph.
	// A dependency cycle is a situation when component A has dependency from
	// a component B, which has directly, or indirectly (through another direct
	// dependency) a dependency from the component A.
	//
	// The implementation is not concurrent and must be used within one go-routine
	// or be synchronized properly. Normal flow is as the following:
	//
	//   Register()
	//   Init()
	//   Shutdown()
	//
	// The Injector uses fail-fast strategy and it panics if any error happens.
	// Shutdown must not be called if Init() was panicing or not called at all.
	Injector struct {
		log     Logger
		tagName string
		// named contains map of name:*component pairs
		named map[string]*component
		// comps map contains Component.Value:*component pairs and it keeps the
		// list of all components whuch must be initialized
		comps map[interface{}]*component

		// slice of initialized components
		iComps []*component
	}

	// Dependency describes component dependency.
	Dependency struct {
		// Name of the dependency component
		Name string

		// Type of the component value
		Type reflect.Type

		// True if component value may be omitted
		Optional bool

		// Value which should be used in case of omitted component value
		// It's optional. In case if optional component is missed and no default value set - nil will be passed
		// to component produce. Component is responsible to handle this nil
		Default any
	}

	// ComponentIF describes component in terms of relations with other components
	ComponentIF interface {
		// GetName returns name of the component or empty string if component doensn't have name
		GetName() string

		// Produce returns instance of the components according to resolved values of dependencies
		Produce([]any) any

		// Returns list of component dependencies
		In() []Dependency

		// Out returns type of the value provided by component
		Out() reflect.Type

		// Identity returns unique value of container. It's used to prevent repetitive registration
		Identity() any
	}

	// PostConstructor interface. Components can implement it to provide a post-
	// construct action (see PostConstruct).
	PostConstructor interface {
		// PostConstruct is called by Injector in the end of construct phase.
		// If a component implements the interface, the PostConstruct() will be
		// called immediately after all dependencies are resolved and injected.
		// PostConstruct is always called before Init() (see Initializer) and
		// Shutdown() (see  Shutdowner) if they are implemented
		//
		// PostConstruct is supposed to be quick and should not block the calling
		// go-routine. If some initialization or blocking could happen, it must
		// be done in Init() method
		PostConstruct()
	}

	// Initializer interface provides a component initialization functionality.
	// A component can implement the interface to provide Init() function where
	// the component can acquire resources and perform some initialization.
	Initializer interface {
		// Init will be called by Injector in a specific order after all components
		// are constructed. The order of calling the Init() functions is defined
		// by the dependency injection graph. Init() function of a component will
		// be called after initializing all dependencies of the component.
		//
		// If the initialization of the component is failed a non-nil result
		// must be returned. This case Injector will shutdown
		// all previously initialized components and fail the initialization phase
		// returning an error in its Init() calle.
		//
		// if the Init() is ever called, it always happens before Shutdown().
		Init(ctx context.Context) error
	}

	// Shutdowner interface allows to provide Shutdown() function which will be
	// called by Injector to shutdown the component properly. A component can implement
	// the interface to release all resources, acquired on initialization phase.
	Shutdowner interface {
		// Shutdown allows to shutdown a component. Injector calls the function
		// on shutdown phase. It never calls Shutdown() for the components, that
		// were not initialized successfully (Init() was not called, or it returned
		// an error)
		Shutdown()
	}

	// Logger interface is used by Injector to print its logs
	Logger interface {
		// Info prints an information message into the log
		Info(args ...interface{})

		// Debug prints a debug message into the log
		Debug(args ...interface{})
	}

	// component struct wraps a component and it keeps its internal init status
	component struct {
		value     interface{}
		tp        reflect.Type
		val       reflect.Value
		deps      map[*component]struct{}
		initOrder int
		comp      ComponentIF
		depSlice  []*component
	}

	nullLogger struct {
	}

	stdLogger struct {
	}
)

const (
	// DefaultTagName contains the tag name, used by the Injector by default
	DefaultTagName = "inject"
)

// New creates new Injector instance
func New() *Injector {
	i := new(Injector)
	i.named = make(map[string]*component)
	i.comps = make(map[interface{}]*component)
	i.log = nullLogger{}
	i.tagName = DefaultTagName
	return i
}

// SetLogger allows to set up the injector logger
func (i *Injector) SetLogger(log Logger) {
	i.log = log
}

// Register called to register programming components. It must be called before
// Init().
func (i *Injector) Register(comps ...ComponentIF) {
	for _, c := range comps {
		val := c.Identity()
		pc, ok := i.comps[val]
		if !ok {
			// well, we don't have the wrapper yet, creating a new one
			pc = new(component)
			i.comps[val] = pc
			pc.comp = c
			pc.tp = c.Out()
		}

		name := c.GetName()
		i.log.Info("Registering component with type ", pc.tp, " as \"", name, "\"")

		if name != "" {
			if _, ok := i.named[name]; ok {
				i.panic("Register(): the name " + name + " already registered.")
			}
			i.named[name] = pc
		}
	}
}

// Init initializes components. It does the following things in a row:
// 1. Inject all dependencies
// 2. it calls PostConsturct() functions for PostConstructors
// 3. it builds an initialization order and calls Init() for Initializors
//
// If any error happens, it panics. If an error happens on initialization phase,
// it's shutting down already initialized components, then panics. If the
// method is over with no panic, Shutdown() must be called to free all resources
// properly
func (i *Injector) Init(ctx context.Context) {
	i.log.Info("Init(): ", len(i.comps), " components are going to be initialized, ", len(i.named), " names are registered.")

	// Resolve dependencies and fill them inside components
	i.resoveDependencies()

	// setting up init order via components list sort
	iList := make([]*component, 0, len(i.comps))
	for _, c := range i.comps {
		if c.getInitOrder() <= 0 {
			i.panic("Internal error, init order should be positive, but the component has wrong one " + c.String())
		}
		iList = append(iList, c)
	}
	sort.Slice(iList, func(i int, j int) bool { return iList[i].getInitOrder() < iList[j].getInitOrder() })

	// Produce all components and inject all values
	for _, c := range iList {
		c.construct()
		c.postConstruct(i.log)
	}

	// Now we're ready to init
	i.iComps = make([]*component, 0, len(iList))
	for idx, c := range iList {
		err := c.init(ctx, i.log)
		if err != nil {
			em := fmt.Sprintf("An error from Init of %s which was #%d in the order, err=%s. Will roll things back and panicing", c.tp, idx, err)
			i.Shutdown()
			i.panic(em)
		}
		i.iComps = append(i.iComps, c)
	}

	i.log.Info("Init(): successfully done.")
}

// Shutdown calls Shutdown() function for all Shutdowners. It must be called only
// when Init() is over successfully. Must not be called if Init() is not invoked
// or panicked before.
func (i *Injector) Shutdown() {
	i.log.Info("Shutdown(): ", len(i.iComps), " components")
	for idx := len(i.iComps) - 1; idx >= 0; idx-- {
		c := i.iComps[idx]
		err := c.shutdown(i.log)
		if err != nil {
			i.log.Info("An error while  shutdown. err=", err)
		}
	}
	i.iComps = nil
	i.comps = nil
	i.named = nil
	i.log.Info("Shutdown(): done.")
}

func (i *Injector) resoveDependencies() {
	for _, c := range i.comps {
		in := c.comp.In()

		for _, inDep := range in {
			dep := i.lookup(inDep.Name, inDep.Type)
			if dep == nil {
				if !inDep.Optional {
					panic(fmt.Sprintf("Can't lookup component for %s:%s", inDep.Name, inDep.Type))
				}
			}
			// Unresolved optional dependencies will be added as nils
			c.addDep(dep)
		}
	}
}

// lookup performs component lookup via registry of components.
// It returns nil in case if lookup didn't have success or best candidate component on success.
func (i *Injector) lookup(name string, fType reflect.Type) *component {
	if name != "" {
		c1, ok := i.named[name]
		if !ok {
			// TODO: panic?
			return nil
		}

		if !c1.tp.AssignableTo(fType) {
			i.panic(fmt.Sprintf("Requested component %s:%s but resolved to incompatible type %s", name, fType, c1.tp))
		}

		return c1
	}

	// search for a assignable type to the field
	var found *component
	for _, uc := range i.comps {
		if uc.tp.AssignableTo(fType) {
			if found != nil {
				i.panic(fmt.Sprintf("Ambiguous component assignment for the field %s with type %s in the type %s. Both unnamed components %s and %s, matched to the field.",
					name, fType, fType, found.tp, uc.tp))
			}
			found = uc
		}
	}
	return found
}

func (i *Injector) panic(err string) {
	i.log.Info(err, " Panicing.")
	panic(err)
}

func (nl nullLogger) Info(args ...interface{}) {
}

func (nl nullLogger) Debug(args ...interface{}) {
}

func (sl stdLogger) Info(args ...interface{}) {
	fmt.Printf("%s INFO: %s\n", time.Now().Format("03:04:05.000"), fmt.Sprint(args...))
}

func (sl stdLogger) Debug(args ...interface{}) {
	fmt.Printf("%s DEBUG: %s\n", time.Now().Format("03:04:05.000"), fmt.Sprint(args...))
}

func isStructPtr(t reflect.Type) bool {
	return t.Kind() == reflect.Ptr && t.Elem().Kind() == reflect.Struct
}

func (c *component) construct() {
	in := make([]any, len(c.depSlice))
	for idx, depC := range c.depSlice {
		if depC != nil {
			in[idx] = c.depSlice[idx].value
		} else {
			in[idx] = nil
		}
	}

	c.value = c.comp.Produce(in)
	c.val = reflect.ValueOf(c.value)
}

func (c *component) postConstruct(log Logger) {
	if pc, ok := c.value.(PostConstructor); ok {
		log.Info("PostConstruct() for ", c.tp, " priority=", c.initOrder)
		pc.PostConstruct()
	}
}

func (c *component) init(ctx context.Context, log Logger) (err error) {
	defer func() {
		r := recover()
		if r != nil {
			err = fmt.Errorf("panic in Init() of %s recover=%v", c.tp, r)
		}
	}()

	if i, ok := c.value.(Initializer); ok {
		log.Info("Init() for ", c.tp, " priority=", c.initOrder)
		err = i.Init(ctx)
	}

	return
}

func (c *component) shutdown(log Logger) (err error) {
	defer func() {
		r := recover()
		if r != nil {
			err = fmt.Errorf("panic in Shutdown() of %s", c.tp)
		}
	}()

	if s, ok := c.value.(Shutdowner); ok {
		log.Info("Shutdown() for ", c.tp, " priority=", c.initOrder)
		s.Shutdown()
	}

	return
}

func (c *component) addDep(c1 *component) {
	if c.deps == nil {
		c.deps = make(map[*component]struct{})
	}
	if c1 != nil {
		c.deps[c1] = struct{}{}
	}
	c.depSlice = append(c.depSlice, c1)
}

func (c *component) getInitOrder() int {
	if c.initOrder > 0 {
		return c.initOrder
	}

	blkList := make(map[*component]struct{})
	return c.setInitOrder(blkList)
}

func (c *component) setInitOrder(blkList map[*component]struct{}) int {
	if c.initOrder > 0 {
		return c.initOrder
	}

	o := 1
	blkList[c] = struct{}{}
	for c1 := range c.deps {
		if _, ok := blkList[c1]; ok {
			panic(fmt.Sprintf("Found a loop in the object graph dependencies. Component %s has a reference to %s, which alrady refers to the first one directly or indirectly",
				c, c1))
		}
		i := c1.setInitOrder(blkList)
		if i >= o {
			o = i + 1
		}
	}
	delete(blkList, c)
	c.initOrder = o
	return c.initOrder
}

func (c *component) String() string {
	return fmt.Sprintf("{tp=%s, }", c.tp)
}
