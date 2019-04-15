package dht

import (
	"context"
	"sync"

	pb "github.com/libp2p/go-libp2p-kad-dht/pb"
	"go.opencensus.io/trace"
	"golang.org/x/xerrors"
)

type peerStreamPool struct {
	newStream func(context.Context) (*stream, error)

	mu      sync.Mutex
	streams map[*stream]struct{}
	waiters map[*streamWaiter]struct{}
	pending int
	sendMu  sync.Mutex
}

type streamWaiter struct {
	s    *stream
	err  error
	done bool
	ret  sync.Mutex
}

func (me *peerStreamPool) getStream(ctx context.Context) (*stream, error) {
	ctx, span := trace.StartSpan(ctx, "get stream")
	defer span.End()
	me.mu.Lock()
	for s := range me.streams {
		delete(me.streams, s)
		me.mu.Unlock()
		return s, nil
	}
	w := &streamWaiter{}
	w.ret.Lock()
	me.waiters[w] = struct{}{}
	if me.pending < len(me.waiters) {
		me.pending++
		go me.addStream()
	}
	if me.pending < len(me.waiters) {
		panic("not enough pending streams")
	}
	me.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		me.mu.Lock()
		me.resolveWaiter(w, nil, ctx.Err())
		me.mu.Unlock()
	}()
	w.ret.Lock()
	return w.s, w.err
}

func (me *peerStreamPool) addStream() {
	s, err := me.newStream(context.Background())
	me.mu.Lock()
	defer me.mu.Unlock()
	me.pending--
	if me.pending < 0 {
		panic("negative pending")
	}
	me.putStreamLocked(s, err)
}

func (me *peerStreamPool) putStream(s *stream, err error) {
	me.mu.Lock()
	defer me.mu.Unlock()
	me.putStreamLocked(s, err)
}

func (me *peerStreamPool) putStreamLocked(s *stream, err error) {
	if err != nil && me.pending >= len(me.waiters) {
		return
	}
	for w := range me.waiters {
		if !me.resolveWaiter(w, s, err) {
			panic("waiter already done but still present")
		}
		return
	}
	if err != nil {
		return
	}
	me.streams[s] = struct{}{}
}

func (me *peerStreamPool) delete(s *stream) {
	me.mu.Lock()
	defer me.mu.Unlock()
	delete(me.streams, s)
}

func (me *peerStreamPool) empty() bool {
	return len(me.streams) == 0 && len(me.waiters) == 0
}

func (me *peerStreamPool) send(ctx context.Context, m *pb.Message) error {
	me.sendMu.Lock()
	defer me.sendMu.Unlock()
	s, err := me.getStream(ctx)
	if err != nil {
		return xerrors.Errorf("getting stream: %w", err)
	}
	err = s.send(m)
	if err == nil {
		me.putStream(s, nil)
	}
	return err
}

func (me *peerStreamPool) doRequest(ctx context.Context, req *pb.Message) (*pb.Message, error) {
	ctx, span := trace.StartSpan(ctx, "peer request")
	defer span.End()
	s, err := me.getStream(ctx)
	if err != nil {
		return nil, xerrors.Errorf("getting stream: %w", err)
	}
	type requestResult struct {
		*pb.Message
		error
	}
	rrCh := make(chan requestResult, 1)
	go func() {
		resp, err := s.request(ctx, req)
		rrCh <- requestResult{resp, err}
		me.putStream(s, err)
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case rr := <-rrCh:
		return rr.Message, rr.error
	}
}

func (me *peerStreamPool) resolveWaiter(w *streamWaiter, s *stream, err error) bool {
	if w.done {
		return false
	}
	delete(me.waiters, w)
	go func() {
		w.s = s
		w.err = err
		w.done = true
		w.ret.Unlock()
	}()
	return true
}
