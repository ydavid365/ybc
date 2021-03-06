package memcache

import (
	"bytes"
	"fmt"
	"github.com/valyala/ybc/bindings/go/ybc"
	"testing"
	"time"
)

const (
	testAddr = "localhost:12345"
)

func newCache(t *testing.T) *ybc.Cache {
	config := ybc.Config{
		MaxItemsCount: 1000 * 1000,
		DataFileSize:  10 * 1000 * 1000,
	}

	cache, err := config.OpenCache(true)
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func newServerCacheWithAddr(listenAddr string, t *testing.T) (s *Server, cache *ybc.Cache) {
	cache = newCache(t)
	s = &Server{
		Cache:      cache,
		ListenAddr: listenAddr,
	}
	return
}

func newServerCache(t *testing.T) (s *Server, cache *ybc.Cache) {
	return newServerCacheWithAddr(testAddr, t)
}

func TestServer_StartStop(t *testing.T) {
	s, cache := newServerCache(t)
	defer cache.Close()
	s.Start()
	s.Stop()
}

func TestServer_StartStop_Multi(t *testing.T) {
	s, cache := newServerCache(t)
	defer cache.Close()
	for i := 0; i < 3; i++ {
		s.Start()
		s.Stop()
	}
}

func TestServer_Serve(t *testing.T) {
	s, cache := newServerCache(t)
	defer cache.Close()
	go func() {
		time.Sleep(time.Millisecond * time.Duration(100))
		s.Stop()
	}()
	s.Serve()
}

func TestServer_Wait(t *testing.T) {
	s, cache := newServerCache(t)
	defer cache.Close()
	go func() {
		time.Sleep(time.Millisecond * time.Duration(100))
		s.Stop()
	}()
	s.Start()
	s.Wait()
}

func newClientServerCache(t *testing.T) (c *Client, s *Server, cache *ybc.Cache) {
	c = &Client{
		ServerAddr:       testAddr,
		ConnectionsCount: 1, // tests require single connection!
	}
	s, cache = newServerCache(t)
	s.Start()
	return
}

func cacher_StartStop(c Cacher) {
	c.Start()
	c.Stop()
}

func TestClient_StartStop(t *testing.T) {
	c, s, cache := newClientServerCache(t)
	defer cache.Close()
	defer s.Stop()

	cacher_StartStop(c)
}

func cacher_StartStop_Multi(c Cacher) {
	for i := 0; i < 3; i++ {
		c.Start()
		c.Stop()
	}
}

func TestClient_StartStop_Multi(t *testing.T) {
	c, s, cache := newClientServerCache(t)
	defer cache.Close()
	defer s.Stop()

	cacher_StartStop_Multi(c)
}

func expectPanic(t *testing.T, f func()) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("unexpected empty panic message for the function [%s]", f)
		}
	}()
	f()
	t.Fatalf("the function [%s] must panic!", f)
}

func cacher_StopWithoutStart(c Cacher, t *testing.T) {
	expectPanic(t, func() { c.Stop() })
}

func TestClient_StopWithoutStart(t *testing.T) {
	c, s, cache := newClientServerCache(t)
	defer cache.Close()
	defer s.Stop()

	cacher_StopWithoutStart(c, t)
}

func cacher_GetSet(c Cacher, t *testing.T) {
	key := []byte("key")
	value := []byte("value")
	flags := uint32(12345)

	item := Item{
		Key: key,
	}
	if err := c.Get(&item); err != ErrCacheMiss {
		t.Fatalf("Unexpected err=[%s] for client.Get(%s)", err, key)
	}

	item.Value = value
	item.Flags = flags
	if err := c.Set(&item); err != nil {
		t.Fatalf("error in client.Set(): [%s]", err)
	}
	item.Value = nil
	item.Flags = 0
	if err := c.Get(&item); err != nil {
		t.Fatalf("cannot obtain value for key=[%s] from memcache: [%s]", key, err)
	}
	if !bytes.Equal(item.Value, value) {
		t.Fatalf("invalid value=[%s] returned. Expected [%s]", item.Value, value)
	}
	if item.Flags != flags {
		t.Fatalf("invalid flags=[%d] returned. Expected [%d]", item.Flags, flags)
	}
}

type cacherTestFunc func(c Cacher, t *testing.T)

