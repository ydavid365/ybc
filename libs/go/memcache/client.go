package memcache

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net"
	"sync"
	"time"
)

var (
	ErrCacheMiss            = errors.New("memcache.Client: cache miss")
	ErrClientNotStarted     = errors.New("memcache.Client: the client isn't started or already stopped")
	ErrCommunicationFailure = errors.New("memcache.Client: communication failure")
	ErrMalformedKey         = errors.New("memcache.Client: malformed key")
	ErrNilValue             = errors.New("memcache.Client: nil value")
	ErrNotModified          = errors.New("memcache.Client: item not modified")
)

const (
	defaultConnectionsCount        = 4
	defaultMaxPendingRequestsCount = 1024
)

// Fast memcache client.
//
// The client is goroutine-safe. It is designed to work fast when hundreds
// concurrent goroutines are calling simultaneously its' methods.
//
// The client works with a single memcached server. Use DistributedClient
// if you want working with multiple servers.
//
// Usage:
//
//   c := Client{
//       ServerAddr: ":11211",
//   }
//   c.Start()
//   defer c.Stop()
//
//   item := Item{
//       Key:   []byte("key"),
//       Value: []byte("value"),
//   }
//   if err := c.Set(&item); err != nil {
//       handleError(err)
//   }
//   if err := c.Get(&item); err != nil {
//       handleError(err)
//   }
//
type Client struct {
	// TCP address of memcached server to connect to.
	// Required parameter.
	//
	// The address should be in the form addr:port.
	ServerAddr string

	// The number of simultaneous TCP connections to establish
	// to memcached server.
	// Optional parameter.
	//
	// The Client is able to squeeze out impossible from a single
	// connection by pipelining a ton of requests on it.
	// Multiple simultaneous connections may be required in the following
	// cases:
	//   * If memcached server delays incoming requests' execution.
	//     Since memcached protocol doesn't allow out-of-order requests'
	//     execution, a single slow request may delay execution of all
	//     the requests pipelined on the connection after it.
	//     Multiple concurrent connections may help in such a situation.
	//   * If memcached server runs on multi-CPU system, but uses a single
	//     CPU (thread) per connection.
	ConnectionsCount int

	// The maximum number of pending requests awaiting to be processed
	// by memcached server.
	// Optional parameter.
	MaxPendingRequestsCount int

	// The size in bytes of buffer used by the Client for reading responses
	// received from memcached per connection.
	// Optional parameter.
	ReadBufferSize int

	// The size in bytes of buffer used by the Client for writing requests
	// to be sent to memcached per connection.
	// Optional parameter.
	WriteBufferSize int

	// The size in bytes of OS-supplied read buffer per TCP connection.
	// Optional parameter.
	OSReadBufferSize int

	// The size in bytes of OS-supplied write buffer per TCP connection.
	// Optional parameter.
	OSWriteBufferSize int

	requests chan tasker
	done     *sync.WaitGroup
}

// Memcache item.
type Item struct {
	// Item's key.
	// Required parameter.
	Key []byte

	// Item's value.
	//
	// The Value is required in set()-type requests and isn't required in
	// get()-type requests.
	Value []byte

	// Expiration time for the item.
	// Zero means the item has no expiration time.
	//
	// The Expiration is used only in set()-type requests.
	Expiration time.Duration

	// An opaque value, which is passed to/from memcache.
	// Optional parameter.
	Flags uint32
}

type tasker interface {
	Init()
	WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool
	ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool
	Done(ok bool)
	Wait() bool
}

func requestsSender(w *bufio.Writer, requests <-chan tasker, responses chan<- tasker, c net.Conn, done *sync.WaitGroup) {
	defer done.Done()
	defer w.Flush()
	defer close(responses)
	scratchBuf := make([]byte, 0, 1024)
	for {
		var t tasker
		var ok bool

		// Flush w only if there are no pending requests.
		select {
		case t, ok = <-requests:
		default:
			w.Flush()
			t, ok = <-requests
		}
		if !ok {
			break
		}
		if !t.WriteRequest(w, &scratchBuf) {
			t.Done(false)
			break
		}
		responses <- t
	}
}

