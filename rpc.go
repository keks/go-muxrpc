package muxrpc // import "cryptoscope.co/go/muxrpc"

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/pkg/errors"

	"cryptoscope.co/go/luigi"
	"cryptoscope.co/go/muxrpc/codec"
)

// rpc implements an Endpoint, but has some more methods like Serve
type rpc struct {

	// pkr is the Sink and Source of the network connection
	pkr Packer

	// reqs is the map we keep, tracking all requests
	reqs  map[int32]*Request
	rLock sync.Mutex

	// highest is the highest request id we already allocated
	highest int32

	root Handler

	// terminated indicates that the rpc session is being terminated
	terminated bool
	tLock      sync.Mutex
}

// Handler allows handling connections.
// When a connection is established, HandleConnect is called.
// When we are being called, HandleCall is called.
type Handler interface {
	HandleCall(ctx context.Context, req *Request)
	HandleConnect(ctx context.Context, e Endpoint)
}

const bufSize = 5
const rxTimeout time.Duration = time.Millisecond

// Handle handles the connection of the packer using the specified handler.
func Handle(pkr Packer, handler Handler) Endpoint {
	r := &rpc{
		pkr:  pkr,
		reqs: make(map[int32]*Request),
		root: handler,
	}

	go handler.HandleConnect(context.Background(), r)
	return r
}

// Async does an aync call on the remote.
func (r *rpc) Async(ctx context.Context, tipe interface{}, method []string, args ...interface{}) (interface{}, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	req := &Request{
		Type:   "async",
		Stream: NewStream(inSrc, r.pkr, 0, false, false),
		in:     inSink,

		Method: method,
		Args:   args,

		tipe: tipe,
	}

	err := r.Do(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}

	v, err := req.Stream.Next(ctx)
	return v, errors.Wrap(err, "error reading response from request source")
}

// Source does a source call on the remote.
func (r *rpc) Source(ctx context.Context, tipe interface{}, method []string, args ...interface{}) (luigi.Source, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	req := &Request{
		Type:   "source",
		Stream: NewStream(inSrc, r.pkr, 0, true, false),
		in:     inSink,

		Method: method,
		Args:   args,

		tipe: tipe,
	}

	err := r.Do(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}

	return req.Stream, nil
}

// Sink does a sink call on the remote.
func (r *rpc) Sink(ctx context.Context, method []string, args ...interface{}) (luigi.Sink, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	req := &Request{
		Type:   "sink",
		Stream: NewStream(inSrc, r.pkr, 0, false, true),
		in:     inSink,

		Method: method,
		Args:   args,
	}

	err := r.Do(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}

	return req.Stream, nil
}

// Duplex does a duplex call on the remote.
func (r *rpc) Duplex(ctx context.Context, tipe interface{}, method []string, args ...interface{}) (luigi.Source, luigi.Sink, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	req := &Request{
		Type:   "duplex",
		Stream: NewStream(inSrc, r.pkr, 0, true, true),
		in:     inSink,

		Method: method,
		Args:   args,

		tipe: tipe,
	}

	err := r.Do(ctx, req)
	if err != nil {
		return nil, nil, errors.Wrap(err, "error sending request")
	}

	return req.Stream, req.Stream, nil
}

// Terminate ends the RPC session
func (r *rpc) Terminate() error {
	r.tLock.Lock()
	defer r.tLock.Unlock()

	r.terminated = true
	return r.pkr.Close()
}

func (r *rpc) finish(ctx context.Context, req int32) error {
	delete(r.reqs, req)

	err := r.pkr.Pour(ctx, newEndOkayPacket(req))
	return errors.Wrap(err, "error pouring done message")
}

// Do executes a generic call
func (r *rpc) Do(ctx context.Context, req *Request) error {
	var (
		pkt codec.Packet
		err error
	)

	if req.Args == nil {
		req.Args = []interface{}{}
	}

	func() {
		r.rLock.Lock()
		defer r.rLock.Unlock()

		pkt.Flag = pkt.Flag.Set(codec.FlagJSON)
		pkt.Flag = pkt.Flag.Set(req.Type.Flags())

		pkt.Body, err = json.Marshal(req)

		pkt.Req = r.highest + 1
		r.highest = pkt.Req
		r.reqs[pkt.Req] = req
		req.Stream.WithReq(pkt.Req)
		req.Stream.WithType(req.tipe)

		req.pkt = &pkt
	}()
	if err != nil {
		return err
	}

	return r.pkr.Pour(ctx, &pkt)
}