func client_RunTest(testFunc cacherTestFunc, t *testing.T) {
	c, s, cache := newClientServerCache(t)
	defer cache.Close()
	defer s.Stop()
	c.Start()
	defer c.Stop()

	testFunc(c, t)
}

func TestClient_GetSet(t *testing.T) {
	client_RunTest(cacher_GetSet, t)
}

func cacher_GetDe(c Cacher, t *testing.T) {
	item := Item{
		Key: []byte("key"),
	}
	grace := 100 * time.Millisecond
	for i := 0; i < 3; i++ {
		if err := c.GetDe(&item, grace); err != ErrCacheMiss {
			t.Fatalf("Unexpected err=[%s] for client.GetDe(key=%s, grace=%s)", err, item.Key, grace)
		}
	}

	item.Value = []byte("value")
	item.Flags = 123
	if err := c.Set(&item); err != nil {
		t.Fatalf("Cannot set value=[%s] for key=[%s]: [%s]", item.Value, item.Key, err)
	}
	oldValue := item.Value
	oldFlags := item.Flags
	item.Value = nil
	item.Flags = 0
	if err := c.GetDe(&item, grace); err != nil {
		t.Fatalf("Cannot obtain value fro key=[%s]: [%s]", item.Key, err)
	}
	if !bytes.Equal(oldValue, item.Value) {
		t.Fatalf("Unexpected value obtained: [%s]. Expected [%s]", item.Value, oldValue)
	}
	if oldFlags != item.Flags {
		t.Fatalf("Unexpected flags obtained: [%d]. Expected [%d]", item.Flags, oldFlags)
	}
}

func TestClient_GetDe(t *testing.T) {
	client_RunTest(cacher_GetDe, t)
}

func cacher_CgetCset(c Cacher, t *testing.T) {
	key := []byte("key")
	value := []byte("value")
	expiration := time.Hour * 123343
	flags := uint32(892379)

	etag := uint64(1234567890)
	validateTtl := uint32(98765432)
	item := Citem{
		Item: Item{
			Key:        key,
			Value:      value,
			Expiration: expiration,
			Flags:      flags,
		},
		Etag:        etag,
		ValidateTtl: validateTtl,
	}

	if err := c.Cget(&item); err != ErrCacheMiss {
		t.Fatalf("Unexpected error returned from Client.Cget(): [%s]. Expected ErrCacheMiss", err)
	}

	if err := c.Cset(&item); err != nil {
		t.Fatalf("Error in Client.Cset(): [%s]", err)
	}

	if err := c.Cget(&item); err != ErrNotModified {
		t.Fatalf("Unexpected error returned from Client.Cget(): [%s]. Expected ErrNotModified", err)
	}

	item.Value = nil
	item.Expiration = expiration + time.Hour
	item.Flags = 0
	item.Etag = 0
	if err := c.Cget(&item); err != nil {
		t.Fatalf("Unexpected error returned from Client.Cget(): [%s]", err)
	}
	if item.Flags != flags {
		t.Fatalf("Unexpected flags=[%d] returned from Client.Cget(). Expected [%s]", item.Flags, flags)
	}
	if item.Etag != etag {
		t.Fatalf("Unexpected etag=[%d] returned from Client.Cget(). Expected [%d]", item.Etag, etag)
	}
	if item.ValidateTtl != validateTtl {
		t.Fatalf("Unexpected validateTtl=[%d] returned from Client.Cget(). Expected [%d]", item.ValidateTtl, validateTtl)
	}
	if !bytes.Equal(item.Value, value) {
		t.Fatalf("Unexpected value=[%s] returned from Client.Cget(). Expected [%d]", item.Value, value)
	}
	if item.Expiration > expiration {
		t.Fatalf("Unexpected expiration=[%d] returned from Client.Cget(). Expected not more than [%d]", item.Expiration, expiration)
	}
}

func TestClient_CgetCset(t *testing.T) {
	client_RunTest(cacher_CgetCset, t)
}

