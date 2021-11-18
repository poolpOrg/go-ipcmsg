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
	"log"
	"math/rand"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/poolpOrg/ipcmsg"
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

// a showcase dispatcher function that's shared by parent and child:
// - consume messages as they arrive on read channel
// - emit a message every second on write channel, some with an fd
//
func dispatcher(processname string, r chan ipcmsg.IPCMessage, w chan ipcmsg.IPCMessage) {
	for {
		messages := []string{
			"foobarbazqux",
			"barbazquxfoo",
			"bazquxfoobar",
			"quxfoobarbaz",
		}

		select {
		case <-time.After(1 * time.Second):
			message := messages[rand.Intn(len(messages))]
			if rand.Int()%5 != 0 {
				log.Printf("[%s] sending message %s", processname, message)
				w <- ipcmsg.Message(42, []byte(message))
			} else {
				fd, err := syscall.Open(os.Args[0], 0700, 0)
				if err != nil {
					log.Fatal(err)
				}
				log.Printf("[%s] sending message %s, fd attached", processname, message)
				w <- ipcmsg.MessageWithFd(42, []byte(message), fd)
			}

		case msg := <-r:
			if msg.Hdr.HasFd != 0 {
				log.Printf("[%s] [fd=%d] data: %s\n", processname, msg.Fd, string(msg.Data))
			} else {
				log.Printf("[%s] data: %s\n", processname, string(msg.Data))
			}
			if msg.Fd != -1 {
				syscall.Close(msg.Fd)
			}
		}
	}
}

// parent process main routine, forks a child then sets up an ipcmsg
// Channel on the socketpair, returning read and write channels. The
// channels can be used to emit messages to the other process.
//
func parent() {
	pid, fd := fork_child()
	child_r, child_w := ipcmsg.Channel(pid, fd)
	dispatcher("parent", child_r, child_w)
}

// child process main routine, sets up an ipcmsg Channel on fd 3,
// returning read and write channels to communicate with the other
// process
//
func child() {
	parent_r, parent_w := ipcmsg.Channel(os.Getppid(), 3)
	dispatcher("child", parent_r, parent_w)
}
