// Copyright (c) 2013-2016 The btcsuite developers
// Copyright (c) 2015-2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package addrmgr

import (
	"container/list"
	crand "crypto/rand" // for seeding
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	jsoniter "github.com/json-iterator/go"

	"github.com/pkt-cash/PKT-FullNode/addrmgr/addrutil"
	"github.com/pkt-cash/PKT-FullNode/addrmgr/externaladdrs"
	"github.com/pkt-cash/PKT-FullNode/addrmgr/localaddrs"
	"github.com/pkt-cash/PKT-FullNode/btcutil/er"
	"github.com/pkt-cash/PKT-FullNode/pktlog/log"
	"github.com/pkt-cash/PKT-FullNode/wire/protocol"

	"github.com/pkt-cash/PKT-FullNode/chaincfg/chainhash"
	"github.com/pkt-cash/PKT-FullNode/wire"
)

// AddrManager provides a concurrency safe address manager for caching potential
// peers on the bitcoin network.
type AddrManager struct {
	mtx           sync.Mutex
	peersFile     string
	lookupFunc    func(string) ([]net.IP, er.R)
	rand          *rand.Rand
	key           [32]byte
	addrIndex     map[string]*KnownAddress // address key to ka for all addrs.
	addrNew       [newBucketCount]map[string]*KnownAddress
	addrTried     [triedBucketCount]*list.List
	started       int32
	shutdown      int32
	wg            sync.WaitGroup
	quit          chan struct{}
	nTried        int
	nNew          int
	version       int
	localAddrs    localaddrs.LocalAddrs
	LocalExternal externaladdrs.ExternalLocalAddrs
}

type serializedKnownAddress struct {
	Addr        string
	Src         string
	Attempts    int
	TimeStamp   int64
	LastAttempt int64
	LastSuccess int64
	Services    protocol.ServiceFlag
	SrcServices protocol.ServiceFlag
	// no refcount or tried, that is available from context.
}

type serializedAddrManager struct {
	Version      int
	Key          [32]byte
	Addresses    []*serializedKnownAddress
	NewBuckets   [newBucketCount][]string // string is addrutil.NetAddressKey
	TriedBuckets [triedBucketCount][]string
}

const (

	// New defaults for some values are normalized with Bitcoin Core.

	// needAddressThreshold is the number of addresses under which the
	// address manager will claim to need more addresses.
	needAddressThreshold = 3000

	// dumpAddressInterval is the interval used to dump the address
	// cache to disk for future use.
	dumpAddressInterval = 2 * time.Minute

	// triedBucketSize is the maximum number of addresses in each
	// tried address bucket.
	triedBucketSize = 256

	// triedBucketCount is the number of buckets we split tried
	// addresses over.
	triedBucketCount = 64

	// newBucketSize is the maximum number of addresses in each new address
	// bucket.
	newBucketSize = 64

	// newBucketCount is the number of buckets that we spread new addresses
	// over.
	newBucketCount = 1024

	// triedBucketsPerGroup is the number of tried buckets over which an
	// address group will be spread.
	triedBucketsPerGroup = 8

	// newBucketsPerGroup is the number of new buckets over which an
	// source address group will be spread.
	newBucketsPerGroup = 64

	// newBucketsPerAddress is the number of buckets a frequently seen new
	// address may end up in.
	newBucketsPerAddress = 8

	// numMissingDays is the number of days before which we assume an
	// address has vanished if we have not seen it announced  in that long.
	numMissingDays = 14

	// numRetries is the number of tried without a single success before
	// we assume an address is bad.
	numRetries = 5

	// maxFailures is the maximum number of failures we will accept without
	// a success before considering an address bad.
	maxFailures = 15

	// minBadDays is the number of days since the last success before we
	// will consider evicting an address.
	minBadDays = 7

	// getAddrMax is the most addresses that we will send in response
	// to a getAddr (in practise the most addresses we will return from a
	// call to AddressCache()).
	getAddrMax = 5000

	getAddrMin = 20

	// getAddrPercent is the percentage of total addresses known that we
	// will share with a call to AddressCache.
	getAddrPercent = 23

	// serialisationVersion is the current version of the on-disk format.
	serialisationVersion = 2
)

