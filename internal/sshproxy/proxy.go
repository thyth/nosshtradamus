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
package sshproxy

import (
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/ssh"

	"fmt"
	"io"
	"net"
	"time"
)

type ProxyConfig struct {
	KeyProvider      HostKeyProvider
	TargetKeyChecker ssh.HostKeyCallback
	ChannelFilter    ChannelStreamFilter
	AuthMethods      []ssh.AuthMethod
	Banner           func(conn ssh.ConnMetadata) string
	ReportAuthErr    bool
	ExtraQuestions   chan *ProxiedAuthQuestion
}

type ProxiedAuthQuestion struct {
	Message  string
	Prompt   string
	Echo     bool
	OnAnswer func(string) bool
}

type HostKeyProvider func() (ssh.Signer, error)

// create a new SSH host key
func GenHostKey() (ssh.Signer, error) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(privateKey)
}

// connect to any SSH server without checking the host key
func AcceptAllHostKeys(_ string, _ net.Addr, _ ssh.PublicKey) error {
	return nil
}

// send blank answers when authenticating
func blankInteractive(_, _ string, questions []string, _ []bool) ([]string, error) {
	answers := make([]string, len(questions))
	for idx := range answers {
		answers[idx] = ""
	}
	return answers, nil
}

var (
	defaultTimeout     = 3 * time.Second
	DefaultAuthMethods = []ssh.AuthMethod{
		ssh.Password(""),
		ssh.KeyboardInteractive(blankInteractive),
	}
)

// A ChannelStreamFilter optionally encapsulates/wraps an SSH channel of the specified channel type.
type ChannelStreamFilter func(channelType string, c ssh.Channel) (io.ReadWriteCloser, ChannelRequestFilter)

// A ChannelRequestSink encapsulates a channel of SSH requests being sent to a recipient channel.
type ChannelRequestSink func(recipient ssh.Channel, sender <-chan *ssh.Request)

// A ChannelRequestFilter takes one request sink and outputs one that may watch, filter, transform those requests.
type ChannelRequestFilter func(sink ChannelRequestSink) ChannelRequestSink

func RunProxy(listener net.Listener, target net.Addr, configOpts *ProxyConfig) error {
	keyProvider := configOpts.KeyProvider
	auth := configOpts.AuthMethods
	keyCallback := configOpts.TargetKeyChecker
	filter := configOpts.ChannelFilter
	reportAuthErr := configOpts.ReportAuthErr
	banner := configOpts.Banner

	var proxyConn *ssh.Client
	config := &ssh.ServerConfig{
		// Note: To make this usable as a generic client-side wrapper for the 'ssh' binary, need to add key and password
		//       authentication mechanisms, local agent forwarding, et cetra.
		KeyboardInteractiveCallback: func(conn ssh.ConnMetadata,
			challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			user := conn.User()
			// connecting to the remote host only when the proxy has enough information to make the connection
			clientConn, err := ssh.Dial("tcp", target.String(), &ssh.ClientConfig{
				User:            user,
				Timeout:         defaultTimeout,
				HostKeyCallback: keyCallback,
				Auth:            auth,
			})
			if err != nil {
				if reportAuthErr {
					_, _ = challenge(user, err.Error(), []string{}, []bool{})
				}
				return nil, err
			}
			proxyConn = clientConn

			asked := false
			if configOpts.ExtraQuestions != nil {
				// ask any supplemental questions; one at a time
				for question := range configOpts.ExtraQuestions {
					asked = true
					answers, err := challenge(user, question.Message, []string{question.Prompt}, []bool{question.Echo})
					if err != nil {
						return nil, err
					}
					if len(answers) != 1 {
						return nil, fmt.Errorf("expected 1 answer, got %d", len(answers))
					}
					if !question.OnAnswer(answers[0]) {
						return nil, fmt.Errorf("wrong answer to %s", question.Message)
					}
				}
			}
			if !asked {
				// send blank challenge so that the user is not prompted to authenticate
				_, _ = challenge(user, "", []string{}, []bool{})
			}
			return nil, nil
		},
		MaxAuthTries:   1,
		BannerCallback: banner,
	}
	hostKey, err := keyProvider()
	if err != nil {
		return err
	}
	config.AddHostKey(hostKey)

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}

		sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
		if err != nil {
			continue
		}
		go func(proxyConn *ssh.Client, sshConn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
			// reflect connection level requests from the client; can the server initiate such requests, or just reply?
			go reflectGlobalRequests(proxyConn, reqs)

			handleSshClientChannels(proxyConn, sshConn, chans, filter)

			_ = proxyConn.Close()
		}(proxyConn, sshConn, chans, reqs)
	}
}

