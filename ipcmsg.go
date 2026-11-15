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
	"fmt"
	"log"
	"math"
	"os"
	"reflect"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

type Channel struct {
	name string

	w chan *IPCMessage
	r chan *IPCMessage

	muQueries sync.Mutex
	queries   map[uuid.UUID]chan *IPCMessage

	muHandlers sync.Mutex
	handlers   map[IPCMsgType]func(*IPCMessage)
}

const IPCMSG_HEADER_SIZE = 31

// IPCMSG_MAX_DATA_SIZE is the largest encoded payload a message can carry,
// the header stores the size on 16 bits.
const IPCMSG_MAX_DATA_SIZE = math.MaxUint16

type IPCMsgType uint32

type ipcMsgHdr struct {
	Id     uuid.UUID
	Type   IPCMsgType
	Size   uint16
	HasFd  uint8
	Peerid uint32
	Pid    uint32
}

// encode packs the header in network byte order, field after field.
func (hdr *ipcMsgHdr) encode() []byte {
	buf := make([]byte, IPCMSG_HEADER_SIZE)
	copy(buf[0:16], hdr.Id[:])
	binary.BigEndian.PutUint32(buf[16:20], uint32(hdr.Type))
	binary.BigEndian.PutUint16(buf[20:22], hdr.Size)
	buf[22] = hdr.HasFd
	binary.BigEndian.PutUint32(buf[23:27], hdr.Peerid)
	binary.BigEndian.PutUint32(buf[27:31], hdr.Pid)
	return buf
}

// decodeHeader is the inverse of encode, buf holds at least a full header.
func decodeHeader(buf []byte) ipcMsgHdr {
	var hdr ipcMsgHdr
	copy(hdr.Id[:], buf[0:16])
	hdr.Type = IPCMsgType(binary.BigEndian.Uint32(buf[16:20]))
	hdr.Size = binary.BigEndian.Uint16(buf[20:22])
	hdr.HasFd = buf[22]
	hdr.Peerid = binary.BigEndian.Uint32(buf[23:27])
	hdr.Pid = binary.BigEndian.Uint32(buf[27:31])
	return hdr
}

type IPCMessage struct {
	channel *Channel
	hdr     ipcMsgHdr
	fd      int
	data    []byte
}

var muMsgTypes sync.Mutex
var msgTypes = make(map[IPCMsgType]reflect.Type)

// NewIPCMsgType registers the Go type of msgObject as the payload type of
// a new message type.
func NewIPCMsgType(msgObject interface{}) IPCMsgType {
	msgReflectType := reflect.TypeOf(msgObject)
	if msgReflectType == nil {
		panic("NewIPCMsgType: nil message object")
	}
	gob.Register(msgObject)

	muMsgTypes.Lock()
	defer muMsgTypes.Unlock()
	msgType := IPCMsgType(len(msgTypes))
	msgTypes[msgType] = msgReflectType
	return msgType
}

// NewChannel wraps the stream socket fd, connected to the process peerid.
// The channel owns fd from then on.
func NewChannel(name string, peerid int, fd int) *Channel {
	channel := &Channel{}
	pid := os.Getpid()

	channel.name = name
	channel.queries = make(map[uuid.UUID]chan *IPCMessage)
	channel.handlers = make(map[IPCMsgType]func(*IPCMessage))
	channel.w = make(chan *IPCMessage)
	channel.r = make(chan *IPCMessage)

	go channel.writer(fd, peerid, pid)
	go channel.reader(fd, peerid, pid)

	return channel
}

// writer reads messages from the write channel and sends them to the peer.
func (channel *Channel) writer(fd int, peerid int, pid int) {
	for msg := range channel.w {
		msg.hdr.Peerid = uint32(peerid)
		msg.hdr.Pid = uint32(pid)

		obuf := append(msg.hdr.encode(), msg.data...)
		if err := sendAll(fd, obuf, msg.fd); err != nil {
			log.Fatal("NewChannel: syscall.Sendmsg: ", err)
		}

		// the descriptor now lives in the peer, our copy is no longer
		// needed: there is nothing left to do should closing it fail.
		if msg.hdr.HasFd != 0 {
			syscall.Close(msg.fd)
		}
	}
}

