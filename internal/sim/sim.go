// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// TODO(mwhittaker): Figure out which parts of the simulator need to be in an
// internal package and which parts can be in weavertest. Everything is
// internal for now.
package sim

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"

	core "github.com/ServiceWeaver/weaver"
	"github.com/ServiceWeaver/weaver/internal/config"
	"github.com/ServiceWeaver/weaver/internal/reflection"
	"github.com/ServiceWeaver/weaver/internal/weaver"
	"github.com/ServiceWeaver/weaver/runtime"
	"github.com/ServiceWeaver/weaver/runtime/codegen"
	"github.com/ServiceWeaver/weaver/runtime/logging"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"golang.org/x/exp/maps"
	"golang.org/x/sync/errgroup"
)

// An Op[T] is a randomized operation performed as part of a simulation.
type Op[T any] struct {
	// The name of the operation.
	Name string

	// A function that generates a "random" instance of type T. Gen should be
	// deterministic. That is, it should return the same value given the same
	// *rand.Rand.
	Gen func(*rand.Rand) T

	// The body of the operation. Func should be a function of the following
	// form:
	//
	//     func(context.Context, T, [Component]...) error
	//
	// Func's first argument is context.Context. It's second argument is T. The
	// remaining arguments must be registered component interface types. Func
	// is called on instances of T generated by Gen.
	//
	// A simulation executes a number of ops. The simulation fails if any op
	// returns a non-nil error.
	Func any
}

// Options configures a simulator.
type Options struct {
	Seed           int64                // the simulator's seed
	NumReplicas    int                  // the number of replicas of every component
	NumOps         int                  // the number of ops to run
	ConfigFilename string               // TOML config filename
	Config         string               // TOML config contents
	Fakes          map[reflect.Type]any // fake component implementations
}

// Simulator deterministically simulates a Service Weaver application.
type Simulator struct {
	opts       Options                                // simulator options
	config     *protos.AppConfig                      // application config
	regs       []*codegen.Registration                // registered components
	regsByIntf map[reflect.Type]*codegen.Registration // regs, by component interface
	components map[string][]any                       // component replicas
	ops        map[string]op                          // registered ops

	ctx   context.Context // simulation context
	group *errgroup.Group // group with all running goroutines

	mu          sync.Mutex // guards the following fields
	rand        *rand.Rand // random number generator
	numOps      int        // number of spawned ops
	calls       []*call    // pending calls
	replies     []*reply   // pending replies
	history     []Event    // history of events
	nextTraceID int        // next trace id
	nextSpanID  int        // next span id
}

// An Event represents an atomic step of a simulation.
type Event interface {
	isEvent()
}

// OpStart represents the start of an op.
type OpStart struct {
	TraceID int      // trace id
	SpanID  int      // span id
	Name    string   // op name
	Args    []string // op arguments
}

// OpFinish represents the finish of an op.
type OpFinish struct {
	TraceID int    // trace id
	SpanID  int    // span id
	Error   string // returned error message
}

// Call represents a component method call.
type Call struct {
	TraceID   int      // trace id
	SpanID    int      // span id
	Caller    string   // calling component (or "op")
	Replica   int      // calling component replica (or op number)
	Component string   // component being called
	Method    string   // method being called
	Args      []string // method arguments
}

// DeliverCall represents a component method call being delivered.
type DeliverCall struct {
	TraceID   int    // trace id
	SpanID    int    // span id
	Component string // component being called
	Replica   int    // component replica being called
}

// Return represents a component method call returning.
type Return struct {
	TraceID   int      // trace id
	SpanID    int      // span id
	Component string   // component returning
	Replica   int      // component replica returning
	Returns   []string // return values
}

// DeliverReturn represents the delivery of a method return.
type DeliverReturn struct {
	TraceID int // trace id
	SpanID  int // span id
}

// DeliverError represents the injection of an error.
type DeliverError struct {
	TraceID int // trace id
	SpanID  int // span id
}

