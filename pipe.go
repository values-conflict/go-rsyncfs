package rsyncfs

import (
	"bytes"
	"io"
	"sync"
)

// defaultPipeSize is the per-direction capacity for [BufPipe]. It is comfortably larger than any handshake preamble (the greeting and the algorithm-negotiation lists are all well under a kilobyte), so the bound only ever acts as backpressure during a bulk transfer -- it never starves the handshake.
const defaultPipeSize = 32 * 1024

// BufPipe returns two connected, in-memory io.ReadWriteCloser ends, each backed by a bounded buffer.
//
// Unlike net.Pipe or io.Pipe -- which are zero-capacity and block a write until the peer reads -- each direction here buffers, so a writer can make progress without the reader simultaneously consuming. The rsync handshake depends on exactly that: both sides write their greeting before reading the peer's, and the vstring algorithm negotiation has both sides send before receiving. A real transport (TCP, Unix socket, etc) supplies that capacity via its kernel buffer; BufPipe supplies it in-process so a [Client] and a [Server] can be wired together with no network stack. The bound mirrors what those kernel buffers do -- apply backpressure when the receiver falls behind and cap memory -- rather than growing without limit.
func BufPipe() (a, b io.ReadWriteCloser) {
	a2b := newPipeBuf(defaultPipeSize)
	b2a := newPipeBuf(defaultPipeSize)
	return &pipeConn{in: b2a, out: a2b}, &pipeConn{in: a2b, out: b2a}
}

// pipeConn is one end of a [BufPipe]: writes go into the peer's buffer, reads come from it, and Close releases both directions.
type pipeConn struct {
	in  *pipeBuf
	out *pipeBuf
}

var _ io.ReadWriteCloser = (*pipeConn)(nil)

func (c *pipeConn) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error) { return c.out.Write(p) }

// Close marks both directions closed, unblocking any waiting reader (io.EOF) or writer (an error). It is safe to call multiple times.
func (c *pipeConn) Close() error {
	if err := c.out.Close(); err != nil {
		return err
	}
	return c.in.Close()
}

// pipeBuf is one direction of a [BufPipe]: a bounded FIFO with wait/close semantics. Write blocks while the buffer is full (backpressure) and Read blocks while it is empty.
type pipeBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	cap    int
	closed bool
}

func newPipeBuf(cap int) *pipeBuf {
	b := &pipeBuf{cap: cap}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *pipeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := 0
	for len(p) > 0 {
		// wait until at least one byte fits; a single Write larger than the
		// whole buffer drains in chunks as the reader consumes
		for b.buf.Len() >= b.cap && !b.closed {
			b.cond.Wait()
		}
		if b.closed {
			return total, io.ErrClosedPipe
		}
		space := b.cap - b.buf.Len()
		n := len(p)
		if n > space {
			n = space
		}
		// bytes.Buffer.Write never fails short or errors
		b.buf.Write(p[:n])
		total += n
		p = p[n:]
		b.cond.Broadcast()
	}
	return total, nil
}

func (b *pipeBuf) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 && !b.closed {
		b.cond.Wait()
	}
	if b.buf.Len() == 0 {
		return 0, io.EOF
	}
	n, _ := b.buf.Read(p)
	// signal any blocked writer that space has freed up
	b.cond.Broadcast()
	return n, nil
}

func (b *pipeBuf) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	b.cond.Broadcast()
	return nil
}