// ParseRequest parses the first packet of a stream and parses the contained request
func (r *rpc) ParseRequest(pkt *codec.Packet) (*Request, error) {
	var req Request

	if !pkt.Flag.Get(codec.FlagJSON) {
		return nil, errors.New("expected JSON flag")
	}

	if pkt.Req >= 0 {
		// request numbers should have been inverted by now
		return nil, errors.New("expected negative request id")
	}

	err := json.Unmarshal(pkt.Body, &req)
	if err != nil {
		return nil, errors.Wrap(err, "error decoding packet")
	}
	req.pkt = pkt

	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	var inStream, outStream bool
	if pkt.Flag.Get(codec.FlagStream) {
		switch req.Type {
		case "duplex":
			inStream, outStream = true, true
		case "source":
			inStream, outStream = false, true
		case "sink":
			inStream, outStream = true, false
		default:
			return nil, errors.Errorf("unhandled request type: %q", req.Type)
		}
	}
	req.Stream = NewStream(inSrc, r.pkr, pkt.Req, inStream, outStream)
	req.in = inSink

	return &req, nil
}

func isTrue(data []byte) bool {
	return len(data) == 4 &&
		data[0] == 't' &&
		data[1] == 'r' &&
		data[2] == 'u' &&
		data[3] == 'e'
}

// fetchRequest returns the request from the reqs map or, if it's not there yet, builds a new one.
func (r *rpc) fetchRequest(ctx context.Context, pkt *codec.Packet) (*Request, bool, error) {
	var err error

	r.rLock.Lock()
	defer r.rLock.Unlock()

	// get request from map, otherwise make new one
	req, ok := r.reqs[pkt.Req]
	if !ok {
		req, err = r.ParseRequest(pkt)
		if err != nil {
			return nil, false, errors.Wrap(err, "error parsing request")
		}
		r.reqs[pkt.Req] = req

		go r.root.HandleCall(ctx, req)
	}

	return req, !ok, nil
}

type Server interface {
	Serve(context.Context) error
}

// Serve handles the RPC session
func (r *rpc) Serve(ctx context.Context) (err error) {
	for {
		var vpkt interface{}

		// read next packet from connection
		doRet := func() bool {
			vpkt, err = r.pkr.Next(ctx)

			r.tLock.Lock()
			defer r.tLock.Unlock()

			if luigi.IsEOS(err) {
				err = nil
				return true
			}
			if err != nil {
				if r.terminated {
					err = nil
					return true
				}
				err = errors.Wrap(err, "error reading from packer source")
				return true
			}

			return false
		}()
		if doRet {
			return err
		}

		pkt := vpkt.(*codec.Packet)

		if pkt.Flag.Get(codec.FlagEndErr) {
			if req, ok := r.reqs[pkt.Req]; ok {
				err := func() error {
					r.rLock.Lock()
					defer r.rLock.Unlock()

					if isTrue(pkt.Body) {
						err = req.in.Close()
						if err != nil {
							return errors.Wrap(err, "error closing pipe sink")
						}

						err = req.Stream.Close()
						if err != nil {
							return errors.Wrap(err, "error closing stream")
						}
					} else {
						e, err := parseError(pkt.Body)
						if err != nil {
							return errors.Wrap(err, "error parsing error packet")
						}

						err = req.in.(luigi.ErrorCloser).CloseWithError(e)
						if err != nil {
							return errors.Wrap(err, "error closing pipe sink with error")
						}
					}

					delete(r.reqs, pkt.Req)
					return nil
				}()
				if err != nil {
					return err
				}

				continue
			}
		}

		req, isNew, err := r.fetchRequest(ctx, pkt)
		if err != nil {
			return errors.Wrap(err, "error getting request")
		}
		if isNew {
			continue
		}

		// localize defer
		err = func() error {
			// pour may block so we need to time out.
			// note that you can use buffers make this less probable
			ctx, cancel := context.WithTimeout(ctx, rxTimeout)
			defer cancel()

			//err := req.in.Pour(ctx, v)
			err := req.in.Pour(ctx, pkt)
			return errors.Wrap(err, "error pouring data to handler")
		}()

		if err != nil {
			return err
		}
	}
}

type CallError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Stack   string `json:"stack"`
}

func (e *CallError) Error() string {
	return e.Message
}

func parseError(data []byte) (*CallError, error) {
	var e CallError

	err := json.Unmarshal(data, &e)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshaling error packet")
	}

	if e.Name != "Error" {
		return nil, errors.New(`name is not "Error"`)
	}

	return &e, nil
}
