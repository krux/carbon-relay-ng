package main

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/Dieterbe/topic"
	"net"
	"sort"
	"sync"
	"testing"
	"time"
)

type TestEndpoint struct {
	t              *testing.T
	ln             net.Listener
	seen           chan []byte
	seenBufs       [][]byte
	shutdown       chan bool
	shutdownHandle chan bool // to shut down 1 handler. if you start more handlers they'll keep running
	WhatHaveISeen  chan bool
	IHaveSeen      chan [][]byte
	addr           string
	accepts        *topic.Topic
	numSeen        *topic.Topic
}

func NewTestEndpoint(t *testing.T, addr string) *TestEndpoint {
	// shutdown chan size 1 so that Close() doesn't have to wait on the write
	// because the loops will typically be stuck in Accept and Readline
	return &TestEndpoint{
		t:              t,
		addr:           addr,
		seen:           make(chan []byte),
		seenBufs:       make([][]byte, 0),
		shutdown:       make(chan bool, 1),
		shutdownHandle: make(chan bool, 1),
		WhatHaveISeen:  make(chan bool),
		IHaveSeen:      make(chan [][]byte),
		accepts:        topic.New(),
		numSeen:        topic.New(),
	}
}

func (tE *TestEndpoint) Start() {
	ln, err := net.Listen("tcp", tE.addr)
	if err != nil {
		panic(err)
	}
	log.Notice("tE %s is now listening\n", tE.addr)
	tE.ln = ln
	go func() {
		numAccepts := 0
		for {
			select {
			case <-tE.shutdown:
				return
			default:
			}
			log.Debug("tE %s waiting for accept\n", tE.addr)
			conn, err := ln.Accept()
			// when closing, this can happen: accept tcp [::]:2005: use of closed network connection
			if err != nil {
				log.Debug("tE %s accept error: '%s' -> stopping tE\n", tE.addr, err)
				return
			}
			numAccepts += 1
			tE.accepts.Broadcast <- numAccepts
			log.Notice("tE %s accepted new conn\n", tE.addr)
			go tE.handle(conn)
			defer func() { log.Debug("tE %s closing conn.\n", tE.addr); conn.Close() }()
		}
	}()
	go func() {
		numSeen := 0
		for {
			select {
			case buf := <-tE.seen:
				tE.seenBufs = append(tE.seenBufs, buf)
				numSeen += 1
				tE.numSeen.Broadcast <- numSeen
			case <-tE.WhatHaveISeen:
				var c [][]byte
				c = append(c, tE.seenBufs...)
				tE.IHaveSeen <- c
			}
		}
	}()
}

func (tE *TestEndpoint) String() string {
	return fmt.Sprintf("testEndpoint %s", tE.addr)
}

// note: per conditionwatcher, only call Allow() or Wait(), once.
// (because the result is in channel met and can only be consumed once)
// feel free to make as many conditionWatcher's as you want.
type conditionWatcher struct {
	t             *testing.T
	key           string
	desiredStatus string

	sync.Mutex
	lastStatus string

	met chan bool
}

func newConditionWatcher(tE *TestEndpoint, key, desiredStatus string) *conditionWatcher {
	c := conditionWatcher{
		t:             tE.t,
		key:           key,
		desiredStatus: desiredStatus,
		met:           make(chan bool, 1),
	}
	return &c
}

func (c *conditionWatcher) AllowBG(timeout time.Duration, wg *sync.WaitGroup) {
	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		c.Allow(timeout)
		wg.Done()
	}(wg)
}

func (c *conditionWatcher) PreferBG(timeout time.Duration, wg *sync.WaitGroup) {
	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		c.Prefer(timeout)
		wg.Done()
	}(wg)
}

func (c *conditionWatcher) Prefer(timeout time.Duration) {
	timeoutChan := time.After(timeout)
	select {
	case <-c.met:
		return
	case <-timeoutChan:
		return
	}
}

func (c *conditionWatcher) Allow(timeout time.Duration) {
	timeoutChan := time.After(timeout)
	select {
	case <-c.met:
		return
	case <-timeoutChan:
		c.Lock()
		// for some reason the c.t.Fatalf often gets somehow stuck. but the fmt.Printf works at least
		fmt.Printf("FATAL: %s timed out after %s of waiting for %s (%s)", c.key, timeout, c.desiredStatus, c.lastStatus)
		c.t.Fatalf("%s timed out after %s of waiting for %s (%s)", c.key, timeout, c.desiredStatus, c.lastStatus)
		c.Unlock()
	}
}

// no timeout
func (c *conditionWatcher) Wait() {
	<-c.met
}

