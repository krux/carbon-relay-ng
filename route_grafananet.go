package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dieterbe/go-metrics"
	"github.com/jpillora/backoff"

	"github.com/lomik/go-carbon/persister"
	"github.com/raintank/raintank-metric/msg"
	"github.com/raintank/raintank-metric/schema"
)

type RouteGrafanaNet struct {
	baseRoute
	addr    string
	apiKey  string
	buf     chan []byte
	schemas persister.WhisperSchemas

	bufSize      int // amount of messages we can buffer up before providing backpressure. each message is about 100B. so 1e7 is about 1GB.
	flushMaxNum  int
	flushMaxWait time.Duration
	timeout      time.Duration
	sslVerify    bool

	numErrFlush       metrics.Counter
	numOut            metrics.Counter   // metrics successfully written to our buffered conn (no flushing yet)
	durationTickFlush metrics.Timer     // only updated after successfull flush
	durationManuFlush metrics.Timer     // only updated after successfull flush. not implemented yet
	tickFlushSize     metrics.Histogram // only updated after successfull flush
	manuFlushSize     metrics.Histogram // only updated after successfull flush. not implemented yet
	numBuffered       metrics.Gauge
}

// NewRouteGrafanaNet creates a special route that writes to a grafana.net datastore
// We will automatically run the route and the destination
// ignores spool for now
func NewRouteGrafanaNet(key, prefix, sub, regex, addr, apiKey, schemasFile string, spool, sslVerify bool, bufSize, flushMaxNum, flushMaxWait, timeout int) (Route, error) {
	m, err := NewMatcher(prefix, sub, regex)
	if err != nil {
		return nil, err
	}
	schemas, err := persister.ReadWhisperSchemas(schemasFile)
	if err != nil {
		return nil, err
	}
	var defaultFound bool
	for _, schema := range schemas {
		if schema.Pattern.String() == ".*" {
			defaultFound = true
		}
		if len(schema.Retentions) == 0 {
			return nil, fmt.Errorf("retention setting cannot be empty")
		}
	}
	if !defaultFound {
		// good graphite health (not sure what graphite does if there's no .*
		// but we definitely need to always be able to determine which interval to use
		return nil, fmt.Errorf("storage-conf does not have a default '.*' pattern")
	}

	cleanAddr := addrToPath(addr)

	r := &RouteGrafanaNet{
		baseRoute: baseRoute{sync.Mutex{}, atomic.Value{}, key},
		addr:      addr,
		apiKey:    apiKey,
		buf:       make(chan []byte, bufSize), // takes about 228MB on 64bit
		schemas:   schemas,

		bufSize:      bufSize,
		flushMaxNum:  flushMaxNum,
		flushMaxWait: time.Duration(flushMaxWait) * time.Millisecond,
		timeout:      time.Duration(timeout) * time.Millisecond,
		sslVerify:    sslVerify,

		numErrFlush:       Counter("dest=" + cleanAddr + ".unit=Err.type=flush"),
		numOut:            Counter("dest=" + cleanAddr + ".unit=Metric.direction=out"),
		durationTickFlush: Timer("dest=" + cleanAddr + ".what=durationFlush.type=ticker"),
		durationManuFlush: Timer("dest=" + cleanAddr + ".what=durationFlush.type=manual"),
		tickFlushSize:     Histogram("dest=" + cleanAddr + ".unit=B.what=FlushSize.type=ticker"),
		manuFlushSize:     Histogram("dest=" + cleanAddr + ".unit=B.what=FlushSize.type=manual"),
		numBuffered:       Gauge("dest=" + cleanAddr + ".unit=Metric.what=numBuffered"),
	}

	r.config.Store(baseRouteConfig{*m, make([]*Destination, 0)})
	go r.run()
	return r, nil
}