func responsesReceiver(r *bufio.Reader, responses <-chan tasker, c net.Conn, done *sync.WaitGroup) {
	defer done.Done()
	line := make([]byte, 0, 1024)
	for t := range responses {
		if !t.ReadResponse(r, &line) {
			t.Done(false)
			c.Close()
			break
		}
		t.Done(true)
	}
	for t := range responses {
		t.Done(false)
	}
}

func handleAddr(c *Client) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", c.ServerAddr)
	if err != nil {
		log.Printf("Cannot resolve ServerAddr=[%s]: [%s]", c.ServerAddr, err)
		return
	}
	conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		log.Printf("Cannot establish tcp connection to addr=[%s]: [%s]", tcpAddr, err)
		return
	}
	defer conn.Close()

	if err = conn.SetReadBuffer(c.OSReadBufferSize); err != nil {
		log.Fatalf("Cannot set TCP read buffer size to %d: [%s]", c.OSReadBufferSize, err)
	}
	if err = conn.SetWriteBuffer(c.OSWriteBufferSize); err != nil {
		log.Fatalf("Cannot set TCP write buffer size to %d: [%s]", c.OSWriteBufferSize, err)
	}

	r := bufio.NewReaderSize(conn, c.ReadBufferSize)
	w := bufio.NewWriterSize(conn, c.WriteBufferSize)

	responses := make(chan tasker, c.MaxPendingRequestsCount)
	sendRecvDone := &sync.WaitGroup{}
	defer sendRecvDone.Wait()
	sendRecvDone.Add(2)
	go requestsSender(w, c.requests, responses, conn, sendRecvDone)
	go responsesReceiver(r, responses, conn, sendRecvDone)
}

func addrHandler(c *Client, done *sync.WaitGroup) {
	defer done.Done()
	for {
		handleAddr(c)

		// cancel all pending requests
		for t := range c.requests {
			t.Done(false)
		}

		// wait for new incoming requests
		t, ok := <-c.requests
		if !ok {
			// The requests channel is closed.
			return
		}
		c.requests <- t
	}
}

func (c *Client) init() {
	if c.ConnectionsCount == 0 {
		c.ConnectionsCount = defaultConnectionsCount
	}
	if c.MaxPendingRequestsCount == 0 {
		c.MaxPendingRequestsCount = defaultMaxPendingRequestsCount
	}
	if c.ReadBufferSize == 0 {
		c.ReadBufferSize = defaultReadBufferSize
	}
	if c.WriteBufferSize == 0 {
		c.WriteBufferSize = defaultWriteBufferSize
	}
	if c.OSReadBufferSize == 0 {
		c.OSReadBufferSize = defaultOSReadBufferSize
	}
	if c.OSWriteBufferSize == 0 {
		c.OSWriteBufferSize = defaultOSWriteBufferSize
	}

	c.requests = make(chan tasker, c.MaxPendingRequestsCount)
	c.done = &sync.WaitGroup{}
	c.done.Add(1)
}

func (c *Client) run() {
	defer c.done.Done()

	connsDone := &sync.WaitGroup{}
	defer connsDone.Wait()
	for i := 0; i < c.ConnectionsCount; i++ {
		connsDone.Add(1)
		go addrHandler(c, connsDone)
	}
}

func handleClosedRequests(err *error) {
	if r := recover(); r != nil {
		*err = ErrClientNotStarted
	}
}

func (c *Client) pushTask(t tasker) (err error) {
	defer handleClosedRequests(&err)
	c.requests <- t
	return
}

func (c *Client) do(t tasker) (err error) {
	if c.requests == nil {
		return ErrClientNotStarted
	}
	t.Init()
	if err = c.pushTask(t); err != nil {
		return
	}
	if !t.Wait() {
		err = ErrCommunicationFailure
	}
	return
}

// Starts the given client.
//
// No longer needed clients must be stopped via Client.Stop() call.
func (c *Client) Start() {
	if c.done != nil {
		panic("Did you call Client.Stop() before calling Client.Start()?")
	}
	c.init()
	go c.run()
}

// Stops the given client, which has been started via Client.Start() call.
func (c *Client) Stop() {
	if c.done == nil {
		panic("Did you call Client.Start() before calling Client.Stop()?")
	}
	close(c.requests)
	c.done.Wait()
	c.done = nil
}