func (OpStart) isEvent()       {}
func (OpFinish) isEvent()      {}
func (Call) isEvent()          {}
func (DeliverCall) isEvent()   {}
func (Return) isEvent()        {}
func (DeliverReturn) isEvent() {}
func (DeliverError) isEvent()  {}

var _ Event = OpStart{}
var _ Event = OpFinish{}
var _ Event = Call{}
var _ Event = DeliverCall{}
var _ Event = Return{}
var _ Event = DeliverReturn{}
var _ Event = DeliverError{}

// Results are the results of running a simulation.
type Results struct {
	Err     error   // first non-nil error returned by an op
	History []Event // a history of all simulation events
}

// op is a non-generic Op[T].
type op struct {
	t          reflect.Type   // the T in Op[T]
	name       string         // Op.Name
	gen        reflect.Value  // Op.Gen
	f          reflect.Value  // Op.Func
	components []reflect.Type // Op.Func component argument types
}

// call is a pending method call.
type call struct {
	traceID   int
	spanID    int
	component reflect.Type    // the component being called
	method    string          // the method being called
	args      []reflect.Value // the call's arguments
	reply     chan *reply     // a channel to receive the call's reply
}

// reply is a pending method reply.
type reply struct {
	call    *call           // the corresponding call
	returns []reflect.Value // the call's return values
}

// We store trace and span ids in the context using the following keys.
type traceIDKey struct{}
type spanIDKey struct{}

// withIDs returns a context embedded with the provided trace and span id.
func withIDs(ctx context.Context, traceID, spanID int) context.Context {
	ctx = context.WithValue(ctx, traceIDKey{}, traceID)
	return context.WithValue(ctx, spanIDKey{}, spanID)
}

// extractIDs returns the trace and span id embedded in the provided context.
// If the provided context does not have embedded trace and span ids,
// extractIDs returns 0, 0.
func extractIDs(ctx context.Context) (int, int) {
	var traceID, spanID int
	if x := ctx.Value(traceIDKey{}); x != nil {
		traceID = x.(int)
	}
	if x := ctx.Value(spanIDKey{}); x != nil {
		spanID = x.(int)
	}
	return traceID, spanID
}

