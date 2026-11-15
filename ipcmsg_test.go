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

package ipcmsg_test

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/poolpOrg/go-ipcmsg"
)

// Time shares its name with time.Time but is a different type.
type Time struct {
	X int
}

// unencodable has no exported field, gob refuses to encode it.
type unencodable struct {
	x int
}

var (
	msgString      = ipcmsg.NewIPCMsgType("")
	msgReply       = ipcmsg.NewIPCMsgType("")
	msgInt         = ipcmsg.NewIPCMsgType(0)
	msgTime        = ipcmsg.NewIPCMsgType(time.Time{})
	msgLocalTime   = ipcmsg.NewIPCMsgType(Time{})
	msgUnencodable = ipcmsg.NewIPCMsgType(unencodable{})
)

const timeout = 10 * time.Second

// wireHeader is the documented layout of a message header on the socket.
type wireHeader struct {
	Id     [16]byte
	Type   uint32
	Size   uint16
	HasFd  uint8
	Peerid uint32
	Pid    uint32
}

func socketpair(t *testing.T) (int, int) {
	t.Helper()
	sp, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	syscall.CloseOnExec(sp[0])
	syscall.CloseOnExec(sp[1])
	t.Cleanup(func() {
		syscall.Close(sp[0])
		syscall.Close(sp[1])
	})
	return sp[0], sp[1]
}

// endOnCleanup shuts the sockets down once the test is over and waits for
// the channels on them to notice, so that no goroutine is still reading a
// socket when it gets closed.
func endOnCleanup(t *testing.T, fds []int, channels ...*ipcmsg.Channel) {
	t.Cleanup(func() {
		for _, fd := range fds {
			syscall.Shutdown(fd, syscall.SHUT_RDWR)
		}
		for _, channel := range channels {
			deadline := time.After(timeout)
			for open := true; open; {
				select {
				case _, open = <-channel.ChannelIn():
				case <-deadline:
					t.Error("channel still open after its socket was shut down")
					open = false
				}
			}
		}
	})
}

type pair struct {
	a, b     *ipcmsg.Channel
	fda, fdb int
}

func newPair(t *testing.T) *pair {
	t.Helper()
	fda, fdb := socketpair(t)
	p := &pair{
		a:   ipcmsg.NewChannel("a", 2, fda),
		b:   ipcmsg.NewChannel("b", 1, fdb),
		fda: fda,
		fdb: fdb,
	}
	endOnCleanup(t, []int{fda, fdb}, p.a, p.b)
	return p
}

// rawPeer is a channel whose other end the test drives by hand.
func rawPeer(t *testing.T) (*ipcmsg.Channel, int) {
	t.Helper()
	fda, fdb := socketpair(t)
	a := ipcmsg.NewChannel("a", 2, fda)
	endOnCleanup(t, []int{fda, fdb}, a)
	return a, fdb
}

func recv(t *testing.T, ch <-chan *ipcmsg.IPCMessage) *ipcmsg.IPCMessage {
	t.Helper()
	select {
	case msg, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while waiting for a message")
		}
		return msg
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a message")
	}
	return nil
}

func recvClosed(t *testing.T, ch <-chan *ipcmsg.IPCMessage) {
	t.Helper()
	select {
	case msg, ok := <-ch:
		if ok {
			t.Fatalf("expected the channel to close, got message type %d", msg.Type())
		}
	case <-time.After(timeout):
		t.Fatal("timed out waiting for the channel to close")
	}
}

func recvDone(t *testing.T, done <-chan bool) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for Dispatch to end")
	}
}

func unmarshalString(t *testing.T, msg *ipcmsg.IPCMessage) string {
	t.Helper()
	var s string
	msg.Unmarshal(&s)
	return s
}

func unmarshalInt(t *testing.T, msg *ipcmsg.IPCMessage) int {
	t.Helper()
	var i int
	msg.Unmarshal(&i)
	return i
}