var doneChansPool = make(chan (chan bool), 1024)

func acquireDoneChan() chan bool {
	select {
	case done := <-doneChansPool:
		return done
	default:
		return make(chan bool, 1)
	}
	panic("unreachable")
}

func releaseDoneChan(done chan bool) {
	select {
	case doneChansPool <- done:
	default:
	}
}

type taskSync struct {
	done chan bool
}

func (t *taskSync) Init() {
	t.done = acquireDoneChan()
}

func (t *taskSync) Done(ok bool) {
	t.done <- ok
}

func (t *taskSync) Wait() bool {
	ok := <-t.done
	releaseDoneChan(t.done)
	return ok
}

type taskGetMulti struct {
	items []Item
	taskSync
}

func readValueHeader(line []byte) (key []byte, flags uint32, size int, ok bool) {
	ok = false

	if !bytes.HasPrefix(line, strValue) {
		log.Printf("Unexpected line read=[%s]. It should start with [%s]", line, strValue)
		return
	}
	line = line[len(strValue):]

	n := -1

	if key = nextToken(line, &n, "key"); key == nil {
		return
	}
	if flags, ok = parseFlagsToken(line, &n); !ok {
		return
	}
	if size, ok = parseSizeToken(line, &n); !ok {
		return
	}
	if n == len(line) {
		return
	}

	if casidUnused := nextToken(line, &n, "casid"); casidUnused == nil {
		ok = false
		return
	}
	ok = expectEof(line, n)
	return
}

func readValue(r *bufio.Reader, size int) (value []byte, ok bool) {
	var err error
	value, err = ioutil.ReadAll(io.LimitReader(r, int64(size)))
	if err != nil {
		log.Printf("Error when reading value with size=%d: [%s]", size, err)
		ok = false
		return
	}
	ok = matchCrLf(r)
	return
}

func readKeyValue(r *bufio.Reader, line []byte) (key []byte, flags uint32, value []byte, ok bool) {
	var size int
	key, flags, size, ok = readValueHeader(line)
	if !ok {
		return
	}

	value, ok = readValue(r, size)
	return
}

func readItem(r *bufio.Reader, scratchBuf *[]byte, item *Item) (ok bool, eof bool, wouldBlock bool) {
	if ok = readLine(r, scratchBuf); !ok {
		return
	}
	line := *scratchBuf
	if bytes.Equal(line, strEnd) {
		ok = true
		eof = true
		return
	}
	if bytes.Equal(line, strWouldBlock) {
		ok = true
		eof = true
		wouldBlock = true
		return
	}

	item.Key, item.Flags, item.Value, ok = readKeyValue(r, line)
	return
}

func (t *taskGetMulti) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	if !writeStr(w, strGets) {
		return false
	}
	itemsCount := len(t.items)
	if itemsCount > 0 {
		if !writeStr(w, t.items[0].Key) {
			return false
		}
	}
	for i := 1; i < itemsCount; i++ {
		if writeWs(w) && !writeStr(w, t.items[i].Key) {
			return false
		}
	}
	return writeCrLf(w)
}

func updateItemByKey(items []Item, item *Item) bool {
	itemsCount := len(items)
	updatedItemsCount := 0

	// This loop may be quite slow for big itemsCount.
	// TODO(valyala): think how to improve it without incurring additional
	// overhead for small itemsCount.
	for i := 0; i < itemsCount; i++ {
		it := &items[i]
		if bytes.Equal(it.Key, item.Key) {
			it.Value = item.Value
			it.Flags = item.Flags
			updatedItemsCount++
		}
	}
	return updatedItemsCount > 0
}

func (t *taskGetMulti) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	var item Item
	for {
		ok, eof, _ := readItem(r, scratchBuf, &item)
		if !ok {
			return false
		}
		if eof {
			break
		}
		if !updateItemByKey(t.items, &item) {
			return false
		}
	}
	return true
}

