package main

import (
	"fmt"
	"log"
	"net"
	"time"

	"os"
	"pinger/protobuf"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var hostname string

type target struct {
	Address  string
	Interval time.Duration
	Padding  uint16
	Conn     *icmp.PacketConn
	DFset    bool
}

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

func (t *target) send() {
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
			//			Data: m,
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

	}, t.Interval)
}

func main() {
	var err error
	hostname, err = os.Hostname()

	if err != nil {
		log.Fatal(err)
	}

	packetconn, err := icmp.ListenPacket("ip4:1", "0.0.0.0")

	for _, destination := range os.Args[1:] {
		google := target{
			Address:  destination,
			Conn:     packetconn,
			Interval: 1 * time.Second,
		}
		go google.send()
	}

	if err != nil {
		log.Fatal(err)
	}
	defer packetconn.Close()

	for {
		rb := make([]byte, 1500)
		n, peer, err := packetconn.ReadFrom(rb)
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
				log.Printf("alert: time skew detected: %v\n", delta)
				continue
			}

			fmt.Printf("<-  1 addr=%s\tsending_host=%s\tlatency=%v\n", peer.String(), gpayload.Source, delta)
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

			fmt.Printf("<-  2 addr=%s\tsending_host=%s\tlatency=%v\n", peer.String(), gpayload.Source, delta)
			/*
				default:
					fmt.Printf("Failed: %+v\n", rm)
			*/
		}
	}
}