func cacher_CgetDe(c Cacher, t *testing.T) {
	item := Citem{
		Item: Item{
			Key: []byte("key"),
		},
	}
	grace := 100 * time.Millisecond
	for i := 0; i < 3; i++ {
		if err := c.CgetDe(&item, grace); err != ErrCacheMiss {
			t.Fatalf("Unexpected err=[%s] for client.CgetDe(key=[%s], grace=[%s])", err, item.Key, grace)
		}
	}

	value := []byte("value")
	flags := uint32(123)
	etag := uint64(8989)
	validateTtl := uint32(87989)

	item.Value = value
	item.Flags = flags
	item.Etag = etag
	item.ValidateTtl = validateTtl
	if err := c.Cset(&item); err != nil {
		t.Fatalf("Cannot set value=[%s] for key=[%s]: [%s]", item.Value, item.Key, err)
	}
	if err := c.CgetDe(&item, grace); err != ErrNotModified {
		t.Fatalf("Unexpected err=[%s] for client.CgetDe(key=[%s], grace=[%s]). Expected ErrNotModified", err, item.Key, grace)
	}

	item.Value = nil
	item.Flags = 0
	item.Etag = 0
	item.ValidateTtl = 0
	if err := c.CgetDe(&item, grace); err != nil {
		t.Fatalf("Error in client.CgetDe(key=[%s], grace=[%s]): [%s]", item.Key, grace, err)
	}
	if !bytes.Equal(item.Value, value) {
		t.Fatalf("Unexpected value obtained: [%s]. Expected [%s]", item.Value, value)
	}
	if item.Flags != flags {
		t.Fatalf("Unexpected flags obtained: [%d]. Expected [%d]", item.Flags, flags)
	}
	if item.Etag != etag {
		t.Fatalf("Unexpected etag obtianed: [%d]. Expected [%d]", item.Etag, etag)
	}
}

func TestClient_CgetDe(t *testing.T) {
	client_RunTest(cacher_CgetDe, t)
}

func lookupItem(items []Item, key []byte) *Item {
	for i := 0; i < len(items); i++ {
		if bytes.Equal(items[i].Key, key) {
			return &items[i]
		}
	}
	return nil
}

func checkItems(c Cacher, orig_items []Item, t *testing.T) {
	items := make([]Item, len(orig_items))
	copy(items, orig_items)
	if err := c.GetMulti(orig_items); err != nil {
		t.Fatalf("Error in client.GetMulti(): [%s]", err)
	}
	for _, item := range items {
		orig_item := lookupItem(orig_items, item.Key)
		if orig_item == nil {
			t.Fatalf("Cannot find original item with key=[%s]", item.Key)
		}
		if !bytes.Equal(item.Value, orig_item.Value) {
			t.Fatalf("Values mismatch for key=[%s]. Returned=[%s], expected=[%s]", item.Key, item.Value, orig_item.Value)
		}
	}
}

func checkCItems(c Cacher, items []Citem, t *testing.T) {
	for i := 0; i < len(items); i++ {
		item := items[i]
		err := c.Cget(&item)
		if err == ErrCacheMiss {
			continue
		}
		if err != ErrNotModified {
			t.Fatalf("Unexpected error returned from Client.Cget(): [%s]. Expected ErrNotModified", err)
		}

		item.Flags = 0
		item.Etag++
		item.ValidateTtl = 0
		if err := c.Cget(&item); err != nil {
			t.Fatalf("Error when calling Client.Cget(): [%s]", err)
		}
		if item.Flags != items[i].Flags {
			t.Fatalf("Unexpected flags=%d returned. Expected %d", item.Flags, items[i].Flags)
		}
		if item.Etag != items[i].Etag {
			t.Fatalf("Unexpected etag=%d returned. Expected %d", item.Etag, items[i].Etag)
		}
		if item.ValidateTtl != items[i].ValidateTtl {
			t.Fatalf("Unexpected validateTtl=%d returned. Expected %d", item.ValidateTtl, items[i].ValidateTtl)
		}
		if !bytes.Equal(item.Value, items[i].Value) {
			t.Fatalf("Unexpected value=[%s] returned. Expected [%s]", item.Value, items[i].Value)
		}
	}
}

func cacher_GetMulti_EmptyItems(c Cacher, t *testing.T) {
	if err := c.GetMulti([]Item{}); err != nil {
		t.Fatalf("Unexpected error in client.GetMulti(): [%s]", err)
	}
}

func TestClient_GetMulti_EmptyItems(t *testing.T) {
	client_RunTest(cacher_GetMulti_EmptyItems, t)
}

