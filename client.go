package jrpc2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"bitbucket.org/creachadair/jrpc2/channel"
	"bitbucket.org/creachadair/jrpc2/code"
)

// A Client is a JSON-RPC 2.0 client. The client sends requests and receives
// responses on a channel.Channel provided by the caller.
type Client struct {
	done chan struct{} // closed when the reader is done at shutdown time

	log    func(string, ...interface{}) // write debug logs here
	enctx  encoder
	snote  func(*jresponse) bool
	allow1 bool // tolerate v1 replies with no version marker
	allowC bool // send rpc.cancel when a request context ends

	mu      sync.Mutex           // protects the fields below
	ch      channel.Channel      // channel to the server
	err     error                // error from a previous operation
	pending map[string]*Response // requests pending completion, by ID
	nextID  int64                // next unused request ID
}

// NewClient returns a new client that communicates with the server via ch.
func NewClient(ch channel.Channel, opts *ClientOptions) *Client {
	c := &Client{
		done:   make(chan struct{}),
		log:    opts.logger(),
		allow1: opts.allowV1(),
		allowC: opts.allowCancel(),
		enctx:  opts.encodeContext(),
		snote:  opts.handleNotification(),

		// Lock-protected fields
		ch:      ch,
		pending: make(map[string]*Response),
		nextID:  1,

		// Note that we start the ID counter at 1 here to avoid issues with a
		// server implementation that treats 0 as equivalent to null.
	}

	// The main client loop reads responses from the server and delivers them
	// back to pending requests by their ID. Outbound requests do not queue;
	// they are sent synchronously in the Send method.

	go func() {
		defer close(c.done)
		for c.accept(ch) == nil {
		}
	}()
	return c
}

// accept receives the next batch of responses from the server.  This may
// either be a list or a single object, the decoder for jresponses knows how to
// handle both. The caller must not hold c.mu.
func (c *Client) accept(ch channel.Receiver) error {
	var in jresponses
	bits, err := ch.Recv()
	if err == nil {
		err = json.Unmarshal(bits, &in)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		c.log("Decoding error: %v", err)
		c.stop(err)
		return err
	}

	c.log("Received %d responses", len(in))
	for _, rsp := range in {
		c.deliver(rsp)
	}
	return nil
}

// For each response, find the request pending on its ID and deliver it.  The
// caller must hold c.mu.  Unknown response IDs are logged and discarded.  As
// we are under the lock, we do not wait for the pending receiver to pick up
// the response; we just drop it in their channel.  The channel is buffered so
// we don't need to rendezvous.
func (c *Client) deliver(rsp *jresponse) {
	if id := string(fixID(rsp.ID)); id == "" {
		if !c.snote(rsp) {
			c.log("Discarding response without ID: %v", rsp)
		}
	} else if p := c.pending[id]; p == nil {
		c.log("Discarding response for unknown ID %q", id)
	} else if !c.versionOK(rsp.V) {
		delete(c.pending, id)
		p.ch <- &jresponse{
			ID: rsp.ID,
			E:  jerrorf(code.InvalidRequest, "incorrect version marker %q", rsp.V),
		}
		c.log("Invalid response for ID %q", id)
	} else {
		// Remove the pending request from the set and deliver its response.
		// Determining whether it's an error is the caller's responsibility.
		delete(c.pending, id)
		p.ch <- rsp
		c.log("Completed request for ID %q", id)
	}
}

// req constructs a fresh request for the specified method and parameters.
// This does not transmit the request to the server; use c.send to do so.
func (c *Client) req(ctx context.Context, method string, params interface{}) (*jrequest, error) {
	bits, err := c.marshalParams(ctx, method, params)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	id := json.RawMessage(strconv.FormatInt(c.nextID, 10))
	c.nextID++
	return &jrequest{
		V:  Version,
		ID: id,
		M:  method,
		P:  bits,
	}, nil
}