func (tE *TestEndpoint) conditionNumAccepts(desired int) *conditionWatcher {
	c := newConditionWatcher(tE, fmt.Sprintf("tE %s", tE.addr), fmt.Sprintf("%d accepts", desired))
	// we want to make sure we can consume straight after registering
	// (otherwise topic can kick us out)
	// but also this function can only return when we're ready to catch appropriate
	// events
	ready := make(chan bool)

	go func(ready chan bool) {
		consumer := make(chan interface{})
		tE.accepts.Register(consumer)
		c.Lock()
		c.lastStatus = fmt.Sprintf("saw 0")
		c.Unlock()
		ready <- true
		for val := range consumer {
			seen := val.(int)
			if desired == seen {
				c.met <- true
				// the condition is no longer useful and can kill itself
				// it's safer and simpler to just create new conditions for new checks
				return
			}
			c.Lock()
			c.lastStatus = fmt.Sprintf("saw %d", seen)
			c.Unlock()
		}
	}(ready)
	<-ready
	return c
}

func (tE *TestEndpoint) conditionNumSeen(desired int) *conditionWatcher {
	c := newConditionWatcher(tE, fmt.Sprintf("tE %s", tE.addr), fmt.Sprintf("%d packets seen", desired))
	ready := make(chan bool)

	go func(ready chan bool) {
		consumer := make(chan interface{})
		tE.numSeen.Register(consumer)
		c.Lock()
		c.lastStatus = fmt.Sprintf("saw 0")
		c.Unlock()
		ready <- true
		for val := range consumer {
			seen := val.(int)
			if desired == seen {
				c.met <- true
				// the condition is no longer useful and can kill itself
				// it's safer and simpler to just create new conditions for new checks
				tE.numSeen.Unregister(consumer)
				return
			}
			c.Lock()
			c.lastStatus = fmt.Sprintf("saw %d", seen)
			c.Unlock()
		}
	}(ready)
	<-ready
	return c
}

// arr implements sort.Interface for [][]byte
type arr [][]byte

func (data arr) Len() int {
	return len(data)
}
func (data arr) Swap(i, j int) {
	data[i], data[j] = data[j], data[i]
}
func (data arr) Less(i, j int) bool {
	return bytes.Compare(data[i], data[j]) < 0
}

// assume that ref feeds in sorted order, because we rely on that!
func (tE *TestEndpoint) SeenThisOrFatal(ref chan []byte) {
	tE.WhatHaveISeen <- true
	seen := <-tE.IHaveSeen
	sort.Sort(arr(seen))
	getRefBuf := func() []byte {
		return <-ref
	}
	i := 0
	getSeenBuf := func() []byte {
		if len(seen) <= i {
			return nil
		}
		i += 1
		return seen[i-1]
	}

	ok := true
	refBuf := getRefBuf()
	seenBuf := getSeenBuf()
	for refBuf != nil || seenBuf != nil {
		cmp := bytes.Compare(seenBuf, refBuf)
		if seenBuf == nil || refBuf == nil {
			// if one of them is nil, we want it to be counted as "very high", because there's no more input
			// so in that case, invert the rules
			cmp *= -1
		}
		switch cmp {
		case 0:
			refBuf = getRefBuf()
			seenBuf = getSeenBuf()
		case 1: // seen bigger than refBuf, i.e. seen is missing a line
			if ok {
				tE.t.Error("diff <reference> <seen>")
			}
			ok = false
			tE.t.Errorf("tE %s - %s", tE.addr, refBuf)
			refBuf = getRefBuf()
		case -1: // seen is smaller than refBuf, i.e. it has a line more than the ref has
			if ok {
				tE.t.Error("diff <reference> <seen>")
			}
			ok = false
			tE.t.Errorf("tE %s + %s", tE.addr, seenBuf)
			seenBuf = getSeenBuf()
		}
	}
	if !ok {
		tE.t.Fatal("bad data")
	}
}

func (tE *TestEndpoint) handle(c net.Conn) {
	defer func() {
		log.Debug("tE %s closing conn %s\n", tE.addr, c)
		c.Close()
	}()
	r := bufio.NewReaderSize(c, 4096)
	for {
		select {
		case <-tE.shutdownHandle:
			return
		default:
		}
		buf, _, err := r.ReadLine()
		if err != nil {
			log.Warning("tE %s read error: %s. closing handler\n", tE.addr, err)
			return
		}
		log.Info("tE %s %s read\n", tE.addr, buf)
		buf_copy := make([]byte, len(buf), len(buf))
		copy(buf_copy, buf)
		tE.seen <- buf_copy
	}
}

func (tE *TestEndpoint) Close() {
	log.Debug("tE %s shutting down accepter (after accept breaks)", tE.addr)
	tE.shutdown <- true
	log.Debug("tE %s shutting down handler (after readLine breaks)", tE.addr)
	tE.shutdownHandle <- true
	log.Debug("tE %s shutting down listener", tE.addr)
	tE.ln.Close()
	log.Debug("tE %s listener down", tE.addr)
}