// updateAddress is a helper function to either update an address already known
// to the address manager, or to add the address if not already known.
func (a *AddrManager) updateAddress(netAddr, srcAddr *wire.NetAddress) {
	// Filter out non-routable addresses. Note that non-routable
	// also includes invalid and local addresses.
	if !addrutil.IsRoutable(netAddr) {
		return
	}

	addr := addrutil.NetAddressKey(netAddr)
	ka := a.find(netAddr)
	if ka != nil {
		// TODO: only update addresses periodically.
		// Update the last seen time and services.
		// note that to prevent causing excess garbage on getaddr
		// messages the netaddresses in addrmaanger are *immutable*,
		// if we need to change them then we replace the pointer with a
		// new copy so that we don't have to copy every na for getaddr.
		if netAddr.Timestamp.After(ka.na.Timestamp) ||
			(ka.na.Services&netAddr.Services) !=
				netAddr.Services {

			naCopy := *ka.na
			naCopy.Timestamp = netAddr.Timestamp
			naCopy.AddService(netAddr.Services)
			ka.na = &naCopy
		}

		// If already in tried, we have nothing to do here.
		if ka.tried {
			return
		}

		// Already at our max?
		if ka.refs == newBucketsPerAddress {
			return
		}

		// The more entries we have, the less likely we are to add more.
		// likelihood is 2N.
		factor := int32(2 * ka.refs)
		if a.rand.Int31n(factor) != 0 {
			return
		}
	} else {
		// Make a copy of the net address to avoid races since it is
		// updated elsewhere in the addrmanager code and would otherwise
		// change the actual netaddress on the peer.
		netAddrCopy := *netAddr
		ka = &KnownAddress{na: &netAddrCopy, srcAddr: srcAddr}
		a.addrIndex[addr] = ka
		a.nNew++
		// XXX time penalty?
	}

	bucket := a.getNewBucket(netAddr, srcAddr)

	// Already exists?
	if _, ok := a.addrNew[bucket][addr]; ok {
		return
	}

	// Enforce max addresses.
	if len(a.addrNew[bucket]) > newBucketSize {
		log.Tracef("new bucket is full, expiring old")
		a.expireNew(bucket)
	}

	// Add to new bucket.
	ka.refs++
	a.addrNew[bucket][addr] = ka

	log.Tracef("Added new address %s for a total of %d addresses", addr,
		a.nTried+a.nNew)
}

// expireNew makes space in the new buckets by expiring the really bad entries.
// If no bad entries are available we look at a few and remove the oldest.
func (a *AddrManager) expireNew(bucket int) {
	// First see if there are any entries that are so bad we can just throw
	// them away. otherwise we throw away the oldest entry in the cache.
	// Bitcoind here chooses four random and just throws the oldest of
	// those away, but we keep track of oldest in the initial traversal and
	// use that information instead.
	var oldest *KnownAddress
	for k, v := range a.addrNew[bucket] {
		if v.isBad() {
			log.Tracef("expiring bad address %v", k)
			delete(a.addrNew[bucket], k)
			v.refs--
			if v.refs == 0 {
				a.nNew--
				delete(a.addrIndex, k)
			}
			continue
		}
		if oldest == nil {
			oldest = v
		} else if !v.na.Timestamp.After(oldest.na.Timestamp) {
			oldest = v
		}
	}

	if oldest != nil {
		key := addrutil.NetAddressKey(oldest.na)
		log.Tracef("expiring oldest address %v", key)

		delete(a.addrNew[bucket], key)
		oldest.refs--
		if oldest.refs == 0 {
			a.nNew--
			delete(a.addrIndex, key)
		}
	}
}

// pickTried selects an address from the tried bucket to be evicted.
// We just choose the eldest. Bitcoind selects 4 random entries and throws away
// the older of them.
func (a *AddrManager) pickTried(bucket int) *list.Element {
	var oldest *KnownAddress
	var oldestElem *list.Element
	for e := a.addrTried[bucket].Front(); e != nil; e = e.Next() {
		ka := e.Value.(*KnownAddress)
		if oldest == nil || oldest.na.Timestamp.After(ka.na.Timestamp) {
			oldestElem = e
			oldest = ka
		}

	}
	return oldestElem
}

