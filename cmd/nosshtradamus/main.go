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
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"flag"
	"fmt"
	"io"
	"io/ioutil"
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

type deferredSigner struct {
	actual    ssh.Signer
	force     func(*deferredSigner) error
	internPub ssh.PublicKey
}

func (ds *deferredSigner) PublicKey() ssh.PublicKey {
	if ds.internPub != nil {
		return ds.internPub
	}
	if ds.actual == nil {
		_ = ds.force(ds)
	}
	return ds.actual.PublicKey()
}
func (ds *deferredSigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	if ds.actual == nil {
		if err := ds.force(ds); err != nil {
			return nil, err
		}
	}
	return ds.actual.Sign(rand, data)
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
	disableAgent := false
	dumbAuth := false
	authErrDetails := false
	printTiming := false
	noBanner := false

	flag.IntVar(&port, "port", 0, "Proxy listen port")
	flag.StringVar(&target, "target", "", "Target SSH host")
	flag.BoolVar(&printPredictiveVersion, "version", false, "Display predictive backend version")
	flag.BoolVar(&noPrediction, "nopredict", false, "Disable the mosh-based predictive backend")
	flag.DurationVar(&fakeDelay, "fakeDelay", 0, "Artificial roundtrip latency added to sessions")
	flag.BoolVar(&printTiming, "printTiming", false, "Print epoch synchronization timing messages")
	flag.BoolVar(&noBanner, "noBanner", false, "Disable the Nosshtradamus proxy banner")

	flag.Var(&optionArgs, "o", "Proxy `SSH client option`s (repeatable)")
	flag.Var(&identityArgs, "i", "Proxy SSH client `identity file path`s (repeatable)")
	flag.BoolVar(&agentForward, "A", false, "Allow proxy SSH client to forward agent")
	flag.BoolVar(&disableAgent, "a", false, "Disable use of SSH agent for key based authentication")
	flag.BoolVar(&dumbAuth, "dumbauth", false, "Use 'dumb' authentication (send blank password)")
	flag.BoolVar(&authErrDetails, "authErr", false, "Show details on authentication errors with target")
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
	hostKeyChecker := sshproxy.AcceptAllHostKeys
	if specifiedStrictChecking, ok := sshClientOptions["StrictHostKeyChecking"]; ok {
		strictHostChecking = truthy(specifiedStrictChecking)
	}
	if strictHostChecking && userKnownHostsFile == "" {
		// asked for strict host key checking, but no known hosts file... die
		panic("Strict host key checking enabled, but no known_hosts provided")
	}
	if strictHostChecking {
		var err error
		hostKeyChecker, err = knownhosts.New(userKnownHostsFile)
		if err != nil {
			panic(err)
		}
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

	authMethods := sshproxy.DefaultAuthMethods
	var extraQuestions chan *sshproxy.ProxiedAuthQuestion
	if !dumbAuth {
		var signers []ssh.Signer
		keySet := map[string]string{}
		// keys from the agent
		if !disableAgent {
			if agentSocket, ok := os.LookupEnv("SSH_AUTH_SOCK"); ok {
				if agentConn, err := net.Dial("unix", agentSocket); err == nil {
					sshAgent := agent.NewClient(agentConn)
					if agentSigners, err := sshAgent.Signers(); err == nil {
						for _, agentSigner := range agentSigners {
							publicKeyIdentity := fmt.Sprintf("%x", agentSigner.PublicKey().Marshal())
							if _, present := keySet[publicKeyIdentity]; !present {
								signers = append(signers, agentSigner)
								keySet[publicKeyIdentity] = publicKeyIdentity
							}
						}
					}
				}
			}
		}
		// keys from identities -- might be password protected
		extraQuestions = make(chan *sshproxy.ProxiedAuthQuestion)
		for _, sshIdentity := range sshIdentities {
			if keyBytes, err := ioutil.ReadFile(sshIdentity); err == nil {
				if signer, err := ssh.ParsePrivateKey(keyBytes); err == nil {
					// unencrypted private key
					publicKeyIdentity := fmt.Sprintf("%x", signer.PublicKey().Marshal())
					if _, present := keySet[publicKeyIdentity]; !present {
						signers = append(signers, signer)
						keySet[publicKeyIdentity] = publicKeyIdentity
					}
				} else if err.Error() == "ssh: cannot decode encrypted private keys" {
					// XXX: Brittle hack -- no dedicated sentinel error for private key decoding in SSH library.
					// create a deferred key, and ask for a password when asked to sign with it (via extra questions)
					if pubKeyBytes, err := ioutil.ReadFile(sshIdentity + ".pub"); err == nil {
						if pubKey, _, _, _, err := ssh.ParseAuthorizedKey(pubKeyBytes); err == nil {
							publicKeyIdentity := fmt.Sprintf("%x", pubKey.Marshal())
							if _, present := keySet[publicKeyIdentity]; !present {
								signers = append(signers, &deferredSigner{
									internPub: pubKey,
									force: func(ds *deferredSigner) error {
										answer := make(chan error, 1)
										extraQuestions <- &sshproxy.ProxiedAuthQuestion{
											Message: fmt.Sprintf("Enter password for '%s'", sshIdentity),
											Prompt:  "Password: ",
											Echo:    false,
											OnAnswer: func(password string) bool {
												if decryptedSigner, err := ssh.ParsePrivateKeyWithPassphrase(keyBytes,
													[]byte(password)); err == nil {
													ds.actual = decryptedSigner
													close(answer)
													return true
												} else {
													answer <- err
													return false
												}
											},
										}
										return <-answer
									},
								})
								keySet[publicKeyIdentity] = publicKeyIdentity
							}
						}
					}
				}
			}
		}

		authMethods = []ssh.AuthMethod{
			ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
				return signers, nil
			}),
			ssh.KeyboardInteractive(func(_, instruction string, questions []string, echos []bool) ([]string, error) {
				var answers []string
				answer := make(chan string, 1)
				for idx, question := range questions {
					echo := echos[idx]
					extraQuestions <- &sshproxy.ProxiedAuthQuestion{
						Message: instruction,
						Prompt:  question,
						Echo:    echo,
						OnAnswer: func(response string) bool {
							answer <- response
							return true
						},
					}
					answers = append(answers, <-answer)
				}
				return answers, nil
			}),
			ssh.PasswordCallback(func() (string, error) {
				passwd := make(chan string, 1)
				extraQuestions <- &sshproxy.ProxiedAuthQuestion{
					Prompt: "[*] Password: ",
					Echo:   false,
					OnAnswer: func(password string) bool {
						passwd <- password
						return true
					},
				}
				return <-passwd, nil
			}),
		}
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
				ioSwitch := predictive.MakeIoSwitch(sshChannel)
				wrapped = ioSwitch

				if !noPrediction {
					activated := false
					var interposer *predictive.Interposer
					activateInterposer := func() {
						if activated {
							return
						}
						activated = true
						var wrapped io.ReadWriteCloser
						wrapped = sshChannel
						if fakeDelay > 0 {
							wrapped = predictive.RingDelay(wrapped, fakeDelay, 512)
						}
						options := predictive.GetDefaultInterposerOptions()
						interposer = predictive.Interpose(wrapped, func(interposer *predictive.Interposer,
							epoch uint64, openedAt time.Time) {
							if printTiming {
								fmt.Printf("Ping %d\n", epoch)
							}
							if fakeDelay > 0 {
								time.Sleep(fakeDelay)
							}
							_, _ = sshChannel.SendRequest(fmt.Sprintf("nosshtradamus/ping/%d", epoch),
								true, nil)

							if printTiming {
								fmt.Printf("Pong %d - (%v)\n", epoch, time.Now().Sub(openedAt))
							}
							time.Sleep(time.Second / 60) // delay closing of the epoch by one frame (???)
							interposer.CloseEpoch(epoch, openedAt)
						}, options)
						wrapped = interposer

						ioSwitch.Enable(wrapped)
					}

					reqFilter = func(sink sshproxy.ChannelRequestSink) sshproxy.ChannelRequestSink {
						return func(recipient ssh.Channel, sender <-chan *ssh.Request) {
							// capture and process a subset of requests prior to forwarding them
							passthrough := make(chan *ssh.Request)
							go sink(recipient, passthrough)
							for request := range sender {
								switch request.Type {
								case "pty-req":
									ptyreq, err := sshproxy.InterpretPtyReq(request.Payload)
									if err == nil {
										activateInterposer()
										interposer.Resize(int(ptyreq.Width), int(ptyreq.Height))
									}
								case "window-change":
									winch, err := sshproxy.InterpretWindowChange(request.Payload)
									if err == nil && interposer != nil {
										interposer.Resize(int(winch.Width), int(winch.Height))
									}
								case "nosshtradamus/displayPreference":
									if interposer == nil {
										continue
									}
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
									if interposer == nil {
										continue
									}
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
		banner := func(conn ssh.ConnMetadata) string {
			return fmt.Sprintf("Nosshtradamus proxying ~ %s@%v\n", conn.User(), target)
		}
		if noBanner {
			banner = nil
		}
		err = sshproxy.RunProxy(listener, addr, &sshproxy.ProxyConfig{
			KeyProvider:      sshproxy.GenHostKey,
			TargetKeyChecker: hostKeyChecker,
			ChannelFilter:    filter,
			AuthMethods:      authMethods,
			Banner:           banner,
			ReportAuthErr:    authErrDetails,
			ExtraQuestions:   extraQuestions,
			BlockAgent:       !agentForward,
		})
		if err != nil {
			panic(err)
		}
	}
}
