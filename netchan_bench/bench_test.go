package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"testing"

	"github.com/robzan8/netchan"
)

var (
	mn    *netchan.Session
	tasks = make(chan benchTask)
	done  = make(chan struct{})
)

const network = "unix"

func check(err error) {
	if err != nil {
		log.Output(2, err.Error())
		os.Exit(1)
	}
}

func TestMain(m *testing.M) {
	flag.Parse()

	peerPath, err := exec.LookPath("netchan_bench")
	if err != nil {
		log.Fatal("Before running the benchmarks, ",
			"you should install netchan_bench and make it reachable from your $PATH.")
	}
	var random [20]byte
	_, err = rand.Read(random[:])
	check(err)
	sockDir := "/tmp/netchan/bench_socks"
	err = os.MkdirAll(sockDir, 0700)
	check(err)
	sockName := sockDir + "/" + hex.EncodeToString(random[:])
	ln, err := net.Listen(network, sockName)
	check(err)
	peer := exec.Command(peerPath, network, sockName)
	stderr, err := peer.StderrPipe()
	check(err)
	err = peer.Start()
	check(err)
	go func() {
		var buf [512]byte
		n, err := stderr.Read(buf[:])
		if err == io.EOF {
			return
		}
		check(err)
		log.Fatalf("Error from bench peer: %s", buf[0:n])
	}()
	conn, err := ln.Accept()
	check(err)
	ln.Close()
	mn = netchan.NewSession(conn)
	go func() {
		<-mn.Done()
		if err := mn.Err(); err != netchan.EndOfSession {
			mn.Quit()
			log.Fatal(err)
		}
	}()
	err = mn.OpenSend("tasks", tasks)
	check(err)
	err = mn.OpenRecv("done", done, 1)
	check(err)

	exitCode := m.Run()

	close(tasks)
	// wait that peer shuts down
	<-mn.Done()
	// wait that local shut down completes
	mn.Quit()
	os.Exit(exitCode)
}

func Benchmark_Chans1(b *testing.B) {
	task := benchTask{1, b.N}
	tasks <- task
	executeTask(task, mn)
	<-done
}

func Benchmark_Chans10(b *testing.B) {
	task := benchTask{10, b.N}
	tasks <- task
	executeTask(task, mn)
	<-done
}

func Benchmark_Chans100(b *testing.B) {
	task := benchTask{100, b.N}
	tasks <- task
	executeTask(task, mn)
	<-done
}
