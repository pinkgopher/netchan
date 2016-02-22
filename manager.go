package netchan

/* TODO:
- more tests
- fuzzy testing
- update docs
- draw graph documenting components
- debug logger, "want to send" msg
- tune desired batch size
- adjust ShutDown timeout
*/

import (
	"bufio"
	"encoding/gob"
	"io"
	"reflect"
	"sync/atomic"
	"time"
)

// once is an implementation of sync.Once that uses a channel.
type once struct {
	done  chan struct{}
	state int32
}

// once state
const (
	onceNotDone int32 = iota
	onceDone
	onceDoing
)

func (o *once) Do(f func()) {
	if atomic.LoadInt32(&o.state) == onceDone {
		return
	}
	// Slow path.
	won := atomic.CompareAndSwapInt32(&o.state, onceNotDone, onceDoing)
	if !won {
		<-o.done
		return
	}
	f()
	atomic.StoreInt32(&o.state, onceDone)
	close(o.done)
	// Relative order of the last two statements is important
	// for correctly retrieving Error() after <-ErrorSignal().
}

// A Manager handles the message traffic of its connection, implementing the netchan
// protocol.
type Manager struct {
	conn   io.ReadWriteCloser
	recvMn *recvManager
	sendMn *sendManager

	errOnce, closeOnce once
	err, closeErr      error
}

/*
This graph shows how the goroutines and channels of a manager are organized:

 ----> +----------+       +---------+       +---------+       +----------+ ---->
 ----> |  sender  | ----> | encoder | ====> | decoder | ----> | receiver | ---->
 ----> +----------+       +---------+       +---------+       +----------+ ---->
       [send table]                                           [recv table]
       +----------+       +---------+       +---------+       +----------+
       | credRecv | <---- | decoder | <==== | encoder | <---- | credSend |
       +----------+       +---------+       +---------+       +----------+

The sender has a table that contains an entry for each channel that has been opened for
sending. The user values flow through a pipeline from the sender to the receiver on the
other side of the connection.
Credits flow in the opposite direction. There is no cycle, as, for example, the sender
shares the table with the credit receiver and they do not communicate through channels.
The former graph is a simplification, because each manager has actually both a sender and
a receiver:

 ----> +----------+                                                   +----------+ ---->
 ----> |  sender  | ---\                                         /--> | receiver | ---->
 ----> +----------+     \                                       /     +----------+ ---->
       [send table]      \--> +---------+       +---------+ ---/      [recv table]
       +----------+           | encoder | ====> | decoder |           +----------+
       | credRecv | <-\   /-> +---------+       +---------+ --\   /-- | credSend |
       +----------+    \ /                                     \ /    +----------+
                        X                                       X
       +----------+    / \                                     / \    +----------+
       | credSend | --/   \-- +---------+       +---------+ <-/   \-> | credRecv |
       +----------+           | decoder | <==== | encoder |           +----------+
       [recv table]      /--- +---------+       +---------+ <--\      [send table]
 <---- +----------+     /                                       \     +----------+ <----
 <---- | receiver | <--/                                         \--- |  sender  | <----
 <---- +----------+                                                   +----------+ <----

To avoid deadlocked/leaking goroutines, termination must happen in pipeline order. For
example, the sender and the credit sender both check periodically if an error occurred
with Error(). If so, they close the channels to the encoder. The encoder does not check
Error and keeps draining the channels until they are empty. Then it sends the error to
the peer and closes the connection.
*/

const (
	defMsgSizeLimit = 16 * 1024
	minMsgSizeLimit = wantBatchSize * 2
	maxNameLen      = 500
)

// Manage function starts a new Manager for the specified connection and returns it. The
// connection can be any full-duplex io.ReadWriteCloser that provides in-order delivery
// of data with best-effort reliability. On each end, a connection must have only one
// manager.
//
// There is a default limit imposed on the size of incoming gob messages. To change it,
// use ManageLimit.
//
// ManageLimit is like Manage, but also allows to specify the maximum size of the gob
// messages that will be accepted from the connection. If msgSizeLimit is 0 or negative,
// the default will be used. When a too big message is received, an error is signaled on
// this manager and the manager shuts down.
func Manage(conn io.ReadWriteCloser) *Manager {
	return ManageLimit(conn, defMsgSizeLimit)
}

type bufReader interface {
	io.Reader
	io.ByteReader
}

type bufWriter interface {
	io.Writer
	Flush() error
}

