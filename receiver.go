package netchan

import (
	"reflect"
	"sync"
)

type recvTable struct {
	sync.Mutex
	m map[hashedName]chan<- reflect.Value
}

type receiver struct {
	name        hashedName
	buffer      <-chan reflect.Value
	errorSignal <-chan struct{}
	dataChan    reflect.Value
	encMu       *sync.Mutex
	toEncoder   chan<- message // credits
	bufCap      int
	received    int
	errOccurred bool
}

func (r *receiver) sendToUser(val reflect.Value) {
	sendAndError := [2]reflect.SelectCase{
		{Dir: reflect.SelectSend, Chan: r.dataChan, Send: val},
		{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(r.errorSignal)},
	}
	i, _, _ := reflect.Select(sendAndError[:])
	if i == 1 { // errorSignal
		r.errOccurred = true
	}
}

func (r *receiver) sendToEncoder(payload interface{}) {
	msg := message{r.name, payload}
	select {
	case r.toEncoder <- msg:
	case <-r.errorSignal:
		r.errOccurred = true
	}
}

func (r receiver) run() {
	r.sendToEncoder(&initialCredit{r.bufCap})
	for !r.errOccurred {
		select {
		case val, ok := <-r.buffer:
			if !ok {
				r.dataChan.Close()
				// elemRouter deletes the entry
				return
			}
			r.received++
			if r.received == ceilDivide(r.bufCap, 2) {
				r.sendToEncoder(&credit{r.received})
				r.received = 0
			}
			r.sendToUser(val)
		case <-r.errorSignal:
			return
		}
	}
}

type elemRouter struct {
	elements  <-chan message // from decoder
	encMu     *sync.Mutex
	toEncoder chan<- message // credits
	table     recvTable
	types     *typeTable // decoder's
	mn        *Manager
}

// Open a net-chan for receiving.
func (r *elemRouter) open(nameStr string, ch reflect.Value, bufCap int) error {
	r.table.Lock()
	defer r.table.Unlock()

	name := hashName(nameStr)
	_, present := r.table.m[name]
	if present {
		return errAlreadyOpen("Recv", nameStr)
	}

	r.types.Lock()
	defer r.types.Unlock()

	buffer := make(chan reflect.Value, bufCap)
	r.table.m[name] = buffer
	r.types.m[name] = ch.Type().Elem()

	go receiver{name, buffer, r.mn.ErrorSignal(),
		ch, r.encMu, r.toEncoder, bufCap, 0, false}.run()
	return nil
}

// Got an element from the decoder.
// WARNING: we are not handling initElemMsg
// if data is nil, we got endOfStream
func (r *elemRouter) handleUserData(name hashedName, data *userData) error {
	r.table.Lock()
	buffer, present := r.table.m[name]
	if !present {
		r.table.Unlock()
		return newErr("data arrived for closed net-chan")
	}
	r.table.Unlock()

	select {
	case buffer <- data.val:
		return nil
	default:
		// Sending to the buffer should never be blocking.
		return newErr("peer sent more than its credit allowed")
	}
}

func (r *elemRouter) handleEOS(name hashedName) error {
	r.table.Lock()
	buffer, present := r.table.m[name]
	if !present {
		r.table.Unlock()
		return newErr("end of stream message arrived for closed net-chan")
	}
	r.types.Lock()
	delete(r.table.m, name)
	delete(r.types.m, name)
	r.types.Unlock()
	r.table.Unlock()

	close(buffer)
	return nil
}

func (r *elemRouter) run() {
	for {
		msg, ok := <-r.elements
		if !ok {
			// An error occurred and decoder shut down.
			return
		}
		err := r.mn.Error()
		if err != nil {
			// keep draining bla bla
			continue
		}

		switch pay := msg.payload.(type) {
		case *userData:
			err = r.handleUserData(msg.name, pay)
		case *endOfStream:
			err = r.handleEOS(msg.name)
		default:
			panic("unexpected msg type")
		}

		if err != nil {
			go r.mn.ShutDownWith(err)
		}
	}
}