func (route *RouteGrafanaNet) run() {
	metrics := make([]*schema.MetricData, 0, route.flushMaxNum)
	ticker := time.NewTicker(route.flushMaxWait)
	client := &http.Client{
		Timeout: route.timeout,
	}
	if !route.sslVerify {
		// this transport should be the equivalent of Go's DefaultTransport
		client.Transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			// except for this
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
	}

	b := &backoff.Backoff{
		Min:    100 * time.Millisecond,
		Max:    time.Minute,
		Factor: 1.5,
		Jitter: true,
	}

	flush := func() {
		if len(metrics) == 0 {
			return
		}

		mda := schema.MetricDataArray(metrics)
		data, err := msg.CreateMsg(mda, 0, msg.FormatMetricDataArrayMsgp)
		if err != nil {
			panic(err)
		}

		for {
			pre := time.Now()
			req, err := http.NewRequest("POST", route.addr, bytes.NewBuffer(data))
			if err != nil {
				panic(err)
			}
			req.Header.Add("Authorization", "Bearer "+route.apiKey)
			req.Header.Add("Content-Type", "rt-metric-binary")
			resp, err := client.Do(req)
			diff := time.Since(pre)
			if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				b.Reset()
				log.Info("GrafanaNet sent %d metrics in %s -msg size %d", len(metrics), diff, len(data))
				route.numOut.Inc(int64(len(metrics)))
				route.tickFlushSize.Update(int64(len(data)))
				route.durationTickFlush.Update(diff)
				metrics = metrics[:0]
				resp.Body.Close()
				break
			}
			route.numErrFlush.Inc(1)
			dur := b.Duration()
			if err != nil {
				log.Warning("GrafanaNet failed to submit data: %s will try again in %s (this attempt took %s)", err, dur, diff)
			} else {
				buf := make([]byte, 300)
				n, _ := resp.Body.Read(buf)
				log.Warning("GrafanaNet failed to submit data: http %d - %s will try again in %s (this attempt took %s)", resp.StatusCode, buf[:n], dur, diff)
				resp.Body.Close()
			}

			time.Sleep(dur)
		}
	}
	for {
		select {
		case buf := <-route.buf:
			route.numBuffered.Dec(1)
			md := parseMetric(buf, route.schemas)
			if md == nil {
				continue
			}
			md.SetId()
			metrics = append(metrics, md)
			if len(metrics) == route.flushMaxNum {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func parseMetric(buf []byte, schemas persister.WhisperSchemas) *schema.MetricData {
	msg := strings.TrimSpace(string(buf))

	elements := strings.Fields(msg)
	if len(elements) != 3 {
		log.Error("RouteGrafanaNet: %q error: need 3 fields", str)
		return nil
	}
	name := elements[0]
	val, err := strconv.ParseFloat(elements[1], 64)
	if err != nil {
		log.Error("RouteGrafanaNet: %q error: %s", str, err.Error())
		return nil
	}
	timestamp, err := strconv.ParseUint(elements[2], 10, 32)
	if err != nil {
		log.Error("RouteGrafanaNet: %q error: %s", str, err.Error())
		return nil
	}

	s, ok := schemas.Match(name)
	if !ok {
		panic(fmt.Errorf("couldn't find a schema for %q - this is impossible since we asserted there was a default with patt .*", name))
	}

	md := schema.MetricData{
		Name:       name,
		Interval:   s.Retentions[0].SecondsPerPoint(),
		Value:      val,
		Unit:       "",
		Time:       int64(timestamp),
		TargetType: "gauge",
		Tags:       []string{},
		OrgId:      1, // the hosted tsdb service will adjust to the correct OrgId if using a regular key.  orgid 1 is only retained with admin key.
	}
	return &md
}

func (route *RouteGrafanaNet) Dispatch(buf []byte) {
	//conf := route.config.Load().(RouteConfig)
	// should return as quickly as possible
	log.Info("route %s sending to dest %s: %s", route.key, route.addr, buf)
	route.numBuffered.Inc(1)
	route.buf <- buf
}

func (route *RouteGrafanaNet) Flush() error {
	//conf := route.config.Load().(RouteConfig)
	// no-op. Flush() is currently not called by anything.
	return nil
}

func (route *RouteGrafanaNet) Shutdown() error {
	//conf := route.config.Load().(RouteConfig)
	return nil
}

func (route *RouteGrafanaNet) Snapshot() RouteSnapshot {
	return makeSnapshot(&route.baseRoute, "GrafanaNet")
}