// tempFd opens a file holding content, as a raw descriptor no os.File owns.
func tempFd(t *testing.T, dir string, content string) int {
	t.Helper()
	f, err := os.CreateTemp(dir, "f")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(f.Name(), syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	syscall.CloseOnExec(fd)
	return fd
}

// highFd moves fd to a number no newly received descriptor will reuse.
func highFd(t *testing.T, fd int) int {
	t.Helper()
	r, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD, 300)
	if errno != 0 {
		t.Fatal(errno)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatal(err)
	}
	return int(r)
}

func readFd(t *testing.T, fd int) string {
	t.Helper()
	if _, err := syscall.Seek(fd, 0, 0); err != nil {
		t.Fatal(err)
	}
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return string(out)
		}
		out = append(out, buf[:n]...)
	}
}

func fdIsOpen(fd int) bool {
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil
}

// nextFd tells which number the next opened descriptor gets.
func nextFd(t *testing.T) int {
	t.Helper()
	fd, err := syscall.Open(os.DevNull, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	syscall.Close(fd)
	return fd
}

func gobBytes(t *testing.T, v interface{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// frame builds a message as it travels on the socket.
func frame(t *testing.T, id uuid.UUID, msgtype ipcmsg.IPCMsgType, hasFd bool, payload []byte) []byte {
	t.Helper()
	hdr := wireHeader{Id: id, Type: uint32(msgtype), Size: uint16(len(payload)), Peerid: 1, Pid: 1}
	if hasFd {
		hdr.HasFd = 1
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.BigEndian, hdr); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != ipcmsg.IPCMSG_HEADER_SIZE {
		t.Fatalf("test header is %d bytes, the library announces %d", buf.Len(), ipcmsg.IPCMSG_HEADER_SIZE)
	}
	buf.Write(payload)
	return buf.Bytes()
}

func rawWrite(t *testing.T, fd int, data []byte, oob []byte) {
	t.Helper()
	if err := syscall.Sendmsg(fd, data, oob, nil, 0); err != nil {
		t.Fatal(err)
	}
}

// rawRecv reads one message as the kernel delivers it, with any
// descriptors attached.
func rawRecv(t *testing.T, fd int) ([]byte, []int) {
	t.Helper()
	buf := make([]byte, 128*1024)
	oob := make([]byte, syscall.CmsgSpace(4*4))
	n, oobn, _, _, err := syscall.Recvmsg(fd, buf, oob, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fds []int
	if oobn > 0 {
		scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			t.Fatal(err)
		}
		for i := range scms {
			got, err := syscall.ParseUnixRights(&scms[i])
			if err != nil {
				t.Fatal(err)
			}
			fds = append(fds, got...)
		}
	}
	return buf[:n], fds
}

func expectPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic mentioning %q", want)
		}
		if !strings.Contains(fmt.Sprint(r), want) {
			t.Fatalf("panic %v does not mention %q", r, want)
		}
	}()
	fn()
}

func TestNewIPCMsgTypeGivesDistinctTypes(t *testing.T) {
	seen := map[ipcmsg.IPCMsgType]bool{}
	for _, msgtype := range []ipcmsg.IPCMsgType{msgString, msgReply, msgInt, msgTime, msgLocalTime} {
		if seen[msgtype] {
			t.Fatalf("message type %d handed out twice", msgtype)
		}
		seen[msgtype] = true
	}
}

func TestNewIPCMsgTypeRejectsNil(t *testing.T) {
	expectPanic(t, "nil", func() { ipcmsg.NewIPCMsgType(nil) })
}

func TestMessageReachesHandler(t *testing.T) {
	p := newPair(t)
	got := make(chan *ipcmsg.IPCMessage, 1)
	p.b.Handler(msgString, func(msg *ipcmsg.IPCMessage) { got <- msg })
	p.b.Dispatch()

	p.a.Message(msgString, "hello", -1)

	msg := recv(t, got)
	if msg.Type() != msgString {
		t.Fatalf("type %d, want %d", msg.Type(), msgString)
	}
	if s := unmarshalString(t, msg); s != "hello" {
		t.Fatalf("data %q, want %q", s, "hello")
	}
	if msg.HasFd() || msg.Fd() != -1 {
		t.Fatalf("HasFd %v Fd %d for a message sent without descriptor", msg.HasFd(), msg.Fd())
	}
}

func TestLatestHandlerWins(t *testing.T) {
	p := newPair(t)
	first := make(chan *ipcmsg.IPCMessage, 1)
	second := make(chan *ipcmsg.IPCMessage, 1)
	p.b.Dispatch()
	// registering while Dispatch runs is allowed
	p.b.Handler(msgString, func(msg *ipcmsg.IPCMessage) { first <- msg })
	p.b.Handler(msgString, func(msg *ipcmsg.IPCMessage) { second <- msg })

	p.a.Message(msgString, "x", -1)

	recv(t, second)
	select {
	case <-first:
		t.Fatal("replaced handler still called")
	default:
	}
}

func TestQueryGetsItsReply(t *testing.T) {
	p := newPair(t)
	p.b.Handler(msgString, func(msg *ipcmsg.IPCMessage) {
		msg.Reply(msgReply, "re:"+unmarshalString(t, msg), -1)
	})
	p.a.Dispatch()
	p.b.Dispatch()

	reply := p.a.Query(msgString, "ping", -1)

	if reply.Type() != msgReply {
		t.Fatalf("reply type %d, want %d", reply.Type(), msgReply)
	}
	if s := unmarshalString(t, reply); s != "re:ping" {
		t.Fatalf("reply data %q", s)
	}
	if reply.OneOf(msgString, msgReply) != reply {
		t.Fatal("OneOf did not return the message")
	}
	expectPanic(t, "type assertion", func() { reply.OneOf(msgString, msgInt) })
}