func cacher_GetMulti(c Cacher, t *testing.T) {
	itemsCount := 100
	items := make([]Item, itemsCount)
	for i := 0; i < itemsCount; i++ {
		item := &items[i]
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		item.Value = []byte(fmt.Sprintf("value_%d", i))
		if err := c.Set(item); err != nil {
			t.Fatalf("error in client.Set(): [%s]", err)
		}
	}

	checkItems(c, items, t)
}

func TestClient_GetMulti(t *testing.T) {
	client_RunTest(cacher_GetMulti, t)
}

func cacher_SetNowait(c Cacher, t *testing.T) {
	itemsCount := 100
	items := make([]Item, itemsCount)
	for i := 0; i < itemsCount; i++ {
		item := &items[i]
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		item.Value = []byte(fmt.Sprintf("value_%d", i))
		c.SetNowait(item)
	}

	checkItems(c, items, t)
}

func TestClient_SetNowait(t *testing.T) {
	client_RunTest(cacher_SetNowait, t)
}

func cacher_CsetNowait(c Cacher, t *testing.T) {
	itemsCount := 100
	items := make([]Citem, itemsCount)
	for i := 0; i < itemsCount; i++ {
		item := &items[i]
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		item.Value = []byte(fmt.Sprintf("value_%d", i))
		item.Flags = uint32(i)
		item.Etag = uint64(i + 1)
		item.ValidateTtl = uint32(i + 2)
		c.CsetNowait(item)
	}

	checkCItems(c, items, t)
}

func TestClient_CsetNowait(t *testing.T) {
	client_RunTest(cacher_CsetNowait, t)
}

func cacher_Delete(c Cacher, t *testing.T) {
	itemsCount := 100
	var item Item
	for i := 0; i < itemsCount; i++ {
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		item.Value = []byte(fmt.Sprintf("value_%d", i))
		if err := c.Delete(item.Key); err != ErrCacheMiss {
			t.Fatalf("error when deleting non-existing item: [%s]", err)
		}
		if err := c.Set(&item); err != nil {
			t.Fatalf("error in client.Set(): [%s]", err)
		}
		if err := c.Delete(item.Key); err != nil {
			t.Fatalf("error when deleting existing item: [%s]", err)
		}
		if err := c.Delete(item.Key); err != ErrCacheMiss {
			t.Fatalf("error when deleting non-existing item: [%s]", err)
		}
	}
}

func TestClient_Delete(t *testing.T) {
	client_RunTest(cacher_Delete, t)
}

func cacher_DeleteNowait(c Cacher, t *testing.T) {
	itemsCount := 100
	var item Item
	for i := 0; i < itemsCount; i++ {
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		item.Value = []byte(fmt.Sprintf("value_%d", i))
		if err := c.Set(&item); err != nil {
			t.Fatalf("error in client.Set(): [%s]", err)
		}
	}
	for i := 0; i < itemsCount; i++ {
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		c.DeleteNowait(item.Key)
	}
	for i := 0; i < itemsCount; i++ {
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		if err := c.Get(&item); err != ErrCacheMiss {
			t.Fatalf("error when obtaining deleted item for key=[%s]: [%s]", item.Key, err)
		}
	}
}

func TestClient_DeleteNowait(t *testing.T) {
	client_RunTest(cacher_DeleteNowait, t)
}

func cacher_FlushAll(c Cacher, t *testing.T) {
	itemsCount := 100
	var item Item
	for i := 0; i < itemsCount; i++ {
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		item.Value = []byte(fmt.Sprintf("value_%d", i))
		if err := c.Set(&item); err != nil {
			t.Fatalf("error in client.Set(): [%s]", err)
		}
	}
	c.FlushAllNowait()
	c.FlushAll()
	for i := 0; i < itemsCount; i++ {
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		if err := c.Get(&item); err != ErrCacheMiss {
			t.Fatalf("error when obtaining deleted item: [%s]", err)
		}
	}
}

func TestClient_FlushAll(t *testing.T) {
	client_RunTest(cacher_FlushAll, t)
}

