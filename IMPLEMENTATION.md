# Implementation

A document that explains in detail how the client and the server works.

## Client implementation

The client holds (0, N) connections to a single host.
A connection is created in the following cases:
- There are no previous existing connections.
- All the connections are busy (aka not able to open more streams).

Connections are stored in a list because it's the easiest way to keep elements.

When a connection is created 2 goroutines are spawned. One for reading
and dispatching events, and another for writing (either frames and requests).

The [read loop](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L357)
will read all the frames and handling only the ones carrying a StreamID.
Lower layers will handle everything related to Settings, WindowUpdate, Ping
and/or disconnection.

The [write loop](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L290)
will write the requests and frames. I like to separate both terms because the request
comes from fasthttp, and the `frames` is a term related to http2.

Why having 2 coroutines? As HTTP/2 is a replacement of HTTP/1.1, the equivalent
to opening a connection per request in HTTP/1 is the figure of the `frame` in HTTP/2.
As writing to the same connection might happen concurrently and thus, can invoke
errors, 2 coroutines are required, one for writing and another for reading
synchronously.

### How sending a request works?

When we send a request we write to a channel to the writeLoop coroutine with
all the data required, in this case we make use of the [Ctx](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/client.go#L26-L33)
structure.

That being sent, it gets received by the writeLoop coroutine, and then
it proceeds to [serialize and write](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L385)
into the connection the required frames, and after that [registers](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L321)
the StreamID into a shared map. This map is shared among the 'write' and 'read' loops.

In the meantime, the client [waits on a channel](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/client.go#L102)
for any error.

When we receive the response from the server, the readLoop will check if the StreamID
is on the shared map, and if so, it will [handle the response](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L559).
After the server finished sending the request, the readLoop will end the request
sending the result to the client. That result might be an error or just a `nil`
over the channel provided by the client.

After the request/response finished, the client will continue thus exiting the
`Do` function.

## Server implementation

The server is initialized via `ConfigureServer` (TLS) or `ConfigureServerH2C` (cleartext).
Each incoming connection spawns `ServeConn`, which:

1. Reads the HTTP/2 connection preface
2. Performs the SETTINGS handshake
3. Starts three goroutines: `readLoop`, `writeLoop`, and `handleStreams`

### Connection goroutines

- **readLoop**: reads frames from the TCP connection, dispatches stream-carrying
  frames to the `reader` channel, handles connection-level frames (SETTINGS,
  WINDOW_UPDATE, PING, GOAWAY) inline.
- **writeLoop**: drains the `writer` channel and flushes frames to the TCP
  connection with batching (flush every 10 frames or when the channel is empty).
- **handleStreams**: processes stream lifecycle — creates streams on HEADERS,
  manages the request/response cycle, enforces max concurrent streams, handles
  timeouts and idle connections.

### Flow control

The server maintains both connection-level and stream-level flow control windows.
When a client sends DATA, the server's receive windows are decremented. The server
sends WINDOW_UPDATE frames to replenish the windows. On the send side, the server
respects the client's advertised windows and queues data when the window is exhausted.

### Response trailers

Handlers can set response trailers via `ctx.SetUserValue(http2.TrailerUserValueKey, map[string]string{...})`.
The server sends a trailing HEADERS frame with END_STREAM after the response body.

### Graceful shutdown

`Server.Shutdown()` sends GOAWAY to all active connections, signaling them to
drain their streams and close.