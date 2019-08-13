/*
 * nosshtradamus: predictive terminal emulation for SSH
 * Copyright 2019 Daniel Selifonov
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */
package main

import (
	"nosshtradamus/internal/predictive"
	"nosshtradamus/internal/sshproxy"

	"golang.org/x/crypto/ssh"

	"flag"
	"fmt"
	"io"
	"net"
	"strings"
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
	flag.DurationVar(&fakeDelay, "fakeDelay", 0, "Artificial roundtrip latency added to sessions")
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
					wrapped = predictive.RingDelay(wrapped, fakeDelay, 512)
				}
				if !noPrediction {
					options := predictive.GetDefaultInterposerOptions()
					interposer := predictive.Interpose(wrapped, func(interposer *predictive.Interposer, epoch uint64, openedAt time.Time) {
						fmt.Printf("Ping %d\n", epoch)
						if fakeDelay > 0 {
							time.Sleep(fakeDelay)
						}
						_, _ = sshChannel.SendRequest(fmt.Sprintf("nosshtradamus/ping/%d", epoch), true, nil)

						fmt.Printf("Pong %d - (%v)\n", epoch, time.Now().Sub(openedAt))
						time.Sleep(time.Second / 60) // delay closing of the epoch by one frame (???)
						interposer.CloseEpoch(epoch, openedAt)
					}, options)
					reqFilter = func(sink sshproxy.ChannelRequestSink) sshproxy.ChannelRequestSink {
						return func(recipient ssh.Channel, sender <-chan *ssh.Request) {
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
								case "nosshtradamus/displayPreference":
									preference := strings.ToLower(string(request.Payload))
									switch preference {
									case "always":
										interposer.ChangeDisplayPreference(predictive.PredictAlways)
										if request.WantReply {
											_ = request.Reply(true, nil)
										}
									case "never":
										interposer.ChangeDisplayPreference(predictive.PredictNever)
										if request.WantReply {
											_ = request.Reply(true, nil)
										}
									case "adaptive":
										interposer.ChangeDisplayPreference(predictive.PredictAdaptive)
										if request.WantReply {
											_ = request.Reply(true, nil)
										}
									case "experimental":
										interposer.ChangeDisplayPreference(predictive.PredictExperimental)
										if request.WantReply {
											_ = request.Reply(true, nil)
										}
									}
									continue // do not pass through the proxy
								case "nosshtradamus/predictOverwrite":
									setting := strings.ToLower(string(request.Payload))
									switch setting {
									case "true":
										fallthrough
									case "1":
										interposer.ChangeOverwritePrediction(true)
										if request.WantReply {
											_ = request.Reply(true, nil)
										}
									case "false":
										fallthrough
									case "0":
										interposer.ChangeOverwritePrediction(false)
										if request.WantReply {
											_ = request.Reply(true, nil)
										}
									default:
										// invalid setting
										if request.WantReply {
											_ = request.Reply(false, nil)
										}
									}
									continue // do not pass through the proxy
								}
								passthrough <- request
							}
							close(passthrough)
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