func (a *AddrManager) getNewBucket(netAddr, srcAddr *wire.NetAddress) int {
	// bitcoind:
	// doublesha256(key + sourcegroup + int64(doublesha256(key + group + sourcegroup))%bucket_per_source_group) % num_new_buckets

	data1 := []byte{}
	data1 = append(data1, a.key[:]...)
	data1 = append(data1, []byte(addrutil.GroupKey(netAddr))...)
	data1 = append(data1, []byte(addrutil.GroupKey(srcAddr))...)
	hash1 := chainhash.DoubleHashB(data1)
	hash64 := binary.LittleEndian.Uint64(hash1)
	hash64 %= newBucketsPerGroup
	var hashbuf [8]byte
	binary.LittleEndian.PutUint64(hashbuf[:], hash64)
	data2 := []byte{}
	data2 = append(data2, a.key[:]...)
	data2 = append(data2, addrutil.GroupKey(srcAddr)...)
	data2 = append(data2, hashbuf[:]...)

	hash2 := chainhash.DoubleHashB(data2)
	return int(binary.LittleEndian.Uint64(hash2) % newBucketCount)
}

func (a *AddrManager) getTriedBucket(netAddr *wire.NetAddress) int {
	// bitcoind hashes this as:
	// doublesha256(key + group + truncate_to_64bits(doublesha256(key)) % buckets_per_group) % num_buckets
	data1 := []byte{}
	data1 = append(data1, a.key[:]...)
	data1 = append(data1, []byte(addrutil.NetAddressKey(netAddr))...)
	hash1 := chainhash.DoubleHashB(data1)
	hash64 := binary.LittleEndian.Uint64(hash1)
	hash64 %= triedBucketsPerGroup
	var hashbuf [8]byte
	binary.LittleEndian.PutUint64(hashbuf[:], hash64)
	data2 := []byte{}
	data2 = append(data2, a.key[:]...)
	data2 = append(data2, addrutil.GroupKey(netAddr)...)
	data2 = append(data2, hashbuf[:]...)

	hash2 := chainhash.DoubleHashB(data2)
	return int(binary.LittleEndian.Uint64(hash2) % triedBucketCount)
}

// addressHandler is the main handler for the address manager.  It must be run
// as a goroutine.
func (a *AddrManager) addressHandler() {
	dumpAddressTicker := time.NewTicker(dumpAddressInterval)
	defer dumpAddressTicker.Stop()
out:
	for {
		select {
		case <-dumpAddressTicker.C:
			a.savePeers()

		case <-a.quit:
			break out
		}
	}
	a.savePeers()
	a.wg.Done()
	log.Trace("Address handler done")
}

// savePeers saves all the known addresses to a file so they can be read back
// in at next run.
func (a *AddrManager) savePeers() {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	// First we make a serialisable datastructure so we can encode it to
	// json.
	sam := new(serializedAddrManager)
	sam.Version = a.version
	copy(sam.Key[:], a.key[:])

	sam.Addresses = make([]*serializedKnownAddress, len(a.addrIndex))
	i := 0
	for k, v := range a.addrIndex {
		ska := new(serializedKnownAddress)
		ska.Addr = k
		ska.TimeStamp = v.na.Timestamp.Unix()
		ska.Src = addrutil.NetAddressKey(v.srcAddr)
		ska.Attempts = v.attempts
		ska.LastAttempt = v.lastattempt.Unix()
		ska.LastSuccess = v.lastsuccess.Unix()
		if a.version > 1 {
			ska.Services = v.na.Services
			ska.SrcServices = v.srcAddr.Services
		}
		// Tried and refs are implicit in the rest of the structure
		// and will be worked out from context on unserialisation.
		sam.Addresses[i] = ska
		i++
	}
	for i := range a.addrNew {
		sam.NewBuckets[i] = make([]string, len(a.addrNew[i]))
		j := 0
		for k := range a.addrNew[i] {
			sam.NewBuckets[i][j] = k
			j++
		}
	}
	for i := range a.addrTried {
		sam.TriedBuckets[i] = make([]string, a.addrTried[i].Len())
		j := 0
		for e := a.addrTried[i].Front(); e != nil; e = e.Next() {
			ka := e.Value.(*KnownAddress)
			sam.TriedBuckets[i][j] = addrutil.NetAddressKey(ka.na)
			j++
		}
	}

	w, err := os.Create(a.peersFile)
	if err != nil {
		log.Errorf("Error opening file %s: %v", a.peersFile, err)
		return
	}
	enc := jsoniter.NewEncoder(w)
	defer w.Close()
	if err := enc.Encode(&sam); err != nil {
		log.Errorf("Failed to encode file %s: %v", a.peersFile, err)
		return
	}
}