func cacher_FlushAllDelayed(c Cacher, t *testing.T) {
	itemsCount := 100
	var item Item
	for i := 0; i < itemsCount; i++ {
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		item.Value = []byte(fmt.Sprintf("value_%d", i))
		if err := c.Set(&item); err != nil {
			t.Fatalf("error in client.Set(): [%s]", err)
		}
	}
	c.FlushAllDelayedNowait(time.Second)
	c.FlushAllDelayed(time.Second)
	foundItems := 0
	for i := 0; i < itemsCount; i++ {
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		err := c.Get(&item)
		if err == ErrCacheMiss {
			continue
		}
		if err != nil {
			t.Fatalf("error when obtaining item: [%s]", err)
		}
		foundItems++
	}
	if foundItems == 0 {
		t.Fatalf("It seems all the %d items are already delayed", itemsCount)
	}

	time.Sleep(time.Second * 2)
	for i := 0; i < itemsCount; i++ {
		item.Key = []byte(fmt.Sprintf("key_%d", i))
		if err := c.Get(&item); err != ErrCacheMiss {
			t.Fatalf("error when obtaining deleted item: [%s]", err)
		}
	}
}

func TestClient_FlushAllDelayed(t *testing.T) {
	client_RunTest(cacher_FlushAllDelayed, t)
}

func checkMalformedKey(c Cacher, key []byte, t *testing.T) {
	item := Item{
		Key: key,
	}
	if err := c.Get(&item); err != ErrMalformedKey {
		t.Fatalf("Unexpected err=[%s] returned. Expected ErrMalformedKey", err)
	}
	if err := c.GetDe(&item, time.Second); err != ErrMalformedKey {
		t.Fatalf("Unexpected err=[%s] returned. Expected ErrMalformedKey", err)
	}
	if err := c.Set(&item); err != ErrMalformedKey {
		t.Fatalf("Unexpected err=[%s] returned. Expected ErrMalformedKey", err)
	}
	if err := c.Delete(item.Key); err != ErrMalformedKey {
		t.Fatalf("Unexpected err=[%s] returned. Expected ErrMalformedKey", err)
	}

	citem := Citem{
		Item: item,
	}
	if err := c.Cget(&citem); err != ErrMalformedKey {
		t.Fatalf("Unexpected err=[%s] returned. Expected ErrMalformedKey", err)
	}
	if err := c.Cset(&citem); err != ErrMalformedKey {
		t.Fatalf("Unexpected err=[%s] returned. Expected ErrMalformedKey", err)
	}
}

func cacher_MalformedKey(c Cacher, t *testing.T) {
	checkMalformedKey(c, nil, t)
	checkMalformedKey(c, []byte{}, t)
	checkMalformedKey(c, []byte("malformed key with spaces"), t)
	checkMalformedKey(c, []byte("malformed\nkey\nwith\nnewlines"), t)
}

func TestClient_MalformedKey(t *testing.T) {
	client_RunTest(cacher_MalformedKey, t)
}

func cacher_NilValue(c Cacher, t *testing.T) {
	item := Item{
		Key: []byte("test"),
	}
	if err := c.Set(&item); err != ErrNilValue {
		t.Fatalf("Unexpected err=[%s] returned. Expected ErrNilValue", err)
	}

	citem := Citem{
		Item: item,
	}
	if err := c.Cset(&citem); err != ErrNilValue {
		t.Fatalf("Unexpected err=[%s] returned. Expected ErrNilValue", err)
	}
}

func TestClient_NilValue(t *testing.T) {
	client_RunTest(cacher_NilValue, t)
}

func cacher_EmptyValue(c Cacher, t *testing.T) {
	flags := uint32(89832)
	item := Item{
		Key:   []byte("test"),
		Value: []byte{},
		Flags: flags,
	}
	if err := c.Set(&item); err != nil {
		t.Fatalf("Cannot set item with empty value: [%s]", err)
	}
	item.Value = nil
	if err := c.Get(&item); err != nil {
		t.Fatalf("Error when obtaining empty value: [%s]", err)
	}
	if item.Value == nil || len(item.Value) != 0 {
		t.Fatalf("Unexpected value obtained=[%s]. Expected empty value", item.Value)
	}
	if item.Flags != flags {
		t.Fatalf("Unexpected Flags obtained=[%s]. Expected [%d]", item.Flags, flags)
	}

	etag := uint64(1234)
	validateTtl := uint32(1000)
	citem := Citem{
		Item:        item,
		Etag:        etag,
		ValidateTtl: validateTtl,
	}
	if err := c.Cset(&citem); err != nil {
		t.Fatalf("Cannot set item with empty value: [%s]", err)
	}
	citem.Value = nil
	citem.Flags = 0
	citem.Etag = 0
	citem.ValidateTtl = 0
	if err := c.Cget(&citem); err != nil {
		t.Fatalf("Error when obtaining empty value: [%s]", err)
	}
	if citem.Value == nil || len(citem.Value) != 0 {
		t.Fatalf("Unexpected value obtained=[%s]. Expected empty value", citem.Value)
	}
	if citem.Flags != flags {
		t.Fatalf("Unexpected flags obtained=[%d]. Expected [%d]", citem.Flags, flags)
	}
	if citem.Etag != etag {
		t.Fatalf("Unexpected etag obtained=[%d]. Expected [%d]", citem.Etag, etag)
	}
	if citem.ValidateTtl != validateTtl {
		t.Fatalf("Unexpected validateTtl obtained=[%s]. Expected [%s]", citem.ValidateTtl, validateTtl)
	}
}