// New returns a new Simulator.
func New(opts Options) (*Simulator, error) {
	// Validate options.
	//
	// TODO(mwhittaker): In the final simulator API, we will pick a number of
	// replicas and number of operations for the user.
	if opts.NumReplicas <= 0 {
		return nil, fmt.Errorf("sim.New: NumReplicas (%d) <= 0", opts.NumReplicas)
	}
	if opts.NumOps <= 0 {
		return nil, fmt.Errorf("sim.New: NumOps (%d) <= 0", opts.NumOps)
	}

	// Index registrations.
	regs := codegen.Registered()
	regsByIntf := map[reflect.Type]*codegen.Registration{}
	for _, reg := range regs {
		regsByIntf[reg.Iface] = reg
	}

	// Parse config.
	app := &protos.AppConfig{}
	if opts.Config != "" {
		var err error
		app, err = runtime.ParseConfig(opts.ConfigFilename, opts.Config, codegen.ComponentConfigValidator)
		if err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	// Create simulator.
	s := &Simulator{
		opts:       opts,
		config:     app,
		regs:       regs,
		regsByIntf: regsByIntf,
		components: map[string][]any{},
		ops:        map[string]op{},
		rand:       rand.New(rand.NewSource(opts.Seed)),

		// Start both trace and span ids at 1 to reserve 0 as an invalid id.
		nextTraceID: 1,
		nextSpanID:  1,
	}

	// Create component replicas.
	for _, reg := range regsByIntf {
		if fake, ok := opts.Fakes[reg.Iface]; ok {
			s.components[reg.Name] = []any{fake}
			continue
		}
		for i := 0; i < opts.NumReplicas; i++ {
			// Create the component implementation.
			v := reflect.New(reg.Impl)
			obj := v.Interface()

			// Fill config.
			if cfg := config.Config(v); cfg != nil {
				if err := runtime.ParseConfigSection(reg.Name, "", app.Sections, cfg); err != nil {
					return nil, err
				}
			}

			// Set logger.
			//
			// TODO(mwhittaker): Use custom logger.
			if err := weaver.SetLogger(obj, slog.Default()); err != nil {
				return nil, err
			}

			// Fill ref fields.
			if err := weaver.FillRefs(obj, func(t reflect.Type) (any, error) {
				return s.getIntf(t, reg.Name, i)
			}); err != nil {
				return nil, err
			}

			// Fill listener fields.
			if err := weaver.FillListeners(obj, func(name string) (net.Listener, string, error) {
				lis, err := net.Listen("tcp", ":0")
				return lis, "", err
			}); err != nil {
				return nil, err
			}

			// Call Init if available.
			if i, ok := obj.(interface{ Init(context.Context) error }); ok {
				// TODO(mwhittaker): Use better context.
				if err := i.Init(context.Background()); err != nil {
					return nil, fmt.Errorf("component %q initialization failed: %w", reg.Name, err)
				}
			}

			s.components[reg.Name] = append(s.components[reg.Name], obj)
		}
	}

	return s, nil
}

// getIntf returns a handle to the component of the provided type.
func (s *Simulator) getIntf(t reflect.Type, caller string, replica int) (any, error) {
	reg, ok := s.regsByIntf[t]
	if !ok {
		return nil, fmt.Errorf("component %v not found", t)
	}
	call := func(method string, ctx context.Context, args []any, returns []any) error {
		return s.call(caller, replica, reg, method, ctx, args, returns)
	}
	return reg.ReflectStubFn(call), nil
}

// call executes a component method call against a random replica.
func (s *Simulator) call(caller string, replica int, reg *codegen.Registration, method string, ctx context.Context, args []any, returns []any) error {
	// Convert the arguments to reflect.Values.
	in := make([]reflect.Value, 1+len(args))
	in[0] = reflect.ValueOf(ctx)
	strings := make([]string, len(args))
	for i, arg := range args {
		in[i+1] = reflect.ValueOf(arg)
		strings[i] = fmt.Sprint(arg)
	}

	// Record the call.
	reply := make(chan *reply, 1)
	s.mu.Lock()
	traceID, _ := extractIDs(ctx)
	spanID := s.nextSpanID
	s.nextSpanID++

	s.calls = append(s.calls, &call{
		traceID:   traceID,
		spanID:    spanID,
		component: reg.Iface,
		method:    method,
		args:      in,
		reply:     reply,
	})

	s.history = append(s.history, Call{
		TraceID:   traceID,
		SpanID:    spanID,
		Caller:    caller,
		Replica:   replica,
		Component: reg.Name,
		Method:    method,
		Args:      strings,
	})
	s.mu.Unlock()

	// Take a step and wait for the call to finish.
	s.step()
	var out []reflect.Value
	select {
	case r := <-reply:
		out = r.returns
	case <-s.ctx.Done():
		return s.ctx.Err()
	}

	// Populate return values.
	if len(returns) != len(out)-1 {
		panic(fmt.Errorf("invalid number of returns: want %d, got %d", len(out)-1, len(returns)))
	}
	for i := 0; i < len(returns); i++ {
		// Note that returns[i] has static type any but dynamic type *T for
		// some T. out[i] has dynamic type T.
		reflect.ValueOf(returns[i]).Elem().Set(out[i])
	}
	if x := out[len(out)-1].Interface(); x != nil {
		return x.(error)
	}
	return nil
}

// RegisterOp registers an operation with the provided simulator. RegisterOp
// panics if the provided op is invalid.
func RegisterOp[T any](s *Simulator, o Op[T]) {
	op, err := validateOp(s, o)
	if err != nil {
		panic(err)
	}
	s.ops[op.name] = op
}

// validateOp validates the provided Op[T] and converts it to an op.
func validateOp[T any](s *Simulator, o Op[T]) (op, error) {
	if _, ok := s.ops[o.Name]; ok {
		return op{}, fmt.Errorf("duplicate registration of op %q", o.Name)
	}

	// TODO(mwhittaker): Improve error messages.
	if o.Name == "" {
		return op{}, fmt.Errorf("missing op Name")
	}
	if o.Gen == nil {
		return op{}, fmt.Errorf("op %q has nil Gen", o.Name)
	}
	if o.Func == nil {
		return op{}, fmt.Errorf("op %q has nil Func", o.Name)
	}
	t := reflect.TypeOf(o.Func)
	if t.Kind() != reflect.Func {
		return op{}, fmt.Errorf("op %q func is not a function: %T", o.Name, o.Func)
	}
	if t.NumIn() < 2 {
		return op{}, fmt.Errorf("op %q func has < 2 arguments: %T", o.Name, o.Func)
	}
	if t.In(0) != reflection.Type[context.Context]() {
		return op{}, fmt.Errorf("op %q func's first argument is not context.Context: %T", o.Name, o.Func)
	}
	if t.In(1) != reflection.Type[T]() {
		return op{}, fmt.Errorf("op %q func's second argument is not %v: %T", o.Name, reflection.Type[T](), o.Func)
	}
	var components []reflect.Type
	for i := 2; i < t.NumIn(); i++ {
		if _, ok := s.regsByIntf[t.In(i)]; !ok {
			return op{}, fmt.Errorf("op %q func argument %d is not a registered component: %T", o.Name, i, o.Func)
		}
		components = append(components, t.In(i))
	}
	if t.NumOut() != 1 {
		return op{}, fmt.Errorf("op %q func does not have exactly one return: %T", o.Name, o.Func)
	}
	if t.Out(0) != reflection.Type[error]() {
		return op{}, fmt.Errorf("op %q func does not return an error: %T", o.Name, o.Func)
	}

	return op{
		t:          reflection.Type[T](),
		name:       o.Name,
		gen:        reflect.ValueOf(o.Gen),
		f:          reflect.ValueOf(o.Func),
		components: components,
	}, nil
}

// Simulate executes a single simulation. Simulate returns an error if the
// simulation fails to execute properly. If the simulation executes properly
// and successfuly finds an invariant violation, no error is returned, but the
// invariant violation is reported as an error in the returned Results.
func (s *Simulator) Simulate(ctx context.Context) (*Results, error) {
	s.group, s.ctx = errgroup.WithContext(ctx)
	s.step()
	// TODO(mwhittaker): Distinguish between cancelled context and failed
	// execution.
	err := s.group.Wait()
	return &Results{Err: err, History: s.history}, nil
}

// step performs one step of a simulation.
func (s *Simulator) step() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ctx.Err() != nil {
		// The simulation has been cancelled.
		return
	}

	// Compute the set of candidate steps.
	const (
		runOp = iota
		deliverCall
		deliverReply
		deliverCallError
		deliverReplyError
	)
	var candidates []int
	if s.numOps < s.opts.NumOps {
		candidates = append(candidates, runOp)
	}
	if len(s.calls) > 0 {
		candidates = append(candidates, deliverCall, deliverCallError)
	}
	if len(s.replies) > 0 {
		candidates = append(candidates, deliverReply, deliverReplyError)
	}
	if len(candidates) == 0 {
		return
	}

	// Randomly execute a step.
	switch x := candidates[s.rand.Intn(len(candidates))]; x {
	case runOp:
		s.numOps++
		// TODO(mwhittaker): Store ops in a slice instead of a map so that
		// picking them is efficient.
		o := pickValue(s.rand, s.ops)
		s.group.Go(func() error {
			return s.runOp(s.ctx, o)
		})

	case deliverCall:
		var call *call
		call, s.calls = pop(s.rand, s.calls)
		s.group.Go(func() error {
			s.deliverCall(call)
			return nil
		})

	case deliverReply:
		var reply *reply
		reply, s.replies = pop(s.rand, s.replies)
		s.history = append(s.history, DeliverReturn{
			TraceID: reply.call.traceID,
			SpanID:  reply.call.spanID,
		})
		reply.call.reply <- reply
		close(reply.call.reply)

	case deliverCallError:
		// TODO(mwhittaker): Implement co-location. Don't return
		// RemoteCallErrors between co-located components.
		var call *call
		call, s.calls = pop(s.rand, s.calls)
		s.history = append(s.history, DeliverError{
			TraceID: call.traceID,
			SpanID:  call.spanID,
		})
		call.reply <- &reply{
			call:    call,
			returns: returnError(call.component, call.method, core.RemoteCallError),
		}
		close(call.reply)

	case deliverReplyError:
		// TODO(mwhittaker): Implement co-location. Don't return
		// RemoteCallErrors between co-located components.
		var reply *reply
		reply, s.replies = pop(s.rand, s.replies)
		s.history = append(s.history, DeliverError{
			TraceID: reply.call.traceID,
			SpanID:  reply.call.spanID,
		})
		reply.returns = returnError(reply.call.component, reply.call.method, core.RemoteCallError)
		reply.call.reply <- reply
		close(reply.call.reply)

	default:
		panic(fmt.Errorf("unrecognized candidate %v", x))
	}
}

