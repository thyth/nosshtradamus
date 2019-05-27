package main

import (
	"nosshtradamus/internal/predictive"
	"nosshtradamus/internal/sshproxy"

	"golang.org/x/crypto/ssh"

	"flag"
	"fmt"
	"io"
	"net"
	"time"
)

func main() {
	port := 0
	target := ""
	printPredictiveVersion := false
	noPrediction := false
	var fakeDelay time.Duration

	flag.IntVar(&port, "port", 0, "Proxy listen port")
	flag.StringVar(&target, "target", "", "Target SSH host")
	flag.BoolVar(&printPredictiveVersion, "version", false, "Display predictive backend version")
	flag.BoolVar(&noPrediction, "nopredict", false, "Disable the mosh-based predictive backend")
	flag.DurationVar(&fakeDelay, "fakeDelay", 0, "Artificial half-duplex latency added to sessions")
	flag.Parse()

	if printPredictiveVersion {
		if noPrediction {
			fmt.Println("Predictive Backend *DISABLED*")
		} else {
			fmt.Printf("Predictive Backend Version: %v\n", predictive.GetVersion())
		}
		if fakeDelay > 0 {
			fmt.Printf("Aritifical Added Latency: %v\n", fakeDelay)
		}
	}

	if port == 0 || target == "" {
		flag.Usage()
		return
	}

	// TODO need to expand channel stream filter to be able to generate request filters (to capture e.g. size changes)
	var filter sshproxy.ChannelStreamFilter
	if !noPrediction || fakeDelay > 0 {
		filter = func(chanType string, sshChannel ssh.Channel) io.ReadWriteCloser {
			var wrapped io.ReadWriteCloser

			if chanType == "session" {
				wrapped = sshChannel
				if fakeDelay > 0 {
					wrapped = predictive.Delay(wrapped, fakeDelay)
				}
				if !noPrediction {
					wrapped = predictive.Interpose(wrapped, predictive.DefaultCoalesceInterval, 80, 24)
				}

				if wrapped == sshChannel {
					wrapped = nil
				}
			}

			return wrapped
		}
	}

	if addr, err := net.ResolveTCPAddr("tcp", target); err != nil {
		panic(err)
	} else {
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			panic(err)
		}
		err = sshproxy.RunProxy(listener, sshproxy.GenHostKey, addr, sshproxy.DefaultAuthMethods,
			sshproxy.AcceptAllHostKeys, filter)
		if err != nil {
			panic(err)
		}
	}
}