func ManageLimit(conn io.ReadWriteCloser, msgSizeLimit int) *Manager {
	if msgSizeLimit < minMsgSizeLimit {
		msgSizeLimit = minMsgSizeLimit
	}

	// create all the components, connect them with channels and fire up the goroutines.
	recvMn := new(recvManager)
	sendMn := new(sendManager)
	mn := &Manager{conn: conn, recvMn: recvMn, sendMn: sendMn}
	mn.errOnce.done = make(chan struct{})
	mn.closeOnce.done = make(chan struct{})

	const chCap int = 8
	recvMnCh := make(chan userData, chCap)
	sendMnCh := make(chan credit, chCap)
	encData := make(chan userData, chCap)
	encCredits := make(chan credit, chCap)

	enc := &encoder{dataCh: encData, credits: encCredits, mn: mn}
	bw, ok := conn.(bufWriter)
	if !ok {
		bw = bufio.NewWriter(conn)
	}
	enc.flushFn = bw.Flush
	enc.countWr = countWriter{w: bw}
	enc.enc = gob.NewEncoder(&enc.countWr)

	dec := &decoder{toRecvMn: recvMnCh, toSendMn: sendMnCh, mn: mn,
		msgSizeLimit: msgSizeLimit}
	br, ok := conn.(bufReader)
	if !ok {
		br = bufio.NewReader(conn)
	}
	dec.types.elemType = make(map[int]reflect.Type)
	dec.limitedRd = limitedReader{bufReader: br}
	dec.dec = gob.NewDecoder(&dec.limitedRd)

	*recvMn = recvManager{dataChan: recvMnCh, toEncoder: encCredits,
		types: &dec.types, mn: mn}
	recvMn.table.entry = make(map[int]recvEntry)
	recvMn.table.present = make(map[string]struct{})
	*sendMn = sendManager{credits: sendMnCh, toEncoder: encData, mn: mn}
	sendMn.table.chans = make(map[int]senderChans)
	sendMn.table.info = make(map[string]openInfo)

	go recvMn.run()
	go sendMn.run()
	go enc.run()
	go dec.run()
	return mn
}

// Open method opens a net-chan with the given name and direction on the connection
// handled by the manager. The channel argument must be a channel and will be used for
// receiving or sending data on this net-chan.
//
// If the direction is Recv, the following rules apply: channel must be buffered and its
// buffer must be empty (cap(channel) > 0 && len(channel) == 0); the channel must be used
// exclusively for receiving values from a single net-chan.
//
// Opening a net-chan twice, i.e. with the same name and direction on the same manager,
// will return an error. It is possible to have, on a single manager/connection, two
// net-chans with the same name and opposite directions.
//
// An eventual error returned by Open does not compromise the netchan session, that is,
// the error will not be caught by ErrorSignal and Error methods, will not be
// communicated to the peer and the manager will not shut down.
//
// To close a net-chan, close the channel used for sending; the receiving channel on the
// other peer will be closed too. Messages that are already in the buffers or in flight
// will not be lost.
func (m *Manager) OpenSend(name string, channel interface{}) error {
	if len(name) > maxNameLen {
		return newErr("OpenSend: name too long")
	}
	ch := reflect.ValueOf(channel)
	if ch.Kind() != reflect.Chan {
		return newErr("OpenSend: channel arg is not a channel")
	}
	if ch.Type().ChanDir()&reflect.RecvDir == 0 {
		return newErr("OpenSend requires a <-chan")
	}
	return m.sendMn.open(name, ch)
}

func (m *Manager) OpenRecv(name string, channel interface{}, bufferCap int) error {
	if len(name) > maxNameLen {
		return newErr("OpenRecv: name too long")
	}
	ch := reflect.ValueOf(channel)
	if ch.Kind() != reflect.Chan {
		return newErr("OpenRecv channel is not a channel")
	}
	if ch.Type().ChanDir()&reflect.SendDir == 0 {
		return newErr("OpenRecv requires a chan<-")
	}
	if bufferCap <= 0 {
		return newErr("OpenRecv bufferCap must be at least 1")
	}
	return m.recvMn.open(name, ch, bufferCap)
}

// Error returns the first error that occurred on this manager. If no error
// occurred, it returns nil. When an error occurs, the manager tries to communicate it to
// the peer and then shuts down.
func (m *Manager) Error() error {
	if atomic.LoadInt32(&m.errOnce.state) == onceDone {
		return m.err
	}
	return nil
}

// ErrorSignal returns a channel that never receives any message and is closed when an
// error occurs on this manager.
func (m *Manager) ErrorSignal() <-chan struct{} {
	return m.errOnce.done
}

// ShutDown tries to send a termination message to the peer and then shuts down the
// manager and closes the connection. The ErrorSignal channel is closed and Error will
// return io.EOF. The remote peer will also shut down and get EOF, if the termination
// message is received correctly.
//
// The return value is the result of calling Close on the connection. The connection is
// guaranteed to be closed once and only once, even if ShutDown is called multiple times,
// possibly by multiple goroutines.
func (m *Manager) ShutDown() error {
	return m.ShutDownWith(EndOfSession)
}

func (m *Manager) closeConn() {
	m.closeOnce.Do(func() {
		m.closeErr = m.conn.Close()
	})
}

// ShutDownWith is like ShutDown, but err is signaled instead of EOF.
func (m *Manager) ShutDownWith(err error) error {
	if err == nil {
		err = EndOfSession
	}
	m.errOnce.Do(func() {
		m.err = err
	})
	select {
	// encoder tries to send error to peer; if/when it succeeds,
	// it closes the connection and we wake up and return
	case <-m.closeOnce.done:
	// if encoder takes too long, we close the connection ourself
	case <-time.After(1 * time.Second):
		m.closeConn()
	}
	return m.closeErr
}