// loadPeers loads the known address from the saved file.  If empty, missing, or
// malformed file, just don't load anything and start fresh
func (a *AddrManager) loadPeers() {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	err := a.deserializePeers(a.peersFile)
	if err != nil {
		log.Errorf("Failed to parse file %s: %v", a.peersFile, err)
		// if it is invalid we nuke the old one unconditionally.
		err = er.E(os.Remove(a.peersFile))
		if err != nil {
			log.Warnf("Failed to remove corrupt peers file %s: %v",
				a.peersFile, err)
		}
		a.reset()
		return
	}
	log.Debugf("Loaded %d addresses from file '%s'", a.numAddresses(), a.peersFile)
}

func (a *AddrManager) deserializePeers(filePath string) er.R {

	_, errr := os.Stat(filePath)
	if os.IsNotExist(errr) {
		return nil
	}
	r, errr := os.Open(filePath)
	if errr != nil {
		return er.Errorf("%s error opening file: %v", filePath, errr)
	}
	defer r.Close()

	var sam serializedAddrManager
	dec := jsoniter.NewDecoder(r)
	errr = dec.Decode(&sam)
	if errr != nil {
		return er.Errorf("error reading %s: %v", filePath, errr)
	}

	// Since decoding JSON is backwards compatible (i.e., only decodes
	// fields it understands), we'll only return an error upon seeing a
	// version past our latest supported version.
	if sam.Version > serialisationVersion {
		return er.Errorf("unknown version %v in serialized "+
			"addrmanager", sam.Version)
	}

	copy(a.key[:], sam.Key[:])

	for _, v := range sam.Addresses {
		ka := new(KnownAddress)

		// The first version of the serialized address manager was not
		// aware of the service bits associated with this address, so
		// we'll assign a default of SFNodeNetwork to it.
		if sam.Version == 1 {
			v.Services = protocol.SFNodeNetwork
		}
		var err er.R
		ka.na, err = a.DeserializeNetAddress(v.Addr, v.Services)
		if err != nil {
			return er.Errorf("failed to deserialize netaddress "+
				"%s: %v", v.Addr, err)
		}

		// The first version of the serialized address manager was not
		// aware of the service bits associated with the source address,
		// so we'll assign a default of SFNodeNetwork to it.
		if sam.Version == 1 {
			v.SrcServices = protocol.SFNodeNetwork
		}
		ka.srcAddr, err = a.DeserializeNetAddress(v.Src, v.SrcServices)
		if err != nil {
			return er.Errorf("failed to deserialize netaddress "+
				"%s: %v", v.Src, err)
		}

		ka.attempts = v.Attempts
		ka.lastattempt = time.Unix(v.LastAttempt, 0)
		ka.lastsuccess = time.Unix(v.LastSuccess, 0)
		a.addrIndex[addrutil.NetAddressKey(ka.na)] = ka
	}

	for i := range sam.NewBuckets {
		for _, val := range sam.NewBuckets[i] {
			ka, ok := a.addrIndex[val]
			if !ok {
				return er.Errorf("newbucket contains %s but "+
					"none in address list", val)
			}

			if ka.refs == 0 {
				a.nNew++
			}
			ka.refs++
			a.addrNew[i][val] = ka
		}
	}
	for i := range sam.TriedBuckets {
		for _, val := range sam.TriedBuckets[i] {
			ka, ok := a.addrIndex[val]
			if !ok {
				return er.Errorf("Newbucket contains %s but "+
					"none in address list", val)
			}

			ka.tried = true
			a.nTried++
			a.addrTried[i].PushBack(ka)
		}
	}

	// Sanity checking.
	for k, v := range a.addrIndex {
		if v.refs == 0 && !v.tried {
			return er.Errorf("address %s after serialisation "+
				"with no references", k)
		}

		if v.refs > 0 && v.tried {
			return er.Errorf("address %s after serialisation "+
				"which is both new and tried!", k)
		}
	}

	return nil
}