// runOp runs the provided operation.
func (s *Simulator) runOp(ctx context.Context, o op) error {
	// Call the op's Gen function to generate a random value. Lock s.mu because
	// s.rand is not safe for concurrent use by multiple goroutines.
	s.mu.Lock()
	val := o.gen.Call([]reflect.Value{reflect.ValueOf(s.rand)})[0]

	// Record an OpStart event.
	traceID, spanID := s.nextTraceID, s.nextSpanID
	s.nextTraceID++
	s.nextSpanID++
	s.history = append(s.history, OpStart{
		TraceID: traceID,
		SpanID:  spanID,
		Name:    o.name,
		Args:    []string{fmt.Sprint(val.Interface())},
	})
	s.mu.Unlock()

	// Construct arguments for func(context.Context, T, [Component]...).
	args := make([]reflect.Value, 2+len(o.components))
	args[0] = reflect.ValueOf(withIDs(ctx, traceID, spanID))
	args[1] = val
	for i, component := range o.components {
		c, err := s.getIntf(component, "op", traceID)
		if err != nil {
			return err
		}
		args[2+i] = reflect.ValueOf(c)
	}

	// Invoke the op.
	var err error
	if x := o.f.Call(args)[0].Interface(); x != nil {
		err = x.(error)
	}

	// Record an OpFinish event.
	msg := "<nil>"
	if err != nil {
		msg = err.Error()
	}
	s.mu.Lock()
	s.history = append(s.history, OpFinish{
		TraceID: traceID,
		SpanID:  spanID,
		Error:   msg,
	})
	s.mu.Unlock()

	if err != nil {
		// If an op returns a non-nil error, abort the simulation and don't
		// take another step. Because the runOp function is running inside an
		// errgroup.Group, the returned error will cancel all goroutines.
		return err
	}

	// If the op succeeded, take another step.
	s.step()
	return nil
}

