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

package ipcmsg

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"log"
	"os"
	"reflect"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

type Channel struct {
	name string

	w chan IPCMessage
	r chan IPCMessage

	muQueries sync.Mutex
	queries   map[uuid.UUID]chan IPCMessage

	muHandlers sync.Mutex
	handlers   map[IPCMsgType]func(IPCMessage)
}

const IPCMSG_HEADER_SIZE = 31

type IPCMsgType uint32

type ipcMsgHdr struct {
	Id     uuid.UUID
	Type   IPCMsgType
	Size   uint16
	HasFd  uint8
	Peerid uint32
	Pid    uint32
}

type IPCMessage struct {
	channel *Channel
	hdr     ipcMsgHdr
	fd      int
	data    []byte
}

var msgTypes = make(map[IPCMsgType]string)

func NewIPCMsgType(msgObject interface{}) IPCMsgType {
	msgType := IPCMsgType(len(msgTypes))
	msgTypes[msgType] = reflect.TypeOf(msgObject).Name()
	gob.Register(msgObject)
	return msgType
}

func NewChannel(name string, peerid int, fd int) *Channel {
	channel := &Channel{}
	pid := os.Getpid()

	channel.name = name
	channel.queries = make(map[uuid.UUID]chan IPCMessage)
	channel.handlers = make(map[IPCMsgType]func(IPCMessage))
	channel.w = make(chan IPCMessage)
	channel.r = make(chan IPCMessage)

	// read message from write channel and send to peer fd
	go func() {
		for msg := range channel.w {
			msg.hdr.Peerid = uint32(peerid)
			msg.hdr.Pid = uint32(pid)

			// pack msg header and msg data into output buf
			obuf := make([]byte, 0)

			var packed bytes.Buffer
			if err := binary.Write(&packed, binary.BigEndian, &msg.hdr); err != nil {
				log.Fatal(err)
			}
			obuf = append(obuf, packed.Bytes()...)
			obuf = append(obuf, msg.data...)

			// if msg has no FD attached, send as is
			if msg.hdr.HasFd == 0 {
				err := syscall.Sendmsg(fd, obuf, nil, nil, 0)
				if err != nil {
					log.Fatal(err)
				}
				// annnnnnnd... we're done for this msg
				continue
			}

			// an FD is attached, we need to craft a UnixRights control message
			err := syscall.Sendmsg(fd, obuf, syscall.UnixRights([]int{msg.fd}...), nil, 0)
			if err != nil {
				log.Fatal(err)
			}

			// close the attached FD
			err = syscall.Close(msg.fd)
			if err != nil {
				log.Fatal(err)
			}
		}
	}()

	// read message from peer fd and write to read channel
	go func() {
		// oh gosh... the fun begins
		for {

			// a buffer to hold the data
			buf := make([]byte, 64*1024)

			// a cmsgbuf for control message, we only expect 1 fd (4 bytes)
			cmsgbuf := make([]byte, syscall.CmsgSpace(1*4))

			// read a msg, for now only expects blocking IO
			n, _, _, _, err := syscall.Recvmsg(fd, buf, cmsgbuf, 0)
			if err != nil {
				log.Fatal(err)
			}
			if n == 0 {
				break
			}

			buf = buf[:n]

			// sometimes we have an FD, sometimes we don't
			// assume there's a control message and try parsing it,
			// if it fails then we assume there's no FD
			// caller can detect this is IPCMsgHdr.HasFlag is 1 and IpcMsg.Fd == -1
			cmsg := true
			scms, err := syscall.ParseSocketControlMessage(cmsgbuf)
			if err != nil {
				if err != syscall.EINVAL {
					log.Fatal(err)
				}
				cmsg = false
			}

			pfd := -1
			if cmsg {
				// we have a control message ...
				// we're only supposed to have one
				if len(scms) != 1 {
					log.Fatal("received more than one control message")
				}
				fds, err := syscall.ParseUnixRights(&scms[0])
				if err != nil {
					log.Fatal(err)
				}

				// we're only supposed to have one FD
				if len(fds) != 1 {
					log.Fatal("received more than one FD")
				}
				pfd = fds[0]
			}

			// we may have multiple messages crammed in our input buffer
			// process them sequentially, parsing header and extracting data
			for {
				// first, decode a header
				var hdr_bin bytes.Buffer
				var hdr ipcMsgHdr
				hdr_bin.Write(buf[:IPCMSG_HEADER_SIZE])
				err = binary.Read(&hdr_bin, binary.BigEndian, &hdr)
				if err != nil {
					log.Fatal(err)
				}

				// unsure if this can happen, sanity check
				if len(buf) < IPCMSG_HEADER_SIZE+int(hdr.Size) {
					log.Fatal("packet too small")
				}

				// now that we have a header, reset peerid and pid
				// extract the right amount of data from input buffer
				// and if a FD is supposed to be attached, use the one
				// we extracted from control message
				msg := IPCMessage{}
				msg.channel = channel
				msg.hdr = hdr
				msg.hdr.Peerid = uint32(peerid)
				msg.hdr.Pid = uint32(pid)
				msg.data = buf[IPCMSG_HEADER_SIZE : IPCMSG_HEADER_SIZE+int(msg.hdr.Size)]
				msg.fd = -1
				if msg.hdr.HasFd != 0 {
					if pfd == -1 {
						// FD exhaustion on receiving end most-likely
					}
					msg.fd = pfd
					pfd = -1
				}

				// discard consumed data from input buffer
				buf = buf[IPCMSG_HEADER_SIZE+int(msg.hdr.Size):]

				// message is ready for caller
				channel.r <- msg

				// input buffer is empty, go back to read loop
				if len(buf) == 0 {
					break
				}

				// not sure if short reads can happen,
				// if so they'll be caught by the earlier log.Fatal()
				// and I'll move the input buffer out of the goroutine
				// into the Channel
			}
		}
	}()

	return channel
}