// Obtains multiple items associated with the the corresponding keys.
//
// Sets Item.Value and Item.Flags for each returned item.
// Doesn't modify Item.Value and Item.Flags for items missing on the server.
func (c *Client) GetMulti(items []Item) error {
	if len(items) == 0 {
		return nil
	}
	for _, item := range items {
		if !validateKey(item.Key) {
			return ErrMalformedKey
		}
	}
	t := taskGetMulti{
		items: items,
	}
	return c.do(&t)
}

type taskGet struct {
	item  *Item
	found bool
	taskSync
}

func (t *taskGet) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeStr(w, strGet) && writeStr(w, t.item.Key) && writeCrLf(w)
}

func readSingleItem(r *bufio.Reader, scratchBuf *[]byte, item *Item) (ok bool, eof bool, wouldBlock bool) {
	keyOriginal := item.Key
	ok, eof, wouldBlock = readItem(r, scratchBuf, item)
	if !ok || eof || wouldBlock {
		return
	}
	if ok = matchStr(r, strEnd); !ok {
		return
	}
	if ok = matchCrLf(r); !ok {
		return
	}
	if ok = bytes.Equal(keyOriginal, item.Key); !ok {
		log.Printf("Key mismatch! Expected [%s], but server returned [%s]", keyOriginal, item.Key)
		return
	}
	item.Key = keyOriginal
	return
}

func (t *taskGet) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	ok, eof, _ := readSingleItem(r, scratchBuf, t.item)
	if !ok {
		return false
	}
	t.found = !eof
	return true
}

// Obtains value (item.Value) and flags (item.Flags) for the given key
// (item.Key) from memcache server.
//
// Returns ErrCacheMiss on cache miss.
func (c *Client) Get(item *Item) error {
	if !validateKey(item.Key) {
		return ErrMalformedKey
	}
	t := taskGet{
		item: item,
	}
	if err := c.do(&t); err != nil {
		return err
	}
	if !t.found {
		return ErrCacheMiss
	}
	return nil
}

// The item for 'conditional set/get' requests - Client.Cget(), Client.Cset(),
// Client.CsetNowait().
type Citem struct {
	Item

	// Etag should uniquely identify the given item.
	Etag uint64

	// Validation time in milliseconds. After this period of time the item
	// shouldn't be returned to the caller without re-validation
	// via Client.Cget().
	ValidateTtl uint32
}

type taskCget struct {
	item        *Citem
	found       bool
	notModified bool
	taskSync
}

func (t *taskCget) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return (writeStr(w, strCget) && writeStr(w, t.item.Key) && writeWs(w) &&
		writeUint64(w, t.item.Etag, scratchBuf) && writeCrLf(w))
}

func readCgetItem(r *bufio.Reader, line []byte, item *Citem) (found, notModified, ok bool) {
	ok = false
	if bytes.Equal(line, strNotFound) {
		found = false
		notModified = false
		ok = true
		return
	}
	if bytes.Equal(line, strNotModified) {
		found = true
		notModified = true
		ok = true
		return
	}
	if !bytes.HasPrefix(line, strValue) {
		log.Printf("Unexpected line read=[%s]. It should start with [%s]", line, strValue)
		return
	}
	line = line[len(strValue):]

	n := -1

	size, ok := parseSizeToken(line, &n)
	if !ok {
		return
	}
	if item.Flags, ok = parseFlagsToken(line, &n); !ok {
		return
	}
	if item.Expiration, ok = parseExpirationToken(line, &n); !ok {
		return
	}
	if item.Etag, ok = parseEtagToken(line, &n); !ok {
		return
	}
	if item.ValidateTtl, ok = parseUint32Token(line, &n, "validateTtl"); !ok {
		return
	}
	if !expectEof(line, n) {
		return
	}
	if item.Value, ok = readValue(r, size); !ok {
		return
	}
	found = true
	notModified = false
	ok = true
	return
}

func (t *taskCget) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	if !readLine(r, scratchBuf) {
		return false
	}
	line := *scratchBuf

	var ok bool
	t.found, t.notModified, ok = readCgetItem(r, line, t.item)
	return ok
}