func TestRepliesReachTheQueryThatAskedEvenOutOfOrder(t *testing.T) {
	const queries = 40
	p := newPair(t)
	p.b.Handler(msgInt, func(msg *ipcmsg.IPCMessage) {
		i := unmarshalInt(t, msg)
		go func() {
			// later queries are answered first
			time.Sleep(time.Duration(queries-i) * time.Millisecond)
			msg.Reply(msgInt, i*10, -1)
		}()
	})
	p.a.Dispatch()
	p.b.Dispatch()

	var wg sync.WaitGroup
	errs := make(chan error, queries)
	for i := 0; i < queries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reply := p.a.Query(msgInt, i, -1)
			if got := unmarshalInt(t, reply); got != i*10 {
				errs <- fmt.Errorf("query %d got reply %d", i, got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestDescriptorPassingTransfersOwnership(t *testing.T) {
	p := newPair(t)
	fd := highFd(t, tempFd(t, t.TempDir(), "payload"))

	p.a.Message(msgString, "take", fd)

	msg := recv(t, p.b.ChannelIn())
	if !msg.HasFd() || msg.Fd() == -1 {
		t.Fatalf("HasFd %v Fd %d, want a descriptor", msg.HasFd(), msg.Fd())
	}
	if s := readFd(t, msg.Fd()); s != "payload" {
		t.Fatalf("received descriptor reads %q", s)
	}
	// the sender's copy is closed once the message is out
	deadline := time.Now().Add(timeout)
	for fdIsOpen(fd) {
		if time.Now().After(deadline) {
			t.Fatal("sender still holds the descriptor it passed")
		}
		time.Sleep(time.Millisecond)
	}
	if err := syscall.Close(msg.Fd()); err != nil {
		t.Fatal(err)
	}
}

func TestDescriptorsFollowTheirMessageInABurst(t *testing.T) {
	const count = 200
	p := newPair(t)
	dir := t.TempDir()
	fds := make([]int, count)
	for i := range fds {
		fds[i] = -1
		if i%3 != 0 {
			fds[i] = tempFd(t, dir, fmt.Sprint(i))
		}
	}
	go func() {
		for i := 0; i < count; i++ {
			p.a.Message(msgInt, i, fds[i])
		}
	}()
	for i := 0; i < count; i++ {
		msg := recv(t, p.b.ChannelIn())
		if got := unmarshalInt(t, msg); got != i {
			t.Fatalf("message %d received as %d", i, got)
		}
		if i%3 == 0 {
			if msg.HasFd() || msg.Fd() != -1 {
				t.Fatalf("message %d got a descriptor it was not sent with", i)
			}
			continue
		}
		if !msg.HasFd() || msg.Fd() == -1 {
			t.Fatalf("message %d lost its descriptor", i)
		}
		if s := readFd(t, msg.Fd()); s != fmt.Sprint(i) {
			t.Fatalf("message %d received descriptor of %q", i, s)
		}
		syscall.Close(msg.Fd())
	}
}

func TestDescriptorPingPongLeaksNothing(t *testing.T) {
	const hops = 300
	p := newPair(t)
	final := make(chan *ipcmsg.IPCMessage, 1)
	hop := 0
	p.b.Handler(msgString, func(msg *ipcmsg.IPCMessage) {
		msg.Reply(msgReply, "pong", msg.Fd())
	})
	p.a.Handler(msgReply, func(msg *ipcmsg.IPCMessage) {
		hop++
		if hop == hops {
			final <- msg
			return
		}
		msg.Reply(msgString, "ping", msg.Fd())
	})
	p.a.Dispatch()
	p.b.Dispatch()

	baseline := nextFd(t)
	p.a.Message(msgString, "ping", tempFd(t, t.TempDir(), "ball"))

	msg := recv(t, final)
	if s := readFd(t, msg.Fd()); s != "ball" {
		t.Fatalf("descriptor reads %q after %d hops", s, hops)
	}
	syscall.Close(msg.Fd())
	if after := nextFd(t); after > baseline+1 {
		t.Fatalf("descriptors leaked: next descriptor was %d, is now %d", baseline, after)
	}
}

func TestBurstOfMessagesArrivesInOrder(t *testing.T) {
	const count = 3000
	p := newPair(t)
	go func() {
		for i := 0; i < count; i++ {
			p.a.Message(msgInt, i, -1)
		}
	}()
	for i := 0; i < count; i++ {
		if got := unmarshalInt(t, recv(t, p.b.ChannelIn())); got != i {
			t.Fatalf("message %d received as %d", i, got)
		}
	}
}

func TestMessagesLargerThanAReadArriveWhole(t *testing.T) {
	const count = 5
	p := newPair(t)
	go func() {
		for i := 0; i < count; i++ {
			p.a.Message(msgString, strings.Repeat(string(rune('a'+i)), 65000), -1)
		}
	}()
	for i := 0; i < count; i++ {
		got := unmarshalString(t, recv(t, p.b.ChannelIn()))
		if want := strings.Repeat(string(rune('a'+i)), 65000); got != want {
			t.Fatalf("message %d: got %d bytes starting %q", i, len(got), got[:1])
		}
	}
}

func TestOversizedDataIsRefused(t *testing.T) {
	p := newPair(t)
	expectPanic(t, "more than", func() {
		p.a.Message(msgString, strings.Repeat("x", ipcmsg.IPCMSG_MAX_DATA_SIZE+1), -1)
	})
	// nothing went out
	p.a.Message(msgString, "canary", -1)
	if s := unmarshalString(t, recv(t, p.b.ChannelIn())); s != "canary" {
		t.Fatalf("received %q", s)
	}
}

func TestInvalidMessagesAreRefusedBeforeSending(t *testing.T) {
	p := newPair(t)
	cases := []struct {
		name    string
		msgtype ipcmsg.IPCMsgType
		data    interface{}
		fd      int
		want    string
	}{
		{"unregistered type", ipcmsg.IPCMsgType(1 << 30), "x", -1, "unregistered"},
		{"wrong data type", msgString, 42, -1, "invalid data type"},
		{"nil data", msgString, nil, -1, "invalid data type"},
		{"same name other package", msgTime, Time{1}, -1, "invalid data type"},
		{"invalid descriptor", msgString, "x", -2, "invalid file descriptor"},
		{"unencodable data", msgUnencodable, unencodable{1}, -1, "no exported fields"},
	}
	for _, c := range cases {
		expectPanic(t, c.want, func() { p.a.Message(c.msgtype, c.data, c.fd) })
		expectPanic(t, c.want, func() { p.a.Query(c.msgtype, c.data, c.fd) })
	}
	p.a.Message(msgString, "canary", -1)
	if s := unmarshalString(t, recv(t, p.b.ChannelIn())); s != "canary" {
		t.Fatalf("received %q", s)
	}
}

func TestUnmarshalIntoTheWrongType(t *testing.T) {
	p := newPair(t)
	p.a.Message(msgString, "text", -1)
	msg := recv(t, p.b.ChannelIn())

	var i int
	if err := msg.TryUnmarshal(&i); err == nil {
		t.Fatal("decoded a string into an int")
	}
	expectPanic(t, "", func() { msg.Unmarshal(&i) })
	// the message is still intact
	if s := unmarshalString(t, msg); s != "text" {
		t.Fatalf("data %q", s)
	}
}

func TestWireFormat(t *testing.T) {
	a, peer := rawPeer(t)
	fd := tempFd(t, t.TempDir(), "x")

	a.Message(msgString, "wire", -1)
	data, fds := rawRecv(t, peer)

	payload := gobBytes(t, "wire")
	if len(data) != ipcmsg.IPCMSG_HEADER_SIZE+len(payload) {
		t.Fatalf("frame is %d bytes, want %d", len(data), ipcmsg.IPCMSG_HEADER_SIZE+len(payload))
	}
	var hdr wireHeader
	if err := binary.Read(bytes.NewReader(data), binary.BigEndian, &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Id == [16]byte{} {
		t.Fatal("message id is zero")
	}
	if hdr.Type != uint32(msgString) || hdr.Size != uint16(len(payload)) || hdr.HasFd != 0 {
		t.Fatalf("header %+v", hdr)
	}
	if hdr.Peerid != 2 || hdr.Pid != uint32(os.Getpid()) {
		t.Fatalf("header carries peer %d pid %d, want 2 and %d", hdr.Peerid, hdr.Pid, os.Getpid())
	}
	if !bytes.Equal(data[ipcmsg.IPCMSG_HEADER_SIZE:], payload) {
		t.Fatal("payload is not the gob encoding of the data")
	}
	if len(fds) != 0 {
		t.Fatalf("%d descriptors attached to a message sent without", len(fds))
	}

	a.Message(msgString, "wire", fd)
	data, fds = rawRecv(t, peer)
	if data[22] != 1 {
		t.Fatal("HasFd not set on the wire")
	}
	if len(fds) != 1 {
		t.Fatalf("%d descriptors attached, want 1", len(fds))
	}
	if s := readFd(t, fds[0]); s != "x" {
		t.Fatalf("attached descriptor reads %q", s)
	}
	syscall.Close(fds[0])
}

func TestFramesSplitAcrossWritesAreReassembled(t *testing.T) {
	a, peer := rawPeer(t)
	fd := tempFd(t, t.TempDir(), "split")

	f := frame(t, uuid.New(), msgString, false, gobBytes(t, "pieces"))
	for _, piece := range [][]byte{f[:10], f[10:ipcmsg.IPCMSG_HEADER_SIZE], f[ipcmsg.IPCMSG_HEADER_SIZE:]} {
		rawWrite(t, peer, piece, nil)
		time.Sleep(20 * time.Millisecond)
	}
	if s := unmarshalString(t, recv(t, a.ChannelIn())); s != "pieces" {
		t.Fatalf("reassembled %q", s)
	}

	// the descriptor rides with the first bytes of its message
	f = frame(t, uuid.New(), msgString, true, gobBytes(t, "with fd"))
	rawWrite(t, peer, f[:5], syscall.UnixRights(fd))
	time.Sleep(20 * time.Millisecond)
	rawWrite(t, peer, f[5:], nil)
	msg := recv(t, a.ChannelIn())
	if s := unmarshalString(t, msg); s != "with fd" {
		t.Fatalf("reassembled %q", s)
	}
	if !msg.HasFd() || msg.Fd() == -1 {
		t.Fatal("descriptor lost on a split message")
	}
	if s := readFd(t, msg.Fd()); s != "split" {
		t.Fatalf("descriptor reads %q", s)
	}
	syscall.Close(msg.Fd())
}

func TestFramesPackedInOneWriteAreSeparated(t *testing.T) {
	a, peer := rawPeer(t)
	big := strings.Repeat("b", 65500)
	var packed []byte
	packed = append(packed, frame(t, uuid.New(), msgString, false, gobBytes(t, "one"))...)
	packed = append(packed, frame(t, uuid.New(), msgString, false, gobBytes(t, big))...)
	packed = append(packed, frame(t, uuid.New(), msgInt, false, gobBytes(t, 3))...)
	written := make(chan error, 1)
	go func() { written <- syscall.Sendmsg(peer, packed, nil, nil, 0) }()

	if s := unmarshalString(t, recv(t, a.ChannelIn())); s != "one" {
		t.Fatalf("first message %q", s)
	}
	if s := unmarshalString(t, recv(t, a.ChannelIn())); s != big {
		t.Fatalf("second message has %d bytes", len(s))
	}
	if i := unmarshalInt(t, recv(t, a.ChannelIn())); i != 3 {
		t.Fatalf("third message %d", i)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

func TestAnnouncedDescriptorThatNeverArrived(t *testing.T) {
	a, peer := rawPeer(t)
	rawWrite(t, peer, frame(t, uuid.New(), msgString, true, gobBytes(t, "empty handed")), nil)

	msg := recv(t, a.ChannelIn())
	if !msg.HasFd() {
		t.Fatal("HasFd lost")
	}
	if msg.Fd() != -1 {
		t.Fatalf("Fd %d for a descriptor that never arrived", msg.Fd())
	}
}

func TestChannelOutForwardsAMessage(t *testing.T) {
	p := newPair(t)
	q := newPair(t)
	fd := tempFd(t, t.TempDir(), "forwarded")

	p.a.Message(msgString, "relay", fd)
	msg := recv(t, p.b.ChannelIn())
	q.a.ChannelOut() <- msg

	got := recv(t, q.b.ChannelIn())
	if got.Type() != msgString {
		t.Fatalf("type %d", got.Type())
	}
	if s := unmarshalString(t, got); s != "relay" {
		t.Fatalf("data %q", s)
	}
	if s := readFd(t, got.Fd()); s != "forwarded" {
		t.Fatalf("descriptor reads %q", s)
	}
	syscall.Close(got.Fd())
}

// rawRecvBytes reads until n bytes have arrived, however the kernel cuts them.
func rawRecvBytes(t *testing.T, fd int, n int) {
	t.Helper()
	for n > 0 {
		data, _ := rawRecv(t, fd)
		if len(data) == 0 {
			t.Fatal("socket closed before the expected bytes arrived")
		}
		n -= len(data)
	}
}

func TestPeerClosingEndsTheChannel(t *testing.T) {
	a, peer := rawPeer(t)
	done := a.Dispatch()
	a.Message(msgString, "last words", -1)
	rawRecv(t, peer)
	if err := syscall.Close(peer); err != nil {
		t.Fatal(err)
	}
	recvDone(t, done)
	recvClosed(t, a.ChannelIn())
}

func TestPeerClosingReleasesPendingQueries(t *testing.T) {
	a, peer := rawPeer(t)
	a.Dispatch()
	replies := make(chan *ipcmsg.IPCMessage, 2)
	for i := 0; i < 2; i++ {
		go func() { replies <- a.Query(msgString, "anyone?", -1) }()
	}
	rawRecvBytes(t, peer, 2*(ipcmsg.IPCMSG_HEADER_SIZE+len(gobBytes(t, "anyone?"))))
	if err := syscall.Close(peer); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case reply := <-replies:
			if reply != nil {
				t.Fatalf("query got a reply from a dead peer: %q", unmarshalString(t, reply))
			}
		case <-time.After(timeout):
			t.Fatal("query still waiting after the peer left")
		}
	}
}

func TestShutdownEndsBothEnds(t *testing.T) {
	p := newPair(t)
	doneA := p.a.Dispatch()
	doneB := p.b.Dispatch()
	if err := syscall.Shutdown(p.fda, syscall.SHUT_RDWR); err != nil {
		t.Fatal(err)
	}
	recvDone(t, doneA)
	recvDone(t, doneB)
}

// Fatal conditions end the process: each is checked in a copy of the test
// binary running only that test, which must exit with status 1 through
// the library's message rather than through a test failure.

func inFatalChild() bool {
	return os.Getenv("IPCMSG_FATAL_CHILD") == "1"
}

func expectFatal(t *testing.T, want string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "IPCMSG_FATAL_CHILD=1")
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected the process to exit with status 1, got %v\n%s", err, out)
	}
	if strings.Contains(string(out), "--- FAIL") {
		t.Fatalf("the test failed instead of the library exiting\n%s", out)
	}
	if !strings.Contains(string(out), want) {
		t.Fatalf("exit message does not mention %q\n%s", want, out)
	}
}

// awaitFatal gives the library time to end the process; returning makes
// the parent test fail.
func awaitFatal() {
	time.Sleep(5 * time.Second)
}

func TestFatalUnexpectedMessageType(t *testing.T) {
	if !inFatalChild() {
		expectFatal(t, "received unexpected message type")
		return
	}
	p := newPair(t)
	p.b.Dispatch()
	p.a.Message(msgString, "nobody handles this", -1)
	awaitFatal()
}

func TestFatalInvalidSocket(t *testing.T) {
	if !inFatalChild() {
		expectFatal(t, "Recvmsg")
		return
	}
	ipcmsg.NewChannel("broken", 1, -1)
	awaitFatal()
}

func TestFatalSendingToAClosedPeer(t *testing.T) {
	if !inFatalChild() {
		expectFatal(t, "Sendmsg")
		return
	}
	a, peer := rawPeer(t)
	if err := syscall.Close(peer); err != nil {
		t.Fatal(err)
	}
	recvClosed(t, a.ChannelIn())
	a.Message(msgString, "to nobody", -1)
	awaitFatal()
}

func TestFatalTwoDescriptorsInOneMessage(t *testing.T) {
	if !inFatalChild() {
		expectFatal(t, "expected one file descriptor, got 2")
		return
	}
	a, peer := rawPeer(t)
	dir := t.TempDir()
	rights := syscall.UnixRights(tempFd(t, dir, "1"), tempFd(t, dir, "2"))
	rawWrite(t, peer, frame(t, uuid.New(), msgString, true, gobBytes(t, "two")), rights)
	recv(t, a.ChannelIn())
	awaitFatal()
}

func TestFatalDescriptorNoMessageAnnounced(t *testing.T) {
	if !inFatalChild() {
		expectFatal(t, "no message announced")
		return
	}
	a, peer := rawPeer(t)
	rights := syscall.UnixRights(tempFd(t, t.TempDir(), "stray"))
	rawWrite(t, peer, frame(t, uuid.New(), msgString, false, gobBytes(t, "no fd here")), rights)
	recv(t, a.ChannelIn())
	awaitFatal()
}

func TestFatalPeerClosingMidMessage(t *testing.T) {
	if !inFatalChild() {
		expectFatal(t, "in the middle of a message")
		return
	}
	a, peer := rawPeer(t)
	rawWrite(t, peer, frame(t, uuid.New(), msgString, false, gobBytes(t, "cut"))[:10], nil)
	time.Sleep(20 * time.Millisecond)
	if err := syscall.Close(peer); err != nil {
		t.Fatal(err)
	}
	recvClosed(t, a.ChannelIn())
	awaitFatal()
}
