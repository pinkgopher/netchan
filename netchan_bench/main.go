package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"github.com/robzan8/netchan"
)

const (
	itemSize      = 32
	wantBatchSize = 4096 // should agree with the one defined in package netchan
	recvBufSize   = 50000
)

type item [itemSize]byte

type benchTask struct {
	NumChans, NumItems int
}

func executeTask(task benchTask, mn *netchan.Session) {
	var wg sync.WaitGroup
	chCap := wantBatchSize / itemSize
	bufCap := recvBufSize / itemSize

	for i := 0; i < task.NumChans; i++ {
		ch := make(chan item, chCap)
		err := mn.OpenSend(fmt.Sprintf("items-%d", i), ch)
		if err != nil {
			panic(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < task.NumItems; j++ {
				ch <- item{}
			}
			close(ch)
		}()
	}

	for i := 0; i < task.NumChans; i++ {
		ch := make(chan item, chCap)
		err := mn.OpenRecv(fmt.Sprintf("items-%d", i), ch, bufCap)
		if err != nil {
			panic(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _ = range ch {
			}
		}()
	}

	wg.Wait()
}

func main() {
	if len(os.Args) != 3 {
		log.Fatal("len(Args) != 3")
	}
	conn, err := net.Dial(os.Args[1], os.Args[2])
	if err != nil {
		log.Fatal(err)
	}
	mn := netchan.NewSession(conn)
	go func() {
		<-mn.Done()
		if err := mn.Err(); err != netchan.EndOfSession {
			mn.Quit()
			os.Exit(1)
		}
	}()
	tasks := make(chan benchTask)
	err = mn.OpenRecv("tasks", tasks, 1)
	if err != nil {
		log.Fatal(err)
	}
	done := make(chan struct{})
	err = mn.OpenSend("done", done)
	if err != nil {
		log.Fatal(err)
	}

	for t := range tasks {
		executeTask(t, mn)
		done <- struct{}{}
	}
	mn.Quit()
}