func (channel *Channel) Dispatch() <-chan bool {
	done := make(chan bool)
	go func() {
		for msg := range channel.r {
			channel.muQueries.Lock()
			callbackChannel, exists := channel.queries[msg.hdr.Id]
			delete(channel.queries, msg.hdr.Id)
			channel.muQueries.Unlock()
			if exists {
				callbackChannel <- msg
				continue
			}

			handler, exists := channel.handlers[msg.hdr.Type]
			if !exists {
				log.Fatalf("channel %s: received unexpected message type %d", channel.name, msg.hdr.Type)
			}

			handler(msg)
		}
		done <- true
	}()
	return done
}

func (channel *Channel) Handler(msgtype IPCMsgType, handler func(IPCMessage)) {
	channel.muHandlers.Lock()
	defer channel.muHandlers.Unlock()
	channel.handlers[msgtype] = handler
}

func createMessage(msgtype IPCMsgType, data interface{}, fd int) IPCMessage {
	if reflecType, exists := msgTypes[msgtype]; !exists {
		panic("unregistered IPC message type")
	} else {
		if reflect.TypeOf(data).Name() != reflecType {
			panic("creating IPC message with invalid data type")
		}
	}

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	err := enc.Encode(data)
	if err != nil {
		panic(err)
	}

	msg := IPCMessage{}
	msg.hdr = ipcMsgHdr{}
	msg.hdr.Id, _ = uuid.NewRandom()
	msg.hdr.Type = msgtype
	msg.hdr.Size = uint16(len(buf.Bytes()))
	if fd == -1 {
		msg.hdr.HasFd = 0
	} else {
		msg.hdr.HasFd = 1
	}
	msg.data = buf.Bytes()
	msg.fd = fd

	return msg
}

func createReply(msg IPCMessage, msgtype IPCMsgType, data interface{}, fd int) IPCMessage {
	reply := createMessage(msgtype, data, fd)
	reply.hdr.Id = msg.hdr.Id
	return reply
}

func (channel *Channel) Message(msgtype IPCMsgType, data interface{}, fd int) {
	channel.w <- createMessage(msgtype, data, fd)
}

func (channel *Channel) Query(msgtype IPCMsgType, data interface{}, fd int) IPCMessage {
	wait := make(chan IPCMessage)
	msg := createMessage(msgtype, data, fd)
	channel.muQueries.Lock()
	channel.queries[msg.hdr.Id] = wait
	channel.muQueries.Unlock()

	channel.w <- msg
	return <-wait
}

func (channel *Channel) ChannelIn() <-chan IPCMessage {
	return channel.r
}

func (channel *Channel) ChannelOut() chan<- IPCMessage {
	return channel.w
}

func (msg *IPCMessage) Type() IPCMsgType {
	return msg.hdr.Type
}

func (msg *IPCMessage) Unmarshal(v interface{}) {
	dec := gob.NewDecoder(bytes.NewBuffer(msg.data))
	err := dec.Decode(v)
	if err != nil {
		panic(err)
	}
}

func (msg *IPCMessage) HasFd() bool {
	return msg.hdr.HasFd == 1
}

func (msg *IPCMessage) Fd() int {
	return msg.fd
}

func (msg *IPCMessage) Reply(msgtype IPCMsgType, data interface{}, fd int) {
	msg.channel.w <- createReply(*msg, msgtype, data, fd)
}
