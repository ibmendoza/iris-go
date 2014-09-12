// Copyright (c) 2013 Project Iris. All rights reserved.
//
// The current language binding is an official support library of the Iris
// cloud messaging framework, and as such, the same licensing terms apply.
// For details please see http://iris.karalabe.com/downloads#License

package iris

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/project-iris/iris/container/queue"
	"gopkg.in/inconshreveable/log15.v2"
)

// Communication stream between the local application and a remote endpoint. The
// ordered delivery of messages is guaranteed and the message flow between the
// peers is throttled.
type Tunnel struct {
	id   uint64      // Tunnel identifier for de/multiplexing
	conn *Connection // Connection to the local relay

	// Chunking fields
	chunkLimit int    // Maximum length of a data payload
	chunkBuf   []byte // Current message being assembled

	// Quality of service fields
	itoaBuf  *queue.Queue  // Iris to application message buffer
	itoaSign chan struct{} // Message arrival signaler
	itoaLock sync.Mutex    // Protects the buffer and signaler

	atoiSpace int           // Application to Iris space allowance
	atoiSign  chan struct{} // Allowance grant signaler
	atoiLock  sync.Mutex    // Protects the allowance and signaler

	// Bookkeeping fields
	init chan bool     // Initialization channel for outbound tunnels
	term chan struct{} // Channel to signal termination to blocked go-routines
	stat error         // Failure reason, if any received

	Log log15.Logger // Logger with connection and tunnel ids injected
}

func (c *Connection) newTunnel() (*Tunnel, error) {
	c.tunLock.Lock()
	defer c.tunLock.Unlock()

	// Make sure the connection is still up
	if c.tunLive == nil {
		return nil, ErrClosed
	}
	// Assign a new locally unique id to the tunnel
	tunId := c.tunIdx
	c.tunIdx++

	// Assemble and store the live tunnel
	tun := &Tunnel{
		id:   tunId,
		conn: c,

		itoaBuf:  queue.New(),
		itoaSign: make(chan struct{}, 1),
		atoiSign: make(chan struct{}, 1),

		init: make(chan bool),
		term: make(chan struct{}),

		Log: c.Log.New("tunnel", tunId),
	}
	c.tunLive[tunId] = tun

	return tun, nil
}

// Initiates a new tunnel to a remote cluster.
func (c *Connection) initTunnel(cluster string, timeout time.Duration) (*Tunnel, error) {
	// Sanity check on the arguments
	if len(cluster) == 0 {
		return nil, errors.New("empty cluster identifier")
	}
	timeoutms := int(timeout.Nanoseconds() / 1000000)
	if timeoutms < 1 {
		return nil, fmt.Errorf("invalid timeout %v < 1ms", timeout)
	}
	// Create a potential tunnel
	tun, err := c.newTunnel()
	if err != nil {
		return nil, err
	}
	tun.Log.Info("constructing outbound tunnel", "cluster", cluster, "timeout", timeout)

	// Try and construct the tunnel
	err = c.sendTunnelInit(tun.id, cluster, timeoutms)
	if err == nil {
		// Wait for tunneling completion or a timeout
		select {
		case init := <-tun.init:
			if init {
				// Send the data allowance
				if err = c.sendTunnelAllowance(tun.id, defaultTunnelBuffer); err == nil {
					tun.Log.Info("tunnel construction completed", "chunk_limit", tun.chunkLimit)
					return tun, nil
				}
			} else {
				err = ErrTimeout
			}
		case <-c.term:
			err = ErrClosed
		}
	}
	// Clean up and return the failure
	c.tunLock.Lock()
	delete(c.tunLive, tun.id)
	c.tunLock.Unlock()

	tun.Log.Warn("tunnel construction failed", "reason", err)
	return nil, err
}

// Accepts an incoming tunneling request and confirms its local id.
func (c *Connection) acceptTunnel(initId uint64, chunkLimit int) (*Tunnel, error) {
	// Create the local tunnel endpoint
	tun, err := c.newTunnel()
	if err != nil {
		return nil, err
	}
	tun.chunkLimit = chunkLimit
	tun.Log.Info("accepting inbound tunnel", "chunk_limit", chunkLimit)

	// Confirm the tunnel creation to the relay node
	err = c.sendTunnelConfirm(initId, tun.id)
	if err == nil {
		// Send the data allowance
		err = c.sendTunnelAllowance(tun.id, defaultTunnelBuffer)
		if err == nil {
			tun.Log.Info("tunnel acceptance completed")
			return tun, nil
		}
	}
	c.tunLock.Lock()
	delete(c.tunLive, tun.id)
	c.tunLock.Unlock()

	tun.Log.Warn("tunnel acceptance failed", "reason", err)
	return nil, err
}

// Sends a message over the tunnel to the remote pair, blocking until the local
// Iris node receives the message or the operation times out.
//
// Infinite blocking is supported with by setting the timeout to zero (0).
func (t *Tunnel) Send(message []byte, timeout time.Duration) error {
	t.Log.Debug("sending message", "data", logLazyBlob(message), "timeout", logLazyTimeout(timeout))

	// Sanity check on the arguments
	if message == nil || len(message) == 0 {
		return errors.New("nil or empty message")
	}
	// Create timeout signaler
	var deadline <-chan time.Time
	if timeout != 0 {
		deadline = time.After(timeout)
	}
	// Split the original message into bounded chunks
	for pos := 0; pos < len(message); pos += t.chunkLimit {
		end := pos + t.chunkLimit
		if end > len(message) {
			end = len(message)
		}
		sizeOrCont := len(message)
		if pos != 0 {
			sizeOrCont = 0
		}
		if err := t.sendChunk(message[pos:end], sizeOrCont, deadline); err != nil {
			return err
		}
	}
	return nil
}

