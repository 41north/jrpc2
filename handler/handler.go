// Package handler provides implementations of the jrpc2.Assigner interface,
// and support for adapting functions to the jrpc2.Handler interface.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/code"
)

// A Func adapts a function having the correct signature to a jrpc2.Handler.
type Func func(context.Context, *jrpc2.Request) (interface{}, error)

// Handle implements the jrpc2.Handler interface by calling m.
func (m Func) Handle(ctx context.Context, req *jrpc2.Request) (interface{}, error) {
	return m(ctx, req)
}

// A Map is a trivial implementation of the jrpc2.Assigner interface that looks
// up method names in a map of static jrpc2.Handler values.
type Map map[string]jrpc2.Handler

// Assign implements part of the jrpc2.Assigner interface.
func (m Map) Assign(_ context.Context, method string) jrpc2.Handler { return m[method] }

// Names implements part of the jrpc2.Assigner interface.
func (m Map) Names() []string {
	var names []string
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Assigner returns m itself as an assigner for use in a Service.
// It never reports an error.
func (m Map) Assigner() (jrpc2.Assigner, error) { return m, nil }

// Finish is a no-op implementation satisfying part of the Service interface.
func (Map) Finish(jrpc2.Assigner, jrpc2.ServerStatus) {}

// A ServiceMap combines multiple assigners into one, permitting a server to
// export multiple services under different names.
type ServiceMap map[string]jrpc2.Assigner

// Assign splits the inbound method name as Service.Method, and passes the
// Method portion to the corresponding Service assigner. If method does not
// have the form Service.Method, or if Service is not set in m, the lookup
// fails and returns nil.
func (m ServiceMap) Assign(ctx context.Context, method string) jrpc2.Handler {
	parts := strings.SplitN(method, ".", 2)
	if len(parts) == 1 {
		return nil
	} else if ass, ok := m[parts[0]]; ok {
		return ass.Assign(ctx, parts[1])
	}
	return nil
}

// Names reports the composed names of all the methods in the service, each
// having the form Service.Method.
func (m ServiceMap) Names() []string {
	var all []string
	for svc, assigner := range m {
		for _, name := range assigner.Names() {
			all = append(all, svc+"."+name)
		}
	}
	sort.Strings(all)
	return all
}

// Assigner returns m itself as an assigner for use in a Service.
// It never reports an error.
func (m ServiceMap) Assigner() (jrpc2.Assigner, error) { return m, nil }

// Finish is a no-op implementation satisfying part of the Service interface.
func (ServiceMap) Finish(jrpc2.Assigner, jrpc2.ServerStatus) {}

// New adapts a function to a jrpc2.Handler. The concrete value of fn must be a
// function with one of the following type signature schemes:
//
//    func(context.Context) error
//    func(context.Context) Y
//    func(context.Context) (Y, error)
//    func(context.Context, X) error
//    func(context.Context, X) Y
//    func(context.Context, X) (Y, error)
//    func(context.Context, ...X) error
//    func(context.Context, ...X) Y
//    func(context.Context, ...X) (Y, error)
//    func(context.Context, *jrpc2.Request) error
//    func(context.Context, *jrpc2.Request) Y
//    func(context.Context, *jrpc2.Request) (Y, error)
//    func(context.Context, *jrpc2.Request) (interface{}, error)
//
// for JSON-marshalable types X and Y. New will panic if the type of fn does
// not have one of these forms.  The resulting method will handle encoding and
// decoding of JSON and report appropriate errors.
//
// Functions adapted in this way can obtain the *jrpc2.Request value using the
// jrpc2.InboundRequest helper on the context value supplied by the server.
func New(fn interface{}) Func {
	m, err := newHandler(fn)
	if err != nil {
		panic(err)
	}
	return m
}

var (
	ctxType = reflect.TypeOf((*context.Context)(nil)).Elem() // type context.Context
	errType = reflect.TypeOf((*error)(nil)).Elem()           // type error
	reqType = reflect.TypeOf((*jrpc2.Request)(nil))          // type *jrpc2.Request
)

func newHandler(fn interface{}) (Func, error) {
	if fn == nil {
		return nil, errors.New("nil method")
	}

	// Special case: If fn has the exact signature of the Handle method, don't do
	// any (additional) reflection at all.
	if f, ok := fn.(func(context.Context, *jrpc2.Request) (interface{}, error)); ok {
		return Func(f), nil
	}

	// Check that fn is a function of one of the correct forms.
	info, err := checkFunctionType(fn)
	if err != nil {
		return nil, err
	}

	// Construct a function to unpack the parameters from the request message,
	// based on the signature of the user's callback.
	var newinput func(req *jrpc2.Request) ([]reflect.Value, error)

	if info.Argument == nil {
		// Case 1: The function does not want any request parameters.
		// Nothing needs to be decoded, but verify no parameters were passed.
		newinput = func(req *jrpc2.Request) ([]reflect.Value, error) {
			if req.HasParams() {
				return nil, jrpc2.Errorf(code.InvalidParams, "no parameters accepted")
			}
			return nil, nil
		}

	} else if info.Argument == reqType {
		// Case 2: The function wants the underlying *jrpc2.Request value.
		newinput = func(req *jrpc2.Request) ([]reflect.Value, error) {
			return []reflect.Value{reflect.ValueOf(req)}, nil
		}

	} else if info.Argument.Kind() == reflect.Ptr {
		// Case 3a: The function wants a pointer to its argument value.
		newinput = func(req *jrpc2.Request) ([]reflect.Value, error) {
			in := reflect.New(info.Argument)
			if err := req.UnmarshalParams(in.Interface()); err != nil {
				return nil, jrpc2.Errorf(code.InvalidParams, "invalid parameters: %v", err)
			}
			return []reflect.Value{in}, nil
		}
	} else {
		// Case 3b: The function wants a bare argument value.
		newinput = func(req *jrpc2.Request) ([]reflect.Value, error) {
			in := reflect.New(info.Argument) // we still need a pointer to unmarshal
			if err := req.UnmarshalParams(in.Interface()); err != nil {
				return nil, jrpc2.Errorf(code.InvalidParams, "invalid parameters: %v", err)
			}
			// Indirect the pointer back off for the callee.
			return []reflect.Value{in.Elem()}, nil
		}
	}

	// Construct a function to decode the result values.
	var decodeOut func([]reflect.Value) (interface{}, error)

	if info.Result == nil {
		// The function returns only an error, the result is always nil.
		decodeOut = func(vals []reflect.Value) (interface{}, error) {
			oerr := vals[0].Interface()
			if oerr != nil {
				return nil, oerr.(error)
			}
			return nil, nil
		}
	} else if !info.ReportsError {
		// The function returns only single non-error: err is always nil.
		decodeOut = func(vals []reflect.Value) (interface{}, error) {
			return vals[0].Interface(), nil
		}
	} else {
		// The function returns both a value and an error.
		decodeOut = func(vals []reflect.Value) (interface{}, error) {
			out, oerr := vals[0].Interface(), vals[1].Interface()
			if oerr != nil {
				return nil, oerr.(error)
			}
			return out, nil
		}
	}

	f := reflect.ValueOf(fn)
	call := f.Call
	if info.IsVariadic {
		call = f.CallSlice
	}

	return Func(func(ctx context.Context, req *jrpc2.Request) (interface{}, error) {
		rest, ierr := newinput(req)
		if ierr != nil {
			return nil, ierr
		}
		args := append([]reflect.Value{reflect.ValueOf(ctx)}, rest...)
		return decodeOut(call(args))
	}), nil
}

// funcInfo captures type signature information from a valid handler function.
type funcInfo struct {
	Type         reflect.Type // the complete function type
	Argument     reflect.Type // the non-context argument type, or nil
	IsVariadic   bool         // true if the argument exists and is variadic
	Result       reflect.Type // the non-error result type, or nil
	ReportsError bool         // true if the function reports an error
}

func checkFunctionType(fn interface{}) (*funcInfo, error) {
	info := &funcInfo{Type: reflect.TypeOf(fn)}
	if info.Type.Kind() != reflect.Func {
		return nil, errors.New("not a function")
	}
	if np := info.Type.NumIn(); np == 0 || np > 2 {
		return nil, errors.New("wrong number of parameters")
	} else if np == 2 {
		info.Argument = info.Type.In(1)
		info.IsVariadic = info.Type.IsVariadic()
	}
	no := info.Type.NumOut()
	if no < 1 || no > 2 {
		return nil, errors.New("wrong number of results")
	} else if info.Type.In(0) != ctxType {
		return nil, errors.New("first parameter is not context.Context")
	} else if no == 2 && info.Type.Out(1) != errType {
		return nil, errors.New("result is not of type error")
	}
	info.ReportsError = info.Type.Out(no-1) == errType
	if no == 2 || !info.ReportsError {
		info.Result = info.Type.Out(0)
	}
	return info, nil
}

// Args is a wrapper that decodes an array of positional parameters into
// concrete locations.
//
// Unmarshaling a JSON value into an Args value v succeeds if the JSON encodes
// an array with length len(v), and unmarshaling each subvalue i into the
// corresponding v[i] succeeds.  As a special case, if v[i] == nil the
// corresponding value is discarded.
//
// Marshaling an Args value v into JSON succeeds if each element of the slice
// is JSON marshalable, and yields a JSON array of length len(v) containing the
// JSON values corresponding to the elements of v.
//
// Usage example:
//
//    func Handler(ctx context.Context, req *jrpc2.Request) (interface{}, error) {
//       var x, y int
//       var s string
//
//       if err := req.UnmarshalParams(&handler.Args{&x, &y, &s}); err != nil {
//          return nil, err
//       }
//       // do useful work with x, y, and s
//    }
//
type Args []interface{}

// UnmarshalJSON supports JSON unmarshaling for a.
func (a Args) UnmarshalJSON(data []byte) error {
	var elts []json.RawMessage
	if err := json.Unmarshal(data, &elts); err != nil {
		return fmt.Errorf("decoding args: %w", err)
	} else if len(elts) != len(a) {
		return fmt.Errorf("wrong number of args (got %d, want %d)", len(elts), len(a))
	}
	for i, elt := range elts {
		if a[i] == nil {
			continue
		} else if err := json.Unmarshal(elt, a[i]); err != nil {
			return fmt.Errorf("decoding argument %d: %w", i+1, err)
		}
	}
	return nil
}

// MarshalJSON supports JSON marshaling for a.
func (a Args) MarshalJSON() ([]byte, error) {
	if len(a) == 0 {
		return []byte(`[]`), nil
	}
	return json.Marshal([]interface{}(a))
}

// Obj is a wrapper that maps object fields into concrete locations.
//
// Unmarshaling a JSON text into an Obj value v succeeds if the JSON encodes an
// object, and unmarshaling the value for each key k of the object into v[k]
// succeeds. If k does not exist in v, it is ignored.
//
// Marshaling an Obj into JSON works as for an ordinary map.
type Obj map[string]interface{}

// UnmarshalJSON supports JSON unmarshaling into o.
func (o Obj) UnmarshalJSON(data []byte) error {
	var base map[string]json.RawMessage
	if err := json.Unmarshal(data, &base); err != nil {
		return fmt.Errorf("decoding object: %v", err)
	}
	for key, val := range base {
		arg, ok := o[key]
		if !ok {
			continue
		} else if err := json.Unmarshal(val, arg); err != nil {
			return fmt.Errorf("decoding %q: %v", key, err)
		}
	}
	return nil
}
