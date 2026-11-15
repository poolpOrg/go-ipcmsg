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
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/google/uuid"
)

// The header must keep the layout encoding/binary gave it in earlier
// versions, so that both ends of a socket may run different versions.
func TestHeaderKeepsItsWireLayout(t *testing.T) {
	hdr := ipcMsgHdr{Id: uuid.New(), Type: 0x01020304, Size: 0xBEEF, HasFd: 1, Peerid: 0x11223344, Pid: 0x55667788}

	var legacy bytes.Buffer
	if err := binary.Write(&legacy, binary.BigEndian, &hdr); err != nil {
		t.Fatal(err)
	}
	if legacy.Len() != IPCMSG_HEADER_SIZE {
		t.Fatalf("header is %d bytes on the wire, IPCMSG_HEADER_SIZE says %d", legacy.Len(), IPCMSG_HEADER_SIZE)
	}
	if got := hdr.encode(); !bytes.Equal(got, legacy.Bytes()) {
		t.Fatalf("encode gives\n%x\nearlier versions sent\n%x", got, legacy.Bytes())
	}
	if got := decodeHeader(legacy.Bytes()); got != hdr {
		t.Fatalf("decoded %+v, want %+v", got, hdr)
	}
}

func devNull(t *testing.T) int {
	t.Helper()
	fd, err := syscall.Open("/dev/null", syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func isOpen(fd int) bool {
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil
}

func TestParseFdAcceptsExactlyOneDescriptor(t *testing.T) {
	fd := devNull(t)
	defer syscall.Close(fd)

	got, err := parseFd(syscall.UnixRights(fd))
	if err != nil {
		t.Fatal(err)
	}
	if got != fd {
		t.Fatalf("got descriptor %d, want %d", got, fd)
	}
}

func TestParseFdRejectsAndClosesSeveralDescriptors(t *testing.T) {
	fd1, fd2 := devNull(t), devNull(t)

	_, err := parseFd(syscall.UnixRights(fd1, fd2))
	if err == nil || !strings.Contains(err.Error(), "got 2") {
		t.Fatalf("err = %v", err)
	}
	if isOpen(fd1) || isOpen(fd2) {
		t.Fatal("rejected descriptors left open")
	}
}

func TestParseFdRejectsSeveralControlMessages(t *testing.T) {
	fd := devNull(t)
	defer syscall.Close(fd)

	oob := append(syscall.UnixRights(fd), syscall.UnixRights(fd)...)
	_, err := parseFd(oob)
	if err == nil || !strings.Contains(err.Error(), "one control message") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseFdRejectsMalformedControlMessage(t *testing.T) {
	fd := devNull(t)
	defer syscall.Close(fd)

	oob := syscall.UnixRights(fd)
	(*syscall.Cmsghdr)(unsafe.Pointer(&oob[0])).SetLen(len(oob) * 4)
	if _, err := parseFd(oob); err == nil {
		t.Fatal("accepted a control message longer than its buffer")
	}
}

func TestParseFdRejectsOtherControlMessages(t *testing.T) {
	fd := devNull(t)
	defer syscall.Close(fd)

	oob := syscall.UnixRights(fd)
	(*syscall.Cmsghdr)(unsafe.Pointer(&oob[0])).Type = syscall.SCM_RIGHTS + 1
	if _, err := parseFd(oob); err == nil {
		t.Fatal("accepted a control message that carries no descriptor")
	}
}