// Sends a single message chunk to the remote endpoint.
func (t *Tunnel) sendChunk(chunk []byte, sizeOrCont int, deadline <-chan time.Time) error {
	for {
		// Short circuit if there's enough space allowance already
		if t.drainAllowance(len(chunk)) {
			return t.conn.sendTunnelTransfer(t.id, sizeOrCont, chunk)
		}
		// Query for a send allowance
		select {
		case <-t.term:
			return ErrClosed
		case <-deadline:
			return ErrTimeout
		case <-t.atoiSign:
			// Potentially enough space allowance, retry
			continue
		}
	}
}

// Checks whether there is enough space allowance available to send a message.
// If yes, the allowance is reduced accordingly.
func (t *Tunnel) drainAllowance(need int) bool {
	t.atoiLock.Lock()
	defer t.atoiLock.Unlock()

	if t.atoiSpace >= need {
		t.atoiSpace -= need
		return true
	}
	// Not enough, reset allowance grant flag
	select {
	case <-t.atoiSign:
	default:
	}
	return false
}

// Retrieves a message from the tunnel, blocking until one is available or the
// operation times out.
//
// Infinite blocking is supported with by setting the timeout to zero (0).
func (t *Tunnel) Recv(timeout time.Duration) ([]byte, error) {
	// Short circuit if there's a message already buffered
	if msg := t.fetchMessage(); msg != nil {
		return msg, nil
	}
	// Create the timeout signaler
	var after <-chan time.Time
	if timeout != 0 {
		after = time.After(timeout)
	}
	// Wait for a message to arrive
	select {
	case <-t.term:
		return nil, ErrClosed
	case <-after:
		return nil, ErrTimeout
	case <-t.itoaSign:
		if msg := t.fetchMessage(); msg != nil {
			return msg, nil
		}
		panic("signal raised but message unavailable")
	}
}

// Fetches the next buffered message, or nil if none is available. If a message
// was available, grants the remote side the space allowance just consumed.
func (t *Tunnel) fetchMessage() []byte {
	t.itoaLock.Lock()
	defer t.itoaLock.Unlock()

	if !t.itoaBuf.Empty() {
		message := t.itoaBuf.Pop().([]byte)
		go t.conn.sendTunnelAllowance(t.id, len(message))

		t.Log.Debug("fetching queued message", "data", logLazyBlob(message))
		return message
	}
	// No message, reset arrival flag
	select {
	case <-t.itoaSign:
	default:
	}
	return nil
}

// Closes the tunnel between the pair. Any blocked read and write operation will
// terminate with a failure.
//
// The method blocks until the local relay node acknowledges the tear-down.
func (t *Tunnel) Close() error {
	// Short circuit if remote end already closed
	select {
	case <-t.term:
		return t.stat
	default:
	}
	// Signal the relay and wait for closure
	t.Log.Info("closing tunnel")
	if err := t.conn.sendTunnelClose(t.id); err != nil {
		return err
	}
	<-t.term
	return t.stat
}

// Finalizes the tunnel construction.
func (t *Tunnel) handleInitResult(chunkLimit int) {
	if chunkLimit > 0 {
		t.chunkLimit = chunkLimit
	}
	t.init <- (chunkLimit > 0)
}

// Increases the available data allowance of the remote endpoint.
func (t *Tunnel) handleAllowance(space int) {
	t.atoiLock.Lock()
	defer t.atoiLock.Unlock()

	t.atoiSpace += space
	select {
	case t.atoiSign <- struct{}{}:
	default:
	}
}

// Adds the chunk to the currently building message and delivers it upon
// completion. If a new message starts, the old is discarded.
func (t *Tunnel) handleTransfer(size int, chunk []byte) {
	// If a new message is arriving, dump anything stored before
	if size != 0 {
		if t.chunkBuf != nil {
			t.Log.Warn("incomplete message discarded", "size", cap(t.chunkBuf), "arrived", len(t.chunkBuf))

			// A large transfer timed out, new started, grant the partials allowance
			go t.conn.sendTunnelAllowance(t.id, len(t.chunkBuf))
		}
		t.chunkBuf = make([]byte, 0, size)
	}
	// Append the new chunk and check completion
	t.chunkBuf = append(t.chunkBuf, chunk...)
	if len(t.chunkBuf) == cap(t.chunkBuf) {
		t.itoaLock.Lock()
		defer t.itoaLock.Unlock()

		t.Log.Debug("queuing arrived message", "data", logLazyBlob(t.chunkBuf))
		t.itoaBuf.Push(t.chunkBuf)
		t.chunkBuf = nil

		select {
		case t.itoaSign <- struct{}{}:
		default:
		}
	}
}

// Handles the graceful remote closure of the tunnel.
func (t *Tunnel) handleClose(reason string) {
	if reason != "" {
		t.Log.Warn("tunnel dropped", "reason", reason)
		t.stat = fmt.Errorf("remote error: %s", reason)
	} else {
		t.Log.Info("tunnel closed gracefully")
	}
	close(t.term)
}
