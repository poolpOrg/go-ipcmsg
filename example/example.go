/*
 * Copyright (c) 2021 Gilles Chehade <gilles@poolp.org>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/poolpOrg/go-ipcmsg"
)

// upon execution, call parent() which will setup a socketpair
// then fork a child to reexec the program with the REEXEC env
// var set to CHILD, making it execute child().
//
func main() {
	reexec := os.Getenv("REEXEC")
	switch reexec {
	case "":
		parent()
	case "CHILD":
		child()
	}
}

// fork_child() sets up the socketpair to be shared by parent and child,
// passing one end as fd 3 to child & returning the other end to parent.
// the child reexecutes the program with env var REEXEC.
//
func fork_child() (int, int) {
	binary, err := exec.LookPath(os.Args[0])
	if err != nil {
		log.Fatal(err)
	}

	sp, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, syscall.AF_UNSPEC)
	if err != nil {
		log.Fatal(err)
	}

	// XXX - not quite there yet
	//syscall.SetNonblock(sp[0], true)
	//syscall.SetNonblock(sp[1], true)

	procAttr := syscall.ProcAttr{}
	procAttr.Files = []uintptr{
		uintptr(syscall.Stdin),
		uintptr(syscall.Stdout),
		uintptr(syscall.Stderr),
		uintptr(sp[0]),
	}
	procAttr.Env = []string{
		"REEXEC=CHILD",
	}

	var pid int

	pid, err = syscall.ForkExec(binary, []string{os.Args[0]}, &procAttr)
	if err != nil {
		log.Fatal(err)
	}

	if syscall.Close(sp[0]) != nil {
		log.Fatal(err)
	}

	return pid, sp[1]
}

// parent process main routine, forks a child then sets up an ipcmsg
// Channel on the socketpair, returning read and write channels. The
// channels can be used to emit messages to the other process.
//
func parentDispatcher(channel *ipcmsg.Channel, msg ipcmsg.IPCMessage) {
	fmt.Printf("PARENT: GOT %s FROM CHILD\n", msg.Data)
}

func parent() {
	pid, fd := fork_child()
	channel := ipcmsg.NewChannel(pid, fd)
	channel.Handler(42, parentDispatcher)
	go channel.Dispatch()

	for {
		channel.Write(42, []byte("PING"), -1)
		time.Sleep(1 * time.Second)
	}
}

// child process main routine, sets up an ipcmsg Channel on fd 3,
// returning read and write channels to communicate with the other
// process
//
func childDispatcher(channel *ipcmsg.Channel, msg ipcmsg.IPCMessage) {
	fmt.Printf("CHILD: GOT %s FROM PARENT\n", msg.Data)
	channel.Reply(msg, []byte("PONG"), -1)
}

func child() {
	channel := ipcmsg.NewChannel(os.Getppid(), 3)
	channel.Handler(42, childDispatcher)
	channel.Dispatch()
}
