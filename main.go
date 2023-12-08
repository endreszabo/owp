package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"

	"os"
	"pinger/protobuf"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sebbbastien/prometheus_remote_client_golang/promremote"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var hostname string
var GitCommit string
var metricsServer string

type target struct {
	Address  string
	Interval time.Duration
	Padding  uint16
	Conn     *icmp.PacketConn
	DFset    bool
}

var (
	packetsSent = promauto.NewCounter(prometheus.CounterOpts{
		Name: "1wping_packets_sent",
		Help: "The total number of ping packets sent",
	})
	packetsRcvd = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "1wping_packets_received",
		Help: "Number of packets received",
	}, []string{"type"})
)

func schedule(what func(), delay time.Duration) chan bool {
	stop := make(chan bool)

	go func() {
		for {
			go what()
			select {
			case <-time.Tick(delay):
			case <-stop:
				return
			}
		}
	}()

	return stop
}

func promMetrics() {
	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(metricsServer, nil)
}

func (t *target) worker() {
	// Sub-second randomization of polling interval
	time.Sleep(time.Duration(rand.Float64() * float64(t.Interval)))

	dst, err := net.ResolveIPAddr("ip4", t.Address)
	if err != nil {
		log.Fatal(err)
	}
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:  os.Getpid() & 0xffff,
			Seq: 0,
			//Data: m,
		},
	}

	schedule(func() {
		ts := timestamppb.Now()
		fmt.Fprintf(os.Stderr, " ->   addr=%s\tsending_host=%s\n", dst, hostname)
		payload := protobuf.Payload{
			LastUpdated: ts,
			Source:      hostname,
		}

		var err error
		msg.Body.(*icmp.Echo).Data, err = proto.Marshal(&payload)
		if err != nil {
			log.Fatal(err)
		}

		wb, err := msg.Marshal(nil)
		if err != nil {
			log.Fatal(err)
		}

		if _, err := t.Conn.WriteTo(wb, dst); err != nil {
			log.Fatal(err)
		}
		packetsSent.Inc()

	}, t.Interval)
}

func pushMetrics(client promremote.Client, metrics chan promremote.TimeSeries) {
	ctx := context.Background()
	for {
		_, writeErr := client.WriteTimeSeries(ctx, []promremote.TimeSeries{<-metrics}, promremote.WriteOptions{})

		if writeErr != nil {
			log.Fatalf("error writing to prometheus remote write: %v", writeErr)
		}
	}
}

func main() {
	log.Printf("starting up version %s\n", GitCommit)

	metricsServer = "127.0.0.1:9951"
	go promMetrics()

	var err error
	hostname, err = os.Hostname()

	if err != nil {
		log.Fatal(err)
	}

	packetconn, err := icmp.ListenPacket("ip4:1", "0.0.0.0")
	_ = packetconn.IPv4PacketConn().SetControlMessage(ipv4.FlagTTL, true)

	for _, destination := range os.Args[1:] {
		google := target{
			Address:  destination,
			Conn:     packetconn,
			Interval: 1 * time.Second,
		}
		go google.worker()
	}

	if err != nil {
		log.Fatal(err)
	}
	defer packetconn.Close()

	cfg := promremote.NewConfig(
		promremote.WriteURLOption("http://127.0.0.1/foo"),
		promremote.HTTPClientTimeoutOption(60*time.Second),
		promremote.UserAgent("1wping"),
	)

	client, err := promremote.NewClient(cfg)
	if err != nil {
		log.Fatalf("unable to construct client: %v", err)
	}

	metricsChan := make(chan promremote.TimeSeries, 1)
	go pushMetrics(client, metricsChan)

	for {
		rb := make([]byte, 1500)
		c4 := packetconn.IPv4PacketConn()
		n, cm, peer, err := c4.ReadFrom(rb)
		if err != nil {
			log.Fatal(err)
		}

		rm, err := icmp.ParseMessage(1, rb[:n])
		if err != nil {
			log.Fatal(err)
		}

		switch rm.Type {
		case ipv4.ICMPTypeEcho:
			gpayload := protobuf.Payload{}
			buf, err := rm.Body.Marshal(1)
			if err != nil {
				log.Fatal(err)
			}
			err = proto.Unmarshal(buf[4:], &gpayload)
			if err != nil {
				continue
			}
			recvTime := gpayload.LastUpdated.AsTime()
			delta := time.Since(recvTime)
			if delta < 0 {
				log.Printf("time skew detected; delta='%v', origin='%s', remote='%s'\n", delta, gpayload.Source, peer.String())
				continue
			}
			metricsChan <- promremote.TimeSeries{
				Labels: []promremote.Label{
					{
						Name:  "__name__",
						Value: "one_way_ping",
					},
					{
						Name:  "destination",
						Value: hostname,
					},
					{
						Name:  "source",
						Value: gpayload.Source,
					},
					{
						Name:  "ttl",
						Value: strconv.Itoa(cm.TTL),
					},
					{
						Name:  "len",
						Value: strconv.Itoa(rm.Body.Len(1)),
					},
				},
				Datapoint: promremote.Datapoint{
					Timestamp: time.Now(),
					Value:     float64(delta.Nanoseconds()),
				},
			}
			packetsRcvd.WithLabelValues("one_way").Inc()

		case ipv4.ICMPTypeEchoReply:
			gpayload := protobuf.Payload{}
			buf, err := rm.Body.Marshal(1)
			if err != nil {
				log.Fatal(err)
			}
			err = proto.Unmarshal(buf[4:], &gpayload)
			if err != nil {
				continue
			}
			recvTime := gpayload.LastUpdated.AsTime()
			delta := time.Since(recvTime)
			if delta < 0 {
				log.Printf("alert: time skew detected: %v\n", delta)
				continue
			}

			metricsChan <- promremote.TimeSeries{
				Labels: []promremote.Label{
					{
						Name:  "__name__",
						Value: "two_way_ping",
					},
					{
						Name:  "source",
						Value: hostname,
					},
					{
						Name:  "destination",
						Value: peer.String(),
					},
					{
						Name:  "ttl",
						Value: strconv.Itoa(cm.TTL),
					},
					{
						Name:  "len",
						Value: strconv.Itoa(rm.Body.Len(1)),
					},
				},
				Datapoint: promremote.Datapoint{
					Timestamp: time.Now(),
					Value:     float64(delta.Nanoseconds()),
				},
			}

			packetsRcvd.WithLabelValues("two_way").Inc()
		}
	}
}
