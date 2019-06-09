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

	var filter sshproxy.ChannelStreamFilter
	if !noPrediction || fakeDelay > 0 {
		filter = func(chanType string, sshChannel ssh.Channel) (io.ReadWriteCloser, sshproxy.ChannelRequestFilter) {
			var wrapped io.ReadWriteCloser
			var reqFilter sshproxy.ChannelRequestFilter

			if chanType == "session" {
				wrapped = sshChannel
				if fakeDelay > 0 {
					wrapped = predictive.Delay(wrapped, fakeDelay)
				}
				if !noPrediction {
					interposer := predictive.Interpose(wrapped, predictive.DefaultCoalesceInterval, 1, 1)
					reqFilter = func(sink sshproxy.ChannelRequestSink) sshproxy.ChannelRequestSink {
						return func (recipient ssh.Channel, sender <-chan *ssh.Request) {
							// capture and process a subset of requests prior to forwarding them
							passthrough := make(chan *ssh.Request)
							go sink(recipient, passthrough)
							for request := range sender {
								switch request.Type {
								case "shell":
									// do we need to enforce that pty-req has been requested before shell?
								case "pty-req":
									ptyreq, err := sshproxy.InterpretPtyReq(request.Payload)
									if err == nil {
										interposer.Resize(int(ptyreq.Width), int(ptyreq.Height))
									}
								case "window-change":
									winch, err := sshproxy.InterpretWindowChange(request.Payload)
									if err == nil {
										interposer.Resize(int(winch.Width), int(winch.Height))
									}
								}
								passthrough <- request
							}
						}
					}
					wrapped = interposer
				}

				if wrapped == sshChannel {
					wrapped = nil
				}
			}

			return wrapped, reqFilter
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
