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

	"github.com/poolpOrg/go-ipcmsg"
)

func parent() {
	pid, fd := fork_child()
	channel := ipcmsg.NewChannel("parent<->child", pid, fd)
	channel.Handler(IPCMSG_PONG, handlePONG)
	channel.Message(IPCMSG_PING, []byte("PING ?"), -1)
	<-channel.Dispatch()
}

func handlePONG(channel *ipcmsg.Channel, msg ipcmsg.IPCMessage) {
	fmt.Printf("parent: got PONG from child: %s\n", string(msg.Data))
	channel.Reply(msg, IPCMSG_PING, []byte("PING !"), -1)
}