// deliverCall delivers the provided pending method call.
func (s *Simulator) deliverCall(call *call) {
	reg, ok := s.regsByIntf[call.component]
	if !ok {
		panic(fmt.Errorf("component %v not found", call.component))
	}

	// Pick a replica to execute the call.
	s.mu.Lock()
	index := s.rand.Intn(len(s.components[reg.Name]))
	replica := s.components[reg.Name][index]

	// Record a DeliverCall event.
	s.history = append(s.history, DeliverCall{
		TraceID:   call.traceID,
		SpanID:    call.spanID,
		Component: reg.Name,
		Replica:   index,
	})
	s.mu.Unlock()

	// Call the component method.
	returns := reflect.ValueOf(replica).MethodByName(call.method).Call(call.args)
	strings := make([]string, len(returns))
	for i, ret := range returns {
		strings[i] = fmt.Sprint(ret.Interface())
	}

	// Record the reply and take a step.
	s.mu.Lock()
	s.replies = append(s.replies, &reply{
		call:    call,
		returns: returns,
	})

	s.history = append(s.history, Return{
		TraceID:   call.traceID,
		SpanID:    call.spanID,
		Component: reg.Name,
		Replica:   index,
		Returns:   strings,
	})
	s.mu.Unlock()
	s.step()
}