func TestClient_EmptyValue(t *testing.T) {
	client_RunTest(cacher_EmptyValue, t)
}

func cacher_NotStartedNoStop(c Cacher, t *testing.T) {
	item := Item{
		Key:   []byte("key"),
		Value: []byte("value"),
	}
	citem := Citem{
		Item: item,
	}

	if err := c.Get(&item); err != ErrClientNotStarted {
		t.Fatalf("Unexpected error received in Cacher.Get(): [%s]. Expected ErrClientNotStarted", err)
	}
	if err := c.GetMulti([]Item{item}); err != ErrClientNotStarted {
		t.Fatalf("Unexpected error received in Cacher.GetMulti(): [%s]. Expected ErrClientNotStarted", err)
	}
	if err := c.GetDe(&item, time.Second); err != ErrClientNotStarted {
		t.Fatalf("Unexpected error received in Cacher.GetDe(): [%s]. Expected ErrClientNotStarted", err)
	}
	if err := c.Cget(&citem); err != ErrClientNotStarted {
		t.Fatalf("Unexpected error received in Cacher.Cget(): [%s]. Expected ErrClientNotStarted", err)
	}
	if err := c.Set(&item); err != ErrClientNotStarted {
		t.Fatalf("Unexpected error received in Cacher.Set(): [%s]. Expected ErrClientNotStarted", err)
	}
	if err := c.Cset(&citem); err != ErrClientNotStarted {
		t.Fatalf("Unexpected error received in Cacher.Cset(): [%s]. Expected ErrClientNotStarted", err)
	}
	if err := c.Delete(item.Key); err != ErrClientNotStarted {
		t.Fatalf("Unexpected error received in Cacher.Delete(): [%s]. Expected ErrClientNotStarted", err)
	}
	if err := c.FlushAll(); err != ErrClientNotStarted {
		t.Fatalf("Unexpected error received in Cacher.FlushAll(): [%s]. Expected ErrClientNotStarted", err)
	}
	if err := c.FlushAllDelayed(time.Second); err != ErrClientNotStarted {
		t.Fatalf("Unexpected error received in Cacher.FlushAllDelayed(): [%s]. Expected ErrClientNotStarted", err)
	}
}

func cacher_NotStarted(c Cacher, t *testing.T) {
	c.Stop()
	defer c.Start()
	cacher_NotStartedNoStop(c, t)
}

func TestClient_NotStarted(t *testing.T) {
	client_RunTest(cacher_NotStarted, t)

	c, s, cache := newClientServerCache(t)
	defer cache.Close()
	defer s.Stop()
	cacher_NotStartedNoStop(c, t)
}

func cacher_DoubleStartDoubleStop(c Cacher, t *testing.T) {
	expectPanic(t, func() { c.Start() })

	c.Stop()
	defer c.Start()
	expectPanic(t, func() { c.Stop() })
}

func TestClient_DoubleStartDoubleStop(t *testing.T) {
	client_RunTest(cacher_DoubleStartDoubleStop, t)
}