// sendAll writes buf to the stream socket fd, attaching msgfd to the
// first bytes when there is one, and keeps going on short writes.
func sendAll(fd int, buf []byte, msgfd int) error {
	var oob []byte
	if msgfd != -1 {
		oob = syscall.UnixRights(msgfd)
	}
	for len(buf) > 0 {
		n, err := syscall.SendmsgN(fd, buf, oob, nil, 0)
		if err != nil {
			return err
		}
		buf = buf[n:]
		oob = nil
	}
	return nil
}

// parseFd extracts the single file descriptor carried by the control
// messages received alongside a message.
func parseFd(oob []byte) (int, error) {
	scms, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, err
	}
	if len(scms) != 1 {
		return -1, fmt.Errorf("expected one control message, got %d", len(scms))
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil {
		return -1, err
	}
	if len(fds) != 1 {
		for _, pfd := range fds {
			syscall.Close(pfd)
		}
		return -1, fmt.Errorf("expected one file descriptor, got %d", len(fds))
	}
	return fds[0], nil
}

// reader receives from the peer, reassembles messages whatever way the
// stream was cut, and hands them to the read channel. Descriptors arrive
// with the read holding the first byte of their message and at most one
// per read, so they are matched in order to the messages announcing one.
func (channel *Channel) reader(fd int, peerid int, pid int) {
	buf := make([]byte, 64*1024)
	cmsgbuf := make([]byte, syscall.CmsgSpace(16*4))
	var pending []byte
	var fds []int

	for {
		n, oobn, _, _, err := syscall.Recvmsg(fd, buf, cmsgbuf, 0)
		if err != nil && err != syscall.ECONNRESET {
			log.Fatal("NewChannel: syscall.Recvmsg: ", err)
		}

		// peer is gone, ECONNRESET is how Linux reports it when the
		// peer left messages unread
		if n <= 0 {
			if len(pending) != 0 {
				log.Fatalf("channel %s: peer closed in the middle of a message", channel.name)
			}
			close(channel.r)
			return
		}

		if oobn > 0 {
			pfd, err := parseFd(cmsgbuf[:oobn])
			if err != nil {
				log.Fatal("NewChannel: control message: ", err)
			}
			fds = append(fds, pfd)
		}

		pending = append(pending, buf[:n]...)
		for len(pending) >= IPCMSG_HEADER_SIZE {
			hdr := decodeHeader(pending)
			total := IPCMSG_HEADER_SIZE + int(hdr.Size)
			if len(pending) < total {
				break
			}

			msg := &IPCMessage{}
			msg.channel = channel
			msg.hdr = hdr
			msg.hdr.Peerid = uint32(peerid)
			msg.hdr.Pid = uint32(pid)
			msg.data = append([]byte(nil), pending[IPCMSG_HEADER_SIZE:total]...)
			msg.fd = -1

			// a message announcing a descriptor that did not make it
			// (descriptor exhaustion on our side) is delivered with
			// Fd() == -1
			if msg.hdr.HasFd != 0 && len(fds) > 0 {
				msg.fd = fds[0]
				fds = fds[1:]
			}

			pending = pending[total:]
			channel.r <- msg
		}

		if len(fds) > 0 && len(pending) == 0 {
			log.Fatalf("channel %s: received a file descriptor no message announced", channel.name)
		}
	}
}

// Dispatch routes replies to their pending Query and other messages to
// the handler registered for their type. The returned channel receives
// once the peer has closed the connection, at which point pending queries
// are released with a nil reply.
func (channel *Channel) Dispatch() <-chan bool {
	done := make(chan bool, 1)
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

			channel.muHandlers.Lock()
			handler, exists := channel.handlers[msg.hdr.Type]
			channel.muHandlers.Unlock()
			if !exists {
				log.Fatalf("channel %s: received unexpected message type %d", channel.name, msg.hdr.Type)
			}

			handler(msg)
		}

		channel.muQueries.Lock()
		for id, callbackChannel := range channel.queries {
			close(callbackChannel)
			delete(channel.queries, id)
		}
		channel.muQueries.Unlock()
		done <- true
	}()
	return done
}

