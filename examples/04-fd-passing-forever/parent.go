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

	"github.com/poolpOrg/go-ipcmsg"
)

func parent() {
	pid, fd := fork_child()
	channel := ipcmsg.NewChannel("parent<->child", pid, fd)
	channel.Handler(IPCMSG_PONG, handlePONG)

	fp, err := os.Open("/etc/passwd")
	if err != nil {
		log.Fatal("could not open")
	}
	channel.Message(IPCMSG_PING, "PING ?", int(fp.Fd()))
	<-channel.Dispatch()
}

func handlePONG(msg *ipcmsg.IPCMessage) {
	var data string
	msg.Unmarshal(&data)

	fmt.Printf("parent: got PONG with fd=%d from child: %s\n", msg.Fd(), data)
	msg.Reply(IPCMSG_PING, "PING !", msg.Fd())
}