func TestDistributedClient_NoServers(t *testing.T) {
	c := DistributedClient{}
	c.Start()
	defer c.Stop()

	item := Item{
		Key:        []byte("key"),
		Value:      []byte("value"),
		Expiration: time.Second,
	}
	citem := Citem{
		Item:        item,
		Etag:        12345,
		ValidateTtl: 1328,
	}
	if err := c.Get(&item); err != ErrNoServers {
		t.Fatalf("Get() should return ErrNoServers, but returned [%s]", err)
	}
	if err := c.GetMulti([]Item{item}); err != ErrNoServers {
		t.Fatalf("GetMulti() should return ErrNoServers, but returned [%s]", err)
	}
	if err := c.GetDe(&item, time.Second); err != ErrNoServers {
		t.Fatalf("GetDe() should return ErrNoServers, but returned [%s]", err)
	}
	if err := c.Cget(&citem); err != ErrNoServers {
		t.Fatalf("Cget() should return ErrNoServers, but returned [%s]", err)
	}
	if err := c.Set(&item); err != ErrNoServers {
		t.Fatalf("Set() should return ErrNoServers, but returned [%s]", err)
	}
	if err := c.Cset(&citem); err != ErrNoServers {
		t.Fatalf("Cset() should return ErrNoServers, but returned [%s]", err)
	}
	if err := c.Delete(item.Key); err != ErrNoServers {
		t.Fatalf("Delete() should return ErrNoServers, but returned [%s]", err)
	}
	if err := c.FlushAll(); err != ErrNoServers {
		t.Fatalf("FlushAll() should return ErrNoServers, but returned [%s]", err)
	}
	if err := c.FlushAllDelayed(time.Second); err != ErrNoServers {
		t.Fatalf("FlushAllDelayed() should return ErrNoServers, but returned [%s]", err)
	}
}

func newDistributedClientServersCaches(t *testing.T) (c *DistributedClient, ss []*Server, caches []*ybc.Cache) {
	c = &DistributedClient{
		ConnectionsCount: 1, // tests require single connection!
	}
	for i := 0; i < 4; i++ {
		serverAddr := fmt.Sprintf("localhost:%d", 12345+i)
		s, cache := newServerCacheWithAddr(serverAddr, t)
		s.Start()
		ss = append(ss, s)
		caches = append(caches, cache)
	}
	return
}

func closeCaches(caches []*ybc.Cache) {
	for _, cache := range caches {
		cache.Close()
	}
}

func stopServers(servers []*Server) {
	for _, server := range servers {
		server.Stop()
	}
}

func TestDistibutedClient_StartStop(t *testing.T) {
	c, ss, caches := newDistributedClientServersCaches(t)
	defer closeCaches(caches)
	defer stopServers(ss)

	cacher_StartStop(c)
}

func TestDistributedClient_StartStaticStop_Multi(t *testing.T) {
	c, ss, caches := newDistributedClientServersCaches(t)
	defer closeCaches(caches)
	defer stopServers(ss)

	serverAddrs := make([]string, len(ss))
	for i, s := range ss {
		serverAddrs[i] = s.ListenAddr
	}
	for i := 0; i < 10; i++ {
		c.StartStatic(serverAddrs)
		c.Stop()
	}
}

func TestDistributedClient_StaticAddRemoveServer(t *testing.T) {
	c, ss, caches := newDistributedClientServersCaches(t)
	defer closeCaches(caches)
	defer stopServers(ss)

	serverAddr := ss[0].ListenAddr
	expectPanic(t, func() { c.AddServer(serverAddr) })
	expectPanic(t, func() { c.DeleteServer(serverAddr) })
}

func TestDistibutedClient_AddDeleteServer(t *testing.T) {
	c, ss, caches := newDistributedClientServersCaches(t)
	defer closeCaches(caches)
	defer stopServers(ss)
	c.Start()
	defer c.Stop()

	for _, s := range ss {
		c.AddServer(s.ListenAddr)
	}
	for _, s := range ss {
		c.DeleteServer(s.ListenAddr)
	}
}

func TestDistributedClient_StartStop_Multi(t *testing.T) {
	c, ss, caches := newDistributedClientServersCaches(t)
	defer closeCaches(caches)
	defer stopServers(ss)

	cacher_StartStop_Multi(c)
}

func TestDistributedClient_StopWithoutStart(t *testing.T) {
	c, ss, caches := newDistributedClientServersCaches(t)
	defer closeCaches(caches)
	defer stopServers(ss)

	cacher_StopWithoutStart(c, t)
}

func distributedClient_RunTest(testFunc cacherTestFunc, t *testing.T) {
	c, ss, caches := newDistributedClientServersCaches(t)
	defer closeCaches(caches)
	defer stopServers(ss)
	c.Start()
	defer c.Stop()
	for _, s := range ss {
		c.AddServer(s.ListenAddr)
	}

	testFunc(c, t)
}