func (channel *Channel) Handler(msgtype IPCMsgType, handler func(*IPCMessage)) {
	channel.muHandlers.Lock()
	defer channel.muHandlers.Unlock()
	channel.handlers[msgtype] = handler
}

func createMessage(msgtype IPCMsgType, data interface{}, fd int) *IPCMessage {
	muMsgTypes.Lock()
	msgReflectType, exists := msgTypes[msgtype]
	muMsgTypes.Unlock()
	if !exists {
		panic("unregistered IPC message type")
	}
	if reflect.TypeOf(data) != msgReflectType {
		panic("creating IPC message with invalid data type")
	}
	if fd < -1 {
		panic("creating IPC message with invalid file descriptor")
	}

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	err := enc.Encode(data)
	if err != nil {
		panic(err)
	}
	if buf.Len() > IPCMSG_MAX_DATA_SIZE {
		panic(fmt.Sprintf("IPC message data is %d bytes, more than the %d allowed", buf.Len(), IPCMSG_MAX_DATA_SIZE))
	}

	msg := &IPCMessage{}
	msg.hdr = ipcMsgHdr{}
	msg.hdr.Id, _ = uuid.NewRandom()
	msg.hdr.Type = msgtype
	msg.hdr.Size = uint16(buf.Len())
	if fd == -1 {
		msg.hdr.HasFd = 0
	} else {
		msg.hdr.HasFd = 1
	}
	msg.data = buf.Bytes()
	msg.fd = fd

	return msg
}

func createReply(msg IPCMessage, msgtype IPCMsgType, data interface{}, fd int) *IPCMessage {
	reply := createMessage(msgtype, data, fd)
	reply.hdr.Id = msg.hdr.Id
	return reply
}

// Message sends data to the peer. The channel takes ownership of fd, if
// any, and closes it once sent.
func (channel *Channel) Message(msgtype IPCMsgType, data interface{}, fd int) {
	channel.w <- createMessage(msgtype, data, fd)
}

// Query sends data to the peer and waits for its reply, which Dispatch
// must be running to route. It returns nil if the peer goes away first.
func (channel *Channel) Query(msgtype IPCMsgType, data interface{}, fd int) *IPCMessage {
	wait := make(chan *IPCMessage, 1)
	msg := createMessage(msgtype, data, fd)
	channel.muQueries.Lock()
	channel.queries[msg.hdr.Id] = wait
	channel.muQueries.Unlock()

	channel.w <- msg
	return <-wait
}

func (channel *Channel) ChannelIn() <-chan *IPCMessage {
	return channel.r
}

func (channel *Channel) ChannelOut() chan<- *IPCMessage {
	return channel.w
}

func (msg *IPCMessage) Type() IPCMsgType {
	return msg.hdr.Type
}

func (msg *IPCMessage) TryUnmarshal(v interface{}) error {
	dec := gob.NewDecoder(bytes.NewBuffer(msg.data))
	return dec.Decode(v)
}

func (msg *IPCMessage) Unmarshal(v interface{}) {
	err := msg.TryUnmarshal(v)
	if err != nil {
		panic(err)
	}
}

func (msg *IPCMessage) HasFd() bool {
	return msg.hdr.HasFd == 1
}

// Fd returns the descriptor received with the message, -1 if none. It is
// the caller's to close.
func (msg *IPCMessage) Fd() int {
	return msg.fd
}

func (msg *IPCMessage) Reply(msgtype IPCMsgType, data interface{}, fd int) {
	msg.channel.w <- createReply(*msg, msgtype, data, fd)
}

func (msg *IPCMessage) OneOf(msgtypes ...IPCMsgType) *IPCMessage {
	for _, msgtype := range msgtypes {
		if msg.Type() == msgtype {
			return msg
		}
	}
	panic("IPCMessage type assertion failed")
}