// note constructs a notification request for the specified method and parameters.
func (c *Client) note(ctx context.Context, method string, params interface{}) (*jrequest, error) {
	bits, err := c.marshalParams(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return &jrequest{V: Version, M: method, P: bits}, nil
}

// send transmits the specified requests to the server and returns a slice of
// pending responses awaiting a reply from the server.
//
// The resulting slice will contain one entry for each input request that
// expects a response (that is, all those that are not notifications). If all
// the requests are notifications, the slice will be empty.
//
// This method blocks until the entire batch of requests has been transmitted.
func (c *Client) send(ctx context.Context, reqs jrequests) ([]*Response, error) {
	if len(reqs) == 0 {
		return nil, errors.New("empty request batch")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}

	// Verify that there are no duplicate request IDs in this batch.  If there
	// are, flag it now before we waste time encoding.
	for _, req := range reqs {
		if id := string(req.ID); id != "" && c.pending[id] != nil {
			return nil, fmt.Errorf("duplicate request ID %q", id)
		}
	}

	b, err := json.Marshal(reqs)
	if err != nil {
		return nil, fmt.Errorf("marshaling request failed: %v", err)
	}
	c.log("Outgoing batch: %s", string(b))
	if err := c.ch.Send(b); err != nil {
		return nil, err
	}

	// Now that we have sent them, record pending requests for each that is not
	// a notification. We do this after transmission so that an error does not
	// leave us with dead pending requests awaiting responses.
	var pends []*Response
	for _, req := range reqs {
		if id := string(req.ID); id != "" {
			pctx, p := newPending(ctx, id)
			c.pending[id] = p
			pends = append(pends, p)
			go c.waitComplete(pctx, id, p)
		}
	}
	return pends, nil
}

// waitComplete waits for completion of the context governing p. When the
// context ends, check whether the request is still in the pending set for the
// client: If so, a reply has not yet been delivered.  Otherwise, the
// cancellation is a no-op ("too late").
func (c *Client) waitComplete(pctx context.Context, id string, p *Response) {
	<-pctx.Done()
	cleanup := func() {}
	c.mu.Lock()
	defer func() {
		c.mu.Unlock()
		cleanup() // N.B. outside the lock
	}()

	if _, ok := c.pending[id]; !ok {
		return
	}

	c.log("Context ended for id %q, err=%v", id, pctx.Err())
	delete(c.pending, id)
	code := code.FromError(pctx.Err())
	p.ch <- &jresponse{
		ID: json.RawMessage(id),
		E:  jerrorf(code, pctx.Err().Error()),
	}

	// Inform the server, best effort only. N.B. Use a background context here,
	// as the original context has ended by the time we get here.
	if c.allowC {
		cleanup = func() {
			c.log("Sending rpc.cancel for id %q to the server", id)
			c.Notify(context.Background(), "rpc.cancel", []json.RawMessage{json.RawMessage(id)})
		}
	}
}

// Call initiates a single request and blocks until the response returns.  If
// err != nil then rsp == nil, which also means that if rsp != nil then the
// request succeeded. Errors from the server have concrete type *jrpc2.Error.
//
//    rsp, err := c.Call(ctx, method, params)
//    if e, ok := err.(*jrpc2.Error); ok {
//       log.Fatalf("Error from server: %v", err)
//    } else if err != nil {
//       log.Fatalf("Call failed: %v", err)
//    }
//    handleValidResponse(rsp)
//
func (c *Client) Call(ctx context.Context, method string, params interface{}) (*Response, error) {
	req, err := c.req(ctx, method, params)
	if err != nil {
		return nil, err
	}
	rsp, err := c.send(ctx, jrequests{req})
	if err != nil {
		return nil, err
	}
	rsp[0].wait()
	if err := rsp[0].Error(); err != nil {
		switch err.code {
		case code.Cancelled:
			return nil, context.Canceled
		case code.DeadlineExceeded:
			return nil, context.DeadlineExceeded
		default:
			return nil, err
		}
	}
	return rsp[0], nil
}

// CallResult invokes Call with the given method and params. If it succeeds,
// the result is decoded into result. This is a convenient shorthand for Call
// followed by UnmarshalResult. It will panic if result == nil.
func (c *Client) CallResult(ctx context.Context, method string, params, result interface{}) error {
	rsp, err := c.Call(ctx, method, params)
	if err != nil {
		return err
	}
	return rsp.UnmarshalResult(result)
}

// Batch initiates a batch of concurrent requests, and blocks until all the
// responses return. The responses are returned in the same order as the
// original specs, omitting notifications.
//
// Any error returned is from sending the batch; the caller must check each
// response for errors from the server.
func (c *Client) Batch(ctx context.Context, specs []Spec) ([]*Response, error) {
	reqs := make(jrequests, len(specs))
	for i, spec := range specs {
		if spec.Notify {
			req, err := c.note(ctx, spec.Method, spec.Params)
			if err != nil {
				return nil, err
			}
			reqs[i] = req
		} else if req, err := c.req(ctx, spec.Method, spec.Params); err != nil {
			return nil, err
		} else {
			reqs[i] = req
		}
	}
	rsps, err := c.send(ctx, reqs)
	if err != nil {
		return nil, err
	}
	for _, rsp := range rsps {
		rsp.wait()
	}
	return rsps, nil
}

// A Spec combines a method name and parameter value. If the Notify field is
// true, the spec is sent as a notification instead of a request.
type Spec struct {
	Method string
	Params interface{}
	Notify bool
}

// Notify transmits a notification to the specified method and parameters.  It
// blocks until the notification has been sent.
func (c *Client) Notify(ctx context.Context, method string, params interface{}) error {
	req, err := c.note(ctx, method, params)
	if err != nil {
		return err
	}
	_, err = c.send(ctx, jrequests{req})
	return err
}

// Close shuts down the client, abandoning any pending in-flight requests.
func (c *Client) Close() error {
	c.mu.Lock()
	c.stop(errClientStopped)
	c.mu.Unlock()
	<-c.done
	// Dont' remark on a closed channel or EOF as a noteworthy failure.
	if c.err == io.EOF || channel.IsErrClosing(c.err) || c.err == errClientStopped {
		return nil
	}
	return c.err
}

// stop closes down the reader for c and records err as its final state.  The
// caller must hold c.mu. If multiple callers invoke stop, only the first will
// successfully record its error status.
func (c *Client) stop(err error) {
	if c.ch == nil {
		return // nothing is running
	}
	c.ch.Close()
	for _, p := range c.pending {
		p.cancel()
	}
	c.err = err
	c.ch = nil
}

func (c *Client) versionOK(v string) bool {
	if v == "" {
		return c.allow1
	}
	return v == Version
}

// marshalParams validates and marshals params to JSON for a request.  The
// value of params must be either nil or encodable as a JSON object or array.
func (c *Client) marshalParams(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if params == nil {
		return c.enctx(ctx, method, nil) // no parameters, that is OK
	}
	pbits, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if len(pbits) == 0 || (pbits[0] != '[' && pbits[0] != '{' && string(pbits) != "null") {
		// JSON-RPC requires that if parameters are provided at all, they are
		// an array or an object.
		return nil, Errorf(code.InvalidRequest, "invalid parameters: array or object required")
	}
	bits, err := c.enctx(ctx, method, pbits)
	if err != nil {
		return nil, err
	}
	return bits, err
}

func newPending(ctx context.Context, id string) (context.Context, *Response) {
	// Buffer the channel so the response reader does not need to rendezvous
	// with the recipient.
	pctx, cancel := context.WithCancel(ctx)
	return pctx, &Response{
		ch:     make(chan *jresponse, 1),
		id:     id,
		cancel: cancel,
	}
}