func distributedClientStatic_RunTest(testFunc cacherTestFunc, t *testing.T) {
	c, ss, caches := newDistributedClientServersCaches(t)
	defer closeCaches(caches)
	defer stopServers(ss)
	serverAddrs := make([]string, len(ss))
	for i, s := range ss {
		serverAddrs[i] = s.ListenAddr
	}
	c.StartStatic(serverAddrs)
	defer c.Stop()

	testFunc(c, t)
}

func TestDistributedClient_GetSet(t *testing.T) {
	distributedClient_RunTest(cacher_GetSet, t)
	distributedClientStatic_RunTest(cacher_GetSet, t)
}

func TestDistributedClient_GetDe(t *testing.T) {
	distributedClient_RunTest(cacher_GetDe, t)
	distributedClientStatic_RunTest(cacher_GetDe, t)
}

func TestDistributedClient_CgetCset(t *testing.T) {
	distributedClient_RunTest(cacher_CgetCset, t)
	distributedClientStatic_RunTest(cacher_CgetCset, t)
}

func TestDistributedClient_CgetDe(t *testing.T) {
	distributedClient_RunTest(cacher_CgetDe, t)
	distributedClientStatic_RunTest(cacher_CgetDe, t)
}

func TestDistributedClient_GetMulti_EmptyItems(t *testing.T) {
	distributedClient_RunTest(cacher_GetMulti_EmptyItems, t)
	distributedClientStatic_RunTest(cacher_GetMulti_EmptyItems, t)
}

func TestDistributedClient_GetMulti(t *testing.T) {
	distributedClient_RunTest(cacher_GetMulti, t)
	distributedClientStatic_RunTest(cacher_GetMulti, t)
}

func TestDistributedClient_SetNowait(t *testing.T) {
	distributedClient_RunTest(cacher_SetNowait, t)
	distributedClientStatic_RunTest(cacher_SetNowait, t)
}

func TestDistributedClient_CsetNowait(t *testing.T) {
	distributedClient_RunTest(cacher_CsetNowait, t)
	distributedClientStatic_RunTest(cacher_CsetNowait, t)
}

func TestDistributedClient_Delete(t *testing.T) {
	distributedClient_RunTest(cacher_Delete, t)
	distributedClientStatic_RunTest(cacher_Delete, t)
}

func TestDistributedClient_DeleteNowait(t *testing.T) {
	distributedClient_RunTest(cacher_DeleteNowait, t)
	distributedClientStatic_RunTest(cacher_DeleteNowait, t)
}

func TestDistributedClient_FlushAll(t *testing.T) {
	distributedClient_RunTest(cacher_FlushAll, t)
	distributedClientStatic_RunTest(cacher_FlushAll, t)
}

func TestDistributedClient_FlushAllDelayed(t *testing.T) {
	distributedClient_RunTest(cacher_FlushAllDelayed, t)
	distributedClientStatic_RunTest(cacher_FlushAllDelayed, t)
}

func TestDistributedClient_MalformedKey(t *testing.T) {
	distributedClient_RunTest(cacher_MalformedKey, t)
	distributedClientStatic_RunTest(cacher_MalformedKey, t)
}

func TestDistributedClient_NilValue(t *testing.T) {
	distributedClient_RunTest(cacher_NilValue, t)
	distributedClientStatic_RunTest(cacher_NilValue, t)
}

func TestDistributedClient_EmptyValue(t *testing.T) {
	distributedClient_RunTest(cacher_EmptyValue, t)
	distributedClientStatic_RunTest(cacher_EmptyValue, t)
}

func TestDistributedClient_NotStarted(t *testing.T) {
	distributedClient_RunTest(cacher_NotStarted, t)
	distributedClientStatic_RunTest(cacher_NotStarted, t)

	c, ss, caches := newDistributedClientServersCaches(t)
	defer closeCaches(caches)
	defer stopServers(ss)
	cacher_NotStartedNoStop(c, t)
}

func TestDistributedClient_DoubleStartDoubleStop(t *testing.T) {
	distributedClient_RunTest(cacher_DoubleStartDoubleStop, t)
	distributedClientStatic_RunTest(cacher_DoubleStartDoubleStop, t)
}
