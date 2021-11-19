# go-ipcmsg

**WIP:**
This is work in progress, do not use for anything serious.

The `go-ipcmsg` package provides a simple mechanism for communication between
local processes using sockets.
Each transmitted message is guaranteed to be presented to the receiving program whole.
They are commonly used in privilege separated processes,
where processes with different rights are required to cooperate.

It is inspired by OpenBSD's `imsg(3)` API,
however it is not intended to be wire compatible and uses a different approach making use of Golang's channels.

For example of use,
see the [example program](https://github.com/poolpOrg/ipcmsg/blob/main/example/example.go)
