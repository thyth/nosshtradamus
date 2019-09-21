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
	"os"
	"strings"
	"time"
)

// arrayFlags: flag.Value interface implementing type to collect multiple values of the same argument
type arrayFlags []string

func (_ *arrayFlags) String() string      { return "" }
func (af *arrayFlags) Set(v string) error { *af = append(*af, v); return nil }

func truthy(s string) bool {
	s = strings.ToLower(s)
	switch s {
	case "yes":
		fallthrough
	case "1":
		fallthrough
	case "true":
		return true
	default:
		return false
	}
}

func main() {
	port := 0
	target := ""
	printPredictiveVersion := false
	noPrediction := false
	var fakeDelay time.Duration
	var optionArgs arrayFlags
	var identityArgs arrayFlags
	agentForward := false

	flag.IntVar(&port, "port", 0, "Proxy listen port")
	flag.StringVar(&target, "target", "", "Target SSH host")
	flag.BoolVar(&printPredictiveVersion, "version", false, "Display predictive backend version")
	flag.BoolVar(&noPrediction, "nopredict", false, "Disable the mosh-based predictive backend")
	flag.DurationVar(&fakeDelay, "fakeDelay", 0, "Artificial roundtrip latency added to sessions")

	flag.Var(&optionArgs, "o", "Proxy SSH client options (repeatable)")
	flag.Var(&identityArgs, "i", "Proxy SSH client identity file paths (repeatable)")
	flag.BoolVar(&agentForward, "A", false, "Allow proxy SSH client to forward agent")
	flag.Parse()

	// create a map of SSH client options to their values
	sshClientOptions := map[string]string{}
	for _, option := range optionArgs {
		kv := strings.SplitN(option, "=", 2)
		if len(kv) == 2 {
			sshClientOptions[kv[0]] = kv[1]
		}
	}

	// default to checking known hosts from $HOME/.ssh/known_hosts
	userKnownHostsFile := ""
	if home, ok := os.LookupEnv("HOME"); ok {
		userKnownHostsFile = home + "/.ssh/known_hosts"
	}
	// unless overridden by the client
	if specifiedKnownHost, ok := sshClientOptions["UserKnownHostsFile"]; ok {
		userKnownHostsFile = specifiedKnownHost
	}

	// default to checking host keys
	strictHostChecking := true
	if specifiedStrictChecking, ok := sshClientOptions["StrictHostKeyChecking"]; ok {
		strictHostChecking = truthy(specifiedStrictChecking)
	}
	if strictHostChecking && userKnownHostsFile == "" {
		// asked for strict host key checking, but no known hosts file... die
		panic("Strict host key checking enabled, but no known_hosts provided")
	}

	// detect between 3 different modes of identity key files:
	// - none provided: use default of $HOME/.ssh/id_rsa and $HOME/.ssh/id_ed25519 (if $HOME exists)
	// - one provided equal to /dev/null: empty out the array (don't use any identity files)
	// - else: specifies a set of identity files to use (if not already in client's agent), in attempt order
	sshIdentitiesSet := map[string]string{}
	var sshIdentities []string
	if len(identityArgs) == 0 {
		defaultIdentities := []string{"/.ssh/id_rsa", "/.ssh/id_ed25519"}
		if home, ok := os.LookupEnv("HOME"); ok {
			for _, identity := range defaultIdentities {
				fn := home + identity
				if _, err := os.Stat(fn); !os.IsNotExist(err) {
					if _, exists := sshIdentitiesSet[fn]; !exists {
						sshIdentitiesSet[fn] = fn
						sshIdentities = append(sshIdentities, fn)
					}
				}
			}
		}
	} else {
		for _, fn := range identityArgs {
			if _, err := os.Stat(fn); !os.IsNotExist(err) {
				if _, exists := sshIdentitiesSet[fn]; !exists {
					sshIdentitiesSet[fn] = fn
					sshIdentities = append(sshIdentities, fn)
				}
			}
		}
	}
	if len(sshIdentities) == 1 && sshIdentities[0] == "/dev/null" {
		sshIdentities = nil
	}

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
