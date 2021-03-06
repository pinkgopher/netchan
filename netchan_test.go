package netchan_test

import (
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinkgopher/netchan"
)

// pipeConn represents one side of a full-duplex
// connection based on io.PipeReader/Writer
type pipeConn struct {
	*io.PipeReader
	*io.PipeWriter
}

func (c pipeConn) Close() error {
	c.PipeReader.Close()
	c.PipeWriter.Close()
	return nil // ignoring errors
}

func newPipeConn() (sideA, sideB pipeConn) {
	sideA.PipeReader, sideB.PipeWriter = io.Pipe()
	sideB.PipeReader, sideA.PipeWriter = io.Pipe()
	return
}

func init() {
	log.SetFlags(0)
}

// intProducer sends integers from 0 to n-1 on net-chan chName
func intProducer(t *testing.T, mn *netchan.Session, chName string, n int) {
	go func() {
		ch := make(chan int, 15)
		err := mn.OpenSend(chName, ch)
		if err != nil {
			log.Fatal(err)
		}
		for i := 0; i < n; i++ {
			select {
			case ch <- i:
			case <-mn.Done():
				log.Fatal(mn.Err())
			}
		}
		close(ch)
	}()
}

// intConsumer drains net-chan chName, stores the received integers in a slice
// and delivers the slice on a channel, which is returned
func intConsumer(t *testing.T, mn *netchan.Session, chName string) <-chan []int {
	sliceCh := make(chan []int, 1)
	go func() {
		var slice []int
		ch := make(chan int, 8)
		err := mn.OpenRecv(chName, ch, 60)
		if err != nil {
			log.Fatal(err)
		}
	Loop:
		for {
			select {
			case i, ok := <-ch:
				if !ok {
					break Loop
				}
				slice = append(slice, i)
			case <-mn.Done():
				log.Fatal(mn.Err())
			}
		}
		sliceCh <- slice
	}()
	return sliceCh
}

// checks that s[i] == i for each i
func checkIntSlice(t *testing.T, s []int) {
	for i, si := range s {
		if i != si {
			log.Fatalf("expected i == s[i], found i == %d, s[i] == %d", i, si)
			return
		}
	}
}

// start the producer before the consumer
func TestSendThenRecv(t *testing.T) {
	sideA, sideB := newPipeConn()
	intProducer(t, netchan.NewSession(sideA), "integers", 100)
	time.Sleep(50 * time.Millisecond)
	s := <-intConsumer(t, netchan.NewSession(sideB), "integers")
	checkIntSlice(t, s)
}

// start the consumer before the producer
func TestRecvThenSend(t *testing.T) {
	sideA, sideB := newPipeConn()
	sliceCh := intConsumer(t, netchan.NewSession(sideB), "integers")
	time.Sleep(50 * time.Millisecond)
	intProducer(t, netchan.NewSession(sideA), "integers", 100)
	checkIntSlice(t, <-sliceCh)
}

// open many chans in both directions
func TestManyChans(t *testing.T) {
	sideA, sideB := newPipeConn()
	manA := netchan.NewSession(sideA)
	manB := netchan.NewSession(sideB)
	var sliceChans [100]<-chan []int
	for i := range sliceChans {
		chName := "integers" + strconv.Itoa(i)
		if i%2 == 0 {
			// producer is sideA, consumer is sideB
			intProducer(t, manA, chName, 400)
			sliceChans[i] = intConsumer(t, manB, chName)
		} else {
			// producer is sideB, consumer is sideA
			intProducer(t, manB, chName, 400)
			sliceChans[i] = intConsumer(t, manA, chName)
		}
	}
	for _, ch := range sliceChans {
		checkIntSlice(t, <-ch)
	}
}

// send many integers on a net-chan with a small buffer. If the credit system is broken,
// at some point the credit will stay 0 (deadlock) or it will excede the limit,
// causing an error
// TODO: find a better way of testing this
func TestCredits(t *testing.T) {
	sideA, sideB := newPipeConn()
	intProducer(t, netchan.NewSession(sideA), "integers", 1000)
	s := <-intConsumer(t, netchan.NewSession(sideB), "integers")
	checkIntSlice(t, s)
}

func TestMsgSizeLimit(t *testing.T) {
	sideA, sideB := newPipeConn()
	go sliceProducer(t, sideA)
	sliceConsumer(t, sideB)
}

const (
	limit     = 2000 // size limit enforced by sliceConsumer
	numSlices = 20   // number of slices to send
)

// sliceProducer sends on "slices". The last slice will be too big.
func sliceProducer(t *testing.T, conn io.ReadWriteCloser) {
	mn := netchan.NewSession(conn)
	ch := make(chan []byte, 1)
	err := mn.OpenSend("slices", ch)
	if err != nil {
		log.Fatal(err)
	}
	small := make([]byte, limit-30) // some tolerance for gob type info
	big := make([]byte, limit+5)
	for i := 1; i <= numSlices; i++ {
		slice := small
		if i == numSlices {
			slice = big
		}
		select {
		case ch <- slice:
		case <-mn.Done():
			log.Fatal(mn.Err())
		}
	}
	close(ch)
}

// sliceConsumer receives from "slices" using a limitedReader. The last slice is too big
// and must generate an error that matches the one returned by the limitedReader used by
// the decoder
func sliceConsumer(t *testing.T, conn io.ReadWriteCloser) {
	mn := netchan.NewSessionLimit(conn, limit)
	// use a receive buffer with capacity 1, so that items come
	// one at a time and we get the error for the last one only
	ch := make(chan []byte)
	err := mn.OpenRecv("slices", ch, 1)
	if err != nil {
		log.Fatal(err)
	}
	for i := 1; i <= numSlices; i++ {
		if i < numSlices {
			select {
			case <-ch:
			case <-mn.Done():
				log.Fatal(mn.Err())
			}
			continue
		}
		// i == numSlices, expect errSizeExceeded
		select {
		case <-ch:
			log.Fatal("manager did not block too big message")
		case <-mn.Done():
			err := mn.Err()
			if strings.Contains(err.Error(), "too big") {
				return // success
			}
			log.Fatal(err)
		}
	}
}