// DeserializeNetAddress converts a given address string to a *wire.NetAddress.
func (a *AddrManager) DeserializeNetAddress(addr string,
	services protocol.ServiceFlag) (*wire.NetAddress, er.R) {

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, er.E(err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, er.E(err)
	}

	return a.HostToNetAddress(host, uint16(port), services)
}

// Start begins the core address handler which manages a pool of known
// addresses, timeouts, and interval based writes.
func (a *AddrManager) Start() {
	// Already started?
	if atomic.AddInt32(&a.started, 1) != 1 {
		return
	}

	log.Trace("Starting address manager")

	go func() {
		for {
			a.localAddrs.Referesh()
			time.Sleep(time.Second * 30)
		}
	}()

	// Load peers we already know about from file.
	a.loadPeers()

	// Start the address ticker to save addresses periodically.
	a.wg.Add(1)
	go a.addressHandler()
}

// Stop gracefully shuts down the address manager by stopping the main handler.
func (a *AddrManager) Stop() er.R {
	if atomic.AddInt32(&a.shutdown, 1) != 1 {
		log.Warnf("Address manager is already in the process of " +
			"shutting down")
		return nil
	}

	log.Infof("Address manager shutting down")
	close(a.quit)
	a.wg.Wait()
	return nil
}

// AddAddresses adds new addresses to the address manager.  It enforces a max
// number of addresses and silently ignores duplicate addresses.  It is
// safe for concurrent access.
func (a *AddrManager) AddAddresses(addrs []*wire.NetAddress, srcAddr *wire.NetAddress) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	for _, na := range addrs {
		a.updateAddress(na, srcAddr)
	}
}

// AddAddress adds a new address to the address manager.  It enforces a max
// number of addresses and silently ignores duplicate addresses.  It is
// safe for concurrent access.
func (a *AddrManager) AddAddress(addr, srcAddr *wire.NetAddress) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	a.updateAddress(addr, srcAddr)
}

// AddAddressByIP adds an address where we are given an ip:port and not a
// wire.NetAddress.
func (a *AddrManager) AddAddressByIP(addrIP string) er.R {
	// Split IP and port
	addr, portStr, err := net.SplitHostPort(addrIP)
	if err != nil {
		return er.E(err)
	}
	// Put it in wire.Netaddress
	ip := net.ParseIP(addr)
	if ip == nil {
		return er.Errorf("invalid ip address %s", addr)
	}
	port, err := strconv.ParseUint(portStr, 10, 0)
	if err != nil {
		return er.Errorf("invalid port %s: %v", portStr, err)
	}
	na := wire.NewNetAddressIPPort(ip, uint16(port), 0)
	a.AddAddress(na, na) // XXX use correct src address
	return nil
}

// NumAddresses returns the number of addresses known to the address manager.
func (a *AddrManager) numAddresses() int {
	return a.nTried + a.nNew
}

// NumAddresses returns the number of addresses known to the address manager.
func (a *AddrManager) NumAddresses() int {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	return a.numAddresses()
}

// NeedMoreAddresses returns whether or not the address manager needs more
// addresses.
func (a *AddrManager) NeedMoreAddresses() bool {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	return a.numAddresses() < needAddressThreshold
}

func (a *AddrManager) addressesThatOnceWorked() []*wire.NetAddress {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	count := 0
	for _, v := range a.addrIndex {
		if v.lastsuccess.After(time.Unix(0, 0)) {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	addrs := make([]*wire.NetAddress, 0, count)
	for _, v := range a.addrIndex {
		if v.lastsuccess.After(time.Unix(0, 0)) {
			addrs = append(addrs, v.na)
		}
	}

	return addrs
}

// AddressCache returns the current address cache.  It must be treated as
// read-only (but since it is a copy now, this is not as dangerous).
func (a *AddrManager) AddressesToShare() []*wire.NetAddress {
	allAddr := a.addressesThatOnceWorked()

	numAddresses := len(allAddr) * getAddrPercent / 100
	if numAddresses > getAddrMax {
		numAddresses = getAddrMax
	} else if numAddresses < getAddrMin {
		numAddresses = len(allAddr)
	}

	// Fisher-Yates shuffle the array. We only need to do the first
	// `numAddresses' since we are throwing the rest.
	for i := 0; i < numAddresses; i++ {
		// pick a number between current index and the end
		j := rand.Intn(len(allAddr)-i) + i
		allAddr[i], allAddr[j] = allAddr[j], allAddr[i]
	}

	// slice off the limit we are willing to share.
	return allAddr[0:numAddresses]
}

// getAddresses returns all of the addresses currently found within the
// manager's address cache.
func (a *AddrManager) getAddresses() []*wire.NetAddress {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	addrIndexLen := len(a.addrIndex)
	if addrIndexLen == 0 {
		return nil
	}

	addrs := make([]*wire.NetAddress, 0, addrIndexLen)
	for _, v := range a.addrIndex {
		addrs = append(addrs, v.na)
	}

	return addrs
}

// reset resets the address manager by reinitialising the random source
// and allocating fresh empty bucket storage.
func (a *AddrManager) reset() {

	a.addrIndex = make(map[string]*KnownAddress)

	// fill key with bytes from a good random source.
	_, err := io.ReadFull(crand.Reader, a.key[:])
	if err != nil {
		panic("reset: io.ReadFull failure")
	}
	for i := range a.addrNew {
		a.addrNew[i] = make(map[string]*KnownAddress)
	}
	for i := range a.addrTried {
		a.addrTried[i] = list.New()
	}
}

// HostToNetAddress returns a netaddress given a host address.
// If the host is not an IP address it will be resolved
func (a *AddrManager) HostToNetAddress(host string, port uint16, services protocol.ServiceFlag) (*wire.NetAddress, er.R) {
	var ip net.IP
	if ip = net.ParseIP(host); ip == nil {
		ips, err := a.lookupFunc(host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, er.Errorf("no addresses found for %s", host)
		}
		ip = ips[0]
	}

	return wire.NewNetAddressIPPort(ip, port, services), nil
}

func (a *AddrManager) isGoodAddress(ka *KnownAddress, relaxedMode bool, isOk func(*KnownAddress) bool) bool {
	// If for some reason, we're not able to get our local addrs (OS permissions)
	// we'll pretend everything is ok.
	if !a.localAddrs.Reachable(ka.NetAddress()) && a.localAddrs.IsWorking() {
		// Unreachable address
		return false
	}
	if ka.lastattempt.Add(time.Second * 60).After(time.Now()) {
		// Never connect to something which has been connected in the past 60 seconds.
		return false
	} else if relaxedMode {
	} else if ka.srcAddr.Services&protocol.SFTrusted == protocol.SFTrusted {
	} else if ka.lastsuccess.After(time.Unix(0, 0)) {
	} else {
		return false
	}
	a.mtx.Unlock()
	ok := isOk(ka)
	a.mtx.Lock()
	if !ok {
		return false
	} else if ka.lastattempt.Add(time.Second * 60).After(time.Now()) {
		// Race condition because we had to unlock
		return false
	}
	return true
}

func (a *AddrManager) getTriedAddress(relaxedMode bool, isOk func(*KnownAddress) bool) *KnownAddress {
	// pick a random starting bucket.
	startBucket := a.rand.Intn(len(a.addrTried))
	for bucketMod := startBucket; bucketMod < startBucket*2; bucketMod++ {
		bucket := bucketMod % len(a.addrTried)
		if a.addrTried[bucket].Len() == 0 {
			continue
		}
		// Pick a random starting point within the bucket
		startingPoint := a.rand.Int63n(int64(a.addrTried[bucket].Len()))

		// Walk to that starting point
		e := a.addrTried[bucket].Front()
		for i := startingPoint; i > 0 && e != nil; i-- {
			e = e.Next()
		}

		// Walk backward from the starting point looking for a usable address
		for e != nil {
			va := e.Value.(*KnownAddress)
			if a.isGoodAddress(va, relaxedMode, isOk) {
				return va
			}
			e = e.Next()
		}

		// Reset and walk from the front of the bucket toward the starting point
		e = a.addrTried[bucket].Front()
		for i := startingPoint; i > 0 && e != nil; i-- {
			va := e.Value.(*KnownAddress)
			if a.isGoodAddress(va, relaxedMode, isOk) {
				return va
			}
			e = e.Next()
		}
	}
	return nil
}

func (a *AddrManager) getUntriedAddress(relaxedMode bool, isOk func(*KnownAddress) bool) *KnownAddress {
	// Pick a random starting bucket.
	startBucket := a.rand.Intn(len(a.addrNew))
	for bucketMod := startBucket; bucketMod < startBucket*2; bucketMod++ {
		bucket := bucketMod % len(a.addrNew)
		if len(a.addrNew[bucket]) == 0 {
			continue
		}
		// Then, a random starting point in it.
		startingPoint := a.rand.Intn(len(a.addrNew[bucket]))

		// Skip until starting point, take first valid address
		i := -1
		for _, value := range a.addrNew[bucket] {
			i++
			if i < startingPoint {
				continue
			}
			if a.isGoodAddress(value, relaxedMode, isOk) {
				return value
			}
		}

		// Skip after starting point, take first valid address
		i = -1
		for _, value := range a.addrNew[bucket] {
			i++
			if i >= startingPoint {
				break
			}
			if a.isGoodAddress(value, relaxedMode, isOk) {
				return value
			}
		}
	}
	return nil
}

func (a *AddrManager) getAddress(relaxedMode bool, isOk func(*KnownAddress) bool) *KnownAddress {
	if a.nTried > 0 && (a.nNew == 0 || a.rand.Intn(2) == 0) {
		if addr := a.getTriedAddress(relaxedMode, isOk); addr != nil {
			return addr
		} else if addr := a.getUntriedAddress(relaxedMode, isOk); addr != nil {
			return addr
		}
	} else {
		if addr := a.getUntriedAddress(relaxedMode, isOk); addr != nil {
			return addr
		} else if addr := a.getTriedAddress(relaxedMode, isOk); addr != nil {
			return addr
		}
	}
	if !relaxedMode {
		return a.getAddress(true, isOk)
	} else {
		return nil
	}
}

// GetAddress returns a single address that should be routable.  It picks a
// random one from the possible addresses with preference given to ones that
// have not been used recently and should not pick 'close' addresses
// consecutively.
func (a *AddrManager) GetAddress(isOk func(*KnownAddress) bool) *KnownAddress {
	// Protect concurrent access.
	a.mtx.Lock()
	defer a.mtx.Unlock()

	if a.numAddresses() == 0 {
		log.Infof("GetAddress() -> nil because no addresses at all")
		return nil
	}
	addr := a.getAddress(false, isOk)
	if addr != nil {
		// Because we have an isOk function, we can assume that if that function passes
		// the address WILL be attempted.
		addr.attempts++
		addr.lastattempt = time.Now()
	} else {
		log.Infof("GetAddress() -> nil no qualifying addresses found")
	}
	return addr
}

func (a *AddrManager) find(addr *wire.NetAddress) *KnownAddress {
	return a.addrIndex[addrutil.NetAddressKey(addr)]
}

// GetLastAttempt retrieves an address' last attempt time.
func (a *AddrManager) GetLastAttempt(addr *wire.NetAddress) time.Time {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	ka := a.find(addr)
	if ka == nil {
		// If not found, return zero time.
		return time.Time{}
	}

	return ka.LastAttempt()
}

// Connected Marks the given address as currently connected and working at the
// current time.  The address must already be known to AddrManager else it will
// be ignored.
func (a *AddrManager) Connected(addr *wire.NetAddress) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	ka := a.find(addr)
	if ka == nil {
		return
	}

	// Update the time as long as it has been 20 minutes since last we did
	// so.
	now := time.Now()
	if now.After(ka.na.Timestamp.Add(time.Minute * 20)) {
		// ka.na is immutable, so replace it.
		naCopy := *ka.na
		naCopy.Timestamp = time.Now()
		ka.na = &naCopy
	}
}

// Good marks the given address as good.  To be called after a successful
// connection and version exchange.  If the address is unknown to the address
// manager it will be ignored.
func (a *AddrManager) Good(addr *wire.NetAddress) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	ka := a.find(addr)
	if ka == nil {
		return
	}

	// ka.Timestamp is not updated here to avoid leaking information
	// about currently connected peers.
	now := time.Now()
	ka.lastsuccess = now
	ka.lastattempt = now
	ka.attempts = 0

	// move to tried set, optionally evicting other addresses if neeed.
	if ka.tried {
		return
	}

	// ok, need to move it to tried.

	// remove from all new buckets.
	// record one of the buckets in question and call it the `first'
	addrKey := addrutil.NetAddressKey(addr)
	oldBucket := -1
	for i := range a.addrNew {
		// we check for existence so we can record the first one
		if _, ok := a.addrNew[i][addrKey]; ok {
			delete(a.addrNew[i], addrKey)
			ka.refs--
			if oldBucket == -1 {
				oldBucket = i
			}
		}
	}
	a.nNew--

	if oldBucket == -1 {
		// What? wasn't in a bucket after all.... Panic?
		return
	}

	bucket := a.getTriedBucket(ka.na)

	// Room in this tried bucket?
	if a.addrTried[bucket].Len() < triedBucketSize {
		ka.tried = true
		a.addrTried[bucket].PushBack(ka)
		a.nTried++
		return
	}

	// No room, we have to evict something else.
	entry := a.pickTried(bucket)
	rmka := entry.Value.(*KnownAddress)

	// First bucket it would have been put in.
	newBucket := a.getNewBucket(rmka.na, rmka.srcAddr)

	// If no room in the original bucket, we put it in a bucket we just
	// freed up a space in.
	if len(a.addrNew[newBucket]) >= newBucketSize {
		newBucket = oldBucket
	}

	// replace with ka in list.
	ka.tried = true
	entry.Value = ka

	rmka.tried = false
	rmka.refs++

	// We don't touch a.nTried here since the number of tried stays the same
	// but we decemented new above, raise it again since we're putting
	// something back.
	a.nNew++

	rmkey := addrutil.NetAddressKey(rmka.na)
	log.Tracef("Replacing %s with %s in tried", rmkey, addrKey)

	// We made sure there is space here just above.
	a.addrNew[newBucket][rmkey] = rmka
}

// SetServices sets the services for the giiven address to the provided value.
func (a *AddrManager) SetServices(addr *wire.NetAddress, services protocol.ServiceFlag) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	ka := a.find(addr)
	if ka == nil {
		return
	}

	// Update the services if needed.
	if ka.na.Services != services {
		// ka.na is immutable, so replace it.
		naCopy := *ka.na
		naCopy.Services = services
		ka.na = &naCopy
	}
}

// New returns a new bitcoin address manager.
// Use Start to begin processing asynchronous address updates.
func New(dataDir string, lookupFunc func(string) ([]net.IP, er.R)) *AddrManager {
	am := AddrManager{
		peersFile:  filepath.Join(dataDir, "peers.json"),
		lookupFunc: lookupFunc,
		rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
		quit:       make(chan struct{}),
		version:    serialisationVersion,
		localAddrs: localaddrs.New(),
	}
	am.reset()
	return &am
}
