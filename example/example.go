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

const (
	IPCMSG_PING  ipcmsg.IPCMsgType = iota
	IPCMSG_HELLO ipcmsg.IPCMsgType = iota
)

// parent process main routine, forks a child then sets up an ipcmsg
// Channel on the socketpair, associating a handler for msg type 42.
//
func parentDispatcherPING(channel *ipcmsg.Channel, msg ipcmsg.IPCMessage) {
	fmt.Printf("PARENT: GOT %s FROM CHILD\n", msg.Data)
}

func parentDispatcherHELLO(channel *ipcmsg.Channel, msg ipcmsg.IPCMessage) {
	fmt.Printf("PARENT: GOT %s FROM CHILD\n", msg.Data)
}

func parent() {
	pid, fd := fork_child()
	channel := ipcmsg.NewChannel(pid, fd)
	channel.Handler(IPCMSG_PING, parentDispatcherPING)
	channel.Handler(IPCMSG_HELLO, parentDispatcherHELLO)
	go channel.Dispatch()

	for {
		channel.Write(IPCMSG_PING, []byte("PING"), -1)
		time.Sleep(1 * time.Second)

		channel.Write(IPCMSG_HELLO, []byte("HELLO"), -1)
		time.Sleep(1 * time.Second)
	}
}

// child process main routine, sets up an ipcmsg Channel on fd 3,
// associating a handler for msg type 42.
//
func childDispatcherPING(channel *ipcmsg.Channel, msg ipcmsg.IPCMessage) {
	fmt.Printf("CHILD: GOT %s FROM PARENT\n", msg.Data)
	channel.Reply(msg, []byte("PONG"), -1)
}

func childDispatcherHELLO(channel *ipcmsg.Channel, msg ipcmsg.IPCMessage) {
	fmt.Printf("CHILD: GOT %s FROM PARENT\n", msg.Data)
	channel.Reply(msg, []byte("HELLO"), -1)
}

func child() {
	channel := ipcmsg.NewChannel(os.Getppid(), 3)
	channel.Handler(IPCMSG_PING, childDispatcherPING)
	channel.Handler(IPCMSG_HELLO, childDispatcherHELLO)
	channel.Dispatch()
}