func handleSshClientChannels(proxyConn *ssh.Client, client *ssh.ServerConn, nc <-chan ssh.NewChannel,
	filter ChannelStreamFilter) {
	for channelRequest := range nc {
		go handleSshClientChannel(proxyConn, client, channelRequest, filter)
	}
}

func handleSshClientChannel(proxyConn *ssh.Client, _ *ssh.ServerConn, request ssh.NewChannel,
	filter ChannelStreamFilter) {

	chanType := request.ChannelType()
	proxyChan, proxyReqs, err := proxyConn.OpenChannel(chanType, request.ExtraData())
	if err != nil {
		if openChanErr, ok := err.(*ssh.OpenChannelError); ok {
			_ = request.Reject(openChanErr.Reason, openChanErr.Message)
		} else {
			_ = request.Reject(ssh.ConnectionFailed, err.Error())
		}
	}

	clientChan, clientReqs, err := request.Accept()
	if err != nil {
		_ = proxyChan.Close()
		return
	}

	clientClosed := make(chan interface{})
	serverClosed := make(chan interface{})

	var copyTarget io.ReadWriteCloser
	var requestFilter ChannelRequestFilter
	if filter != nil {
		copyTarget, requestFilter = filter(chanType, proxyChan)
	}
	if copyTarget == nil {
		copyTarget = proxyChan
	}
	var clientRequestSink ChannelRequestSink = reflectRequests
	if requestFilter != nil {
		clientRequestSink = requestFilter(clientRequestSink)
	}

	// copy data across both channels
	go func() {
		_, _ = io.Copy(copyTarget, clientChan) // client closed connection for channel writes
		_ = proxyChan.CloseWrite()
		close(clientClosed)
	}()
	go func() {
		_, _ = io.Copy(clientChan, copyTarget) // server closed connection for channel writes
		_ = clientChan.CloseWrite()
		close(serverClosed)
	}()

	// copy requests across both channels
	go func() {
		clientRequestSink(proxyChan, clientReqs) // client closed connection for channel requests
		<-clientClosed
		_ = proxyChan.Close()
		if copyTarget != proxyChan {
			_ = copyTarget.Close()
		}
	}()
	go func() {
		reflectRequests(clientChan, proxyReqs) // server closed connection for channel requests
		<-serverClosed
		_ = clientChan.Close()
	}()
}

func reflectRequests(recipient ssh.Channel, sender <-chan *ssh.Request) {
	for request := range sender {
		reply, err := recipient.SendRequest(request.Type, request.WantReply, request.Payload)
		if request.WantReply {
			if err != nil {
				_ = request.Reply(false, nil)
			} else {
				// Note: (at least in the Go x.crypto SSH library) payload argument is ignored for channel-specific
				//       requests. This behavior appears to be defined in RFC4254 section 5.4, where clients can send
				//       multiple messages without waiting for responses.
				_ = request.Reply(reply, nil)
			}
		}
	}
}

func reflectGlobalRequests(recipient ssh.Conn, sender <-chan *ssh.Request) {
	for request := range sender {
		reply, payload, err := recipient.SendRequest(request.Type, request.WantReply, request.Payload)
		if request.WantReply {
			if err != nil {
				_ = request.Reply(false, nil)
			} else {
				_ = request.Reply(reply, payload)
			}
		}
	}
}