// returnError returns a slice of reflect.Values compatible with the return
// type of the provided method. The final return value is the provided error.
// All other return values are zero initialized.
func returnError(component reflect.Type, method string, err error) []reflect.Value {
	m, ok := component.MethodByName(method)
	if !ok {
		panic(fmt.Errorf("method %s.%s not found", component, method))
	}
	t := m.Type
	n := t.NumOut()
	returns := make([]reflect.Value, n)
	for i := 0; i < n-1; i++ {
		returns[i] = reflect.Zero(t.Out(i))
	}
	returns[n-1] = reflect.ValueOf(err)
	return returns
}

// Mermaid returns a [mermaid][1] diagram that illustrates a simulation
// history.
//
// TODO(mwhittaker): Arrange replicas in topological order.
//
// [1]: https://mermaid.js.org/
func (r *Results) Mermaid() string {
	// Some abbreviations to save typing.
	shorten := logging.ShortenComponent
	commas := func(xs []string) string { return strings.Join(xs, ", ") }

	// Gather the set of all ops and replicas.
	type replica struct {
		component string
		replica   int
	}
	var ops []OpStart
	replicas := map[replica]struct{}{}
	calls := map[int]Call{}
	returns := map[int]Return{}
	for _, event := range r.History {
		switch x := event.(type) {
		case OpStart:
			ops = append(ops, x)
		case Call:
			calls[x.SpanID] = x
		case DeliverCall:
			call := calls[x.SpanID]
			replicas[replica{call.Component, x.Replica}] = struct{}{}
		case Return:
			returns[x.SpanID] = x
		}
	}

	// Create the diagram.
	var b strings.Builder
	fmt.Fprintln(&b, "sequenceDiagram")

	// Create ops.
	for _, op := range ops {
		fmt.Fprintf(&b, "    participant op%d as Op %d\n", op.TraceID, op.TraceID)
	}

	// Create component replicas.
	sorted := maps.Keys(replicas)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].component != sorted[j].component {
			return sorted[i].component < sorted[j].component
		}
		return sorted[i].replica < sorted[j].replica
	})
	for _, replica := range sorted {
		fmt.Fprintf(&b, "    participant %s%d as %s %d\n", replica.component, replica.replica, shorten(replica.component), replica.replica)
	}

	// Create events.
	for _, event := range r.History {
		switch x := event.(type) {
		case OpStart:
			fmt.Fprintf(&b, "    note right of op%d: [%d:%d] %s(%s)\n", x.TraceID, x.TraceID, x.SpanID, x.Name, commas(x.Args))
		case OpFinish:
			fmt.Fprintf(&b, "    note right of op%d: [%d:%d] return %s\n", x.TraceID, x.TraceID, x.SpanID, x.Error)
		case DeliverCall:
			call := calls[x.SpanID]
			fmt.Fprintf(&b, "    %s%d->>%s%d: [%d:%d] %s.%s(%s)\n", call.Caller, call.Replica, call.Component, x.Replica, x.TraceID, x.SpanID, shorten(call.Component), call.Method, commas(call.Args))
		case DeliverReturn:
			call := calls[x.SpanID]
			ret := returns[x.SpanID]
			fmt.Fprintf(&b, "    %s%d->>%s%d: [%d:%d] return %s\n", ret.Component, ret.Replica, call.Caller, call.Replica, x.TraceID, x.SpanID, commas(ret.Returns))
		case DeliverError:
			call := calls[x.SpanID]
			fmt.Fprintf(&b, "    note right of %s%d: [%d:%d] RemoteCallError\n", call.Caller, call.Replica, x.TraceID, x.SpanID)
		}
	}
	return b.String()
}