// Performs conditional get request for the given item.Key and item.Etag.
//
// This is an extension to memcache protocol, so it isn't supported
// by the original memcache server.
//
// Conditional get requests must be performed only on items stored in the cache
// via Client.Cset(). The method returns garbage for items stored via other
// mechanisms.
//
// Fills item.Value, item.Expiration, item.Flags, item.Etag and item.ValidateTtl
// only on cache hit and only if the given etag doesn't match the etag on
// the server, i.e. if the server contains new value for the given key.
//
// Returns ErrCacheMiss on cache miss.
// Returns ErrNotModified if the corresponding item on the server has
// the same etag.
//
// Client.Cset() and Client.Cget() are intended for reducing network bandwidth
// consumption in multi-level caches. They are modelled after HTTP cache
// validation approach with entity tags -
// see http://www.w3.org/Protocols/rfc2616/rfc2616-sec3.html#sec3.11 .
func (c *Client) Cget(item *Citem) error {
	if !validateKey(item.Key) {
		return ErrMalformedKey
	}
	t := taskCget{
		item: item,
	}
	if err := c.do(&t); err != nil {
		return err
	}
	if t.notModified {
		return ErrNotModified
	}
	if !t.found {
		return ErrCacheMiss
	}
	return nil
}

type taskCgetDe struct {
	item          *Citem
	graceDuration time.Duration
	found         bool
	wouldBlock    bool
	notModified   bool
	taskSync
}

func (t *taskCgetDe) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return (writeStr(w, strCgetDe) && writeStr(w, t.item.Key) && writeWs(w) &&
		writeUint64(w, t.item.Etag, scratchBuf) && writeWs(w) &&
		writeMilliseconds(w, t.graceDuration, scratchBuf) && writeCrLf(w))
}

func (t *taskCgetDe) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	if !readLine(r, scratchBuf) {
		return false
	}
	line := *scratchBuf
	if bytes.Equal(line, strWouldBlock) {
		t.found = false
		t.wouldBlock = true
		t.notModified = false
		return true
	}
	t.wouldBlock = false

	var ok bool
	t.found, t.notModified, ok = readCgetItem(r, line, t.item)
	return ok
}

// Combines functionality of Client.Cget() and Client.GetDe().
func (c *Client) CgetDe(item *Citem, graceDuration time.Duration) error {
	if !validateKey(item.Key) {
		return ErrMalformedKey
	}
	for {
		t := taskCgetDe{
			item:          item,
			graceDuration: graceDuration,
		}
		if err := c.do(&t); err != nil {
			return err
		}
		if t.wouldBlock {
			time.Sleep(time.Millisecond * time.Duration(100))
			continue
		}
		if t.notModified {
			return ErrNotModified
		}
		if !t.found {
			return ErrCacheMiss
		}
		return nil
	}
	panic("unreachable")
}

type taskGetDe struct {
	item          *Item
	graceDuration time.Duration
	found         bool
	wouldBlock    bool
	taskSync
}

func (t *taskGetDe) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return (writeStr(w, strGetDe) && writeStr(w, t.item.Key) && writeWs(w) &&
		writeMilliseconds(w, t.graceDuration, scratchBuf) && writeCrLf(w))
}

func (t *taskGetDe) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	ok, eof, wouldBlock := readSingleItem(r, scratchBuf, t.item)
	if !ok {
		return false
	}
	if wouldBlock {
		t.found = true
		t.wouldBlock = true
		return true
	}
	t.found = !eof
	t.wouldBlock = false
	return true
}

// Performs dogpile effect-aware get for the given item.Key.
//
// This is an extension to memcache protocol, so it isn't supported
// by the original memcache server.
//
// Returns ErrCacheMiss on cache miss. It is expected that the caller
// will create and store in the cache an item on cache miss during the given
// graceDuration interval.
func (c *Client) GetDe(item *Item, graceDuration time.Duration) error {
	if !validateKey(item.Key) {
		return ErrMalformedKey
	}
	for {
		t := taskGetDe{
			item:          item,
			graceDuration: graceDuration,
		}
		if err := c.do(&t); err != nil {
			return err
		}
		if t.wouldBlock {
			time.Sleep(time.Millisecond * time.Duration(100))
			continue
		}
		if !t.found {
			return ErrCacheMiss
		}
		return nil
	}
	panic("unreachable")
}

type taskSet struct {
	item *Item
	taskSync
}

