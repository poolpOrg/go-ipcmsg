# go-ipcmsg

**WIP:**
This is work in progress.

The `go-ipcmsg` package provides a simple mechanism for communication between
local processes using sockets.
Each transmitted message is guaranteed to be presented to the receiving program whole.
They are commonly used in privilege separated processes,
where processes with different rights are required to cooperate.

It is inspired by OpenBSD's `imsg(3)` API,
however it is not intended to be wire compatible and uses a different approach making use of Golang's channels.

For example of use,
see the [examples directory](https://github.com/poolpOrg/ipcmsg/blob/main/examples/).

## Semantics

- A channel wraps one end of a stream socket (`socketpair(2)` with
  `SOCK_STREAM`) and owns it from then on.
- The encoded payload of a message is at most `IPCMSG_MAX_DATA_SIZE`
  (65535) bytes, larger payloads are refused.
- A message may carry one file descriptor. The channel takes ownership of
  it and closes it once sent: hand it a descriptor nothing else owns, not
  the `Fd()` of an `os.File` that is still alive. On the receiving side
  `Fd()` is the caller's to close.
- `Query` waits for the reply; `Dispatch` must be running to route it.
  When the peer goes away first, `Query` returns `nil`.
- When the peer closes the connection, `ChannelIn()` is closed and the
  channel returned by `Dispatch` receives once.
- Protocol violations and socket errors end the process, as `imsg(3)`
  users would `fatal()`.