func writeSetRequest(w *bufio.Writer, item *Item, noreply bool, scratchBuf *[]byte) bool {
	size := len(item.Value)
	if !writeStr(w, strSet) || !writeStr(w, item.Key) || !writeWs(w) ||
		!writeUint32(w, item.Flags, scratchBuf) || !writeWs(w) ||
		!writeExpiration(w, item.Expiration, scratchBuf) || !writeWs(w) ||
		!writeInt(w, size, scratchBuf) {
		return false
	}
	if noreply {
		if !writeNoreply(w) {
			return false
		}
	}
	return writeCrLf(w) && writeStr(w, item.Value) && writeCrLf(w)
}

func readSetResponse(r *bufio.Reader) bool {
	return matchStr(r, strStored) && matchCrLf(r)
}

func (t *taskSet) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeSetRequest(w, t.item, false, scratchBuf)
}

func (t *taskSet) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	return readSetResponse(r)
}

// Stores the given item in the memcache server.
func (c *Client) Set(item *Item) error {
	if !validateKey(item.Key) {
		return ErrMalformedKey
	}
	if item.Value == nil {
		return ErrNilValue
	}
	t := taskSet{
		item: item,
	}
	return c.do(&t)
}

type taskCset struct {
	item *Citem
	taskSync
}

func writeCsetRequest(w *bufio.Writer, item *Citem, noreply bool, scratchBuf *[]byte) bool {
	size := len(item.Value)
	if !writeStr(w, strCset) || !writeStr(w, item.Key) || !writeWs(w) ||
		!writeInt(w, size, scratchBuf) || !writeWs(w) ||
		!writeUint32(w, item.Flags, scratchBuf) || !writeWs(w) ||
		!writeExpiration(w, item.Expiration, scratchBuf) || !writeWs(w) ||
		!writeUint64(w, item.Etag, scratchBuf) || !writeWs(w) ||
		!writeUint32(w, item.ValidateTtl, scratchBuf) {
		return false
	}
	if noreply {
		if !writeNoreply(w) {
			return false
		}
	}
	return writeCrLf(w) && writeStr(w, item.Value) && writeCrLf(w)
}

func (t *taskCset) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeCsetRequest(w, t.item, false, scratchBuf)
}

func (t *taskCset) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	return readSetResponse(r)
}

// Performs conditional set for the given item.
//
// This is an extension to memcache protocol, so it isn't supported
// by the original memcache server.
//
// Items stored via this method must be obtained only via Client.Cget() call!
// Calls to other methods such as Client.Get() will return garbage
// for item's key.
//
// Client.Cset() and Client.Cget() are intended for reducing network bandwidth
// consumption in multi-level caches. They are modelled after HTTP cache
// validation approach with entity tags -
// see http://www.w3.org/Protocols/rfc2616/rfc2616-sec3.html#sec3.11 .
func (c *Client) Cset(item *Citem) error {
	if !validateKey(item.Key) {
		return ErrMalformedKey
	}
	if item.Value == nil {
		return ErrNilValue
	}
	t := taskCset{
		item: item,
	}
	return c.do(&t)
}

type taskNowait struct{}

func (t *taskNowait) Init() {}

func (t *taskNowait) Done(ok bool) {}

func (t *taskNowait) Wait() bool {
	return true
}

func (t *taskNowait) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	return true
}

type taskSetNowait struct {
	item Item
	taskNowait
}

func (t *taskSetNowait) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeSetRequest(w, &t.item, true, scratchBuf)
}

// The same as Client.Set(), but doesn't wait for operation completion.
//
// Do not modify slices pointed by item.Key and item.Value after passing
// to this function - it actually becomes an owner of these slices.
func (c *Client) SetNowait(item *Item) {
	if !validateKey(item.Key) || item.Value == nil {
		return
	}
	t := taskSetNowait{
		item: *item,
	}
	c.do(&t)
}

type taskCsetNowait struct {
	item Citem
	taskNowait
}

func (t *taskCsetNowait) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeCsetRequest(w, &t.item, true, scratchBuf)
}

// The same as Client.Cset(), but doesn't wait for operation completion.
//
// Do not modify slices pointed by item.Key and item.Value after passing
// to this function - it actually becomes an owner of these slices.
func (c *Client) CsetNowait(item *Citem) {
	if !validateKey(item.Key) || item.Value == nil {
		return
	}
	t := taskCsetNowait{
		item: *item,
	}
	c.do(&t)
}

type taskDelete struct {
	key         []byte
	itemDeleted bool
	taskSync
}

func writeDeleteRequest(w *bufio.Writer, key []byte, noreply bool) bool {
	if !writeStr(w, strDelete) || !writeStr(w, key) {
		return false
	}
	if noreply {
		if !writeNoreply(w) {
			return false
		}
	}
	return writeCrLf(w)
}

func (t *taskDelete) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeDeleteRequest(w, t.key, false)
}

func (t *taskDelete) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	if !readLine(r, scratchBuf) {
		return false
	}
	line := *scratchBuf
	if bytes.Equal(line, strDeleted) {
		t.itemDeleted = true
		return true
	}
	if bytes.Equal(line, strNotFound) {
		t.itemDeleted = false
		return true
	}
	log.Printf("Unexpected response for 'delete' request: [%s]", line)
	return false
}

// Deletes an item with the given key from memcache server.
//
// Returns ErrCacheMiss if there were no item with such key
// on the server.
func (c *Client) Delete(key []byte) error {
	if !validateKey(key) {
		return ErrMalformedKey
	}
	t := taskDelete{
		key: key,
	}
	if err := c.do(&t); err != nil {
		return err
	}
	if !t.itemDeleted {
		return ErrCacheMiss
	}
	return nil
}

type taskDeleteNowait struct {
	key []byte
	taskNowait
}

func (t *taskDeleteNowait) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeDeleteRequest(w, t.key, true)
}

// The same as Client.Delete(), but doesn't wait for operation completion.
//
// Do not modify slice pointed by key after passing to this function -
// it actually becomes an owner of this slice.
func (c *Client) DeleteNowait(key []byte) {
	if !validateKey(key) {
		return
	}
	t := taskDeleteNowait{
		key: key,
	}
	c.do(&t)
}

type taskFlushAllDelayed struct {
	expiration time.Duration
	taskSync
}

func (t *taskFlushAllDelayed) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeStr(w, strFlushAll) && writeExpiration(w, t.expiration, scratchBuf) && writeCrLf(w)
}

func (t *taskFlushAllDelayed) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	return matchStr(r, strOkCrLf)
}

// Flushes all the items on the server after the given expiration delay.
func (c *Client) FlushAllDelayed(expiration time.Duration) error {
	t := taskFlushAllDelayed{
		expiration: expiration,
	}
	return c.do(&t)
}

type taskFlushAll struct {
	taskSync
}

func (t *taskFlushAll) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeStr(w, strFlushAll) && writeCrLf(w)
}

func (t *taskFlushAll) ReadResponse(r *bufio.Reader, scratchBuf *[]byte) bool {
	return matchStr(r, strOkCrLf)
}

// Flushes all the items on the server.
func (c *Client) FlushAll() error {
	t := taskFlushAll{}
	return c.do(&t)
}

type taskFlushAllDelayedNowait struct {
	expiration time.Duration
	taskNowait
}

func (t *taskFlushAllDelayedNowait) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeStr(w, strFlushAll) && writeExpiration(w, t.expiration, scratchBuf) && writeWs(w) &&
		writeStr(w, strNoreply) && writeCrLf(w)
}

// The same as Client.FlushAllDelayed(), but doesn't wait for operation
// completion.
func (c *Client) FlushAllDelayedNowait(expiration time.Duration) {
	t := taskFlushAllDelayedNowait{
		expiration: expiration,
	}
	c.do(&t)
}

type taskFlushAllNowait struct {
	taskNowait
}

func (t *taskFlushAllNowait) WriteRequest(w *bufio.Writer, scratchBuf *[]byte) bool {
	return writeStr(w, strFlushAll) && writeStr(w, strNoreply) && writeCrLf(w)
}

// The same as Client.FlushAll(), but doesn't wait for operation completion.
func (c *Client) FlushAllNowait() {
	t := taskFlushAllNowait{}
	c.do(&t)
}
