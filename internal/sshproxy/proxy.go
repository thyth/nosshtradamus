package sshproxy

import (
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/ssh"

	"fmt"
	"io"
	"net"
	"time"
)

// create a new SSH host key
func genHostKey() (ssh.Signer, error) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(privateKey)
}

// connect to any SSH server without checking the host key
func acceptAllHostKeys(_ string, _ net.Addr, _ ssh.PublicKey) error {
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
	defaultAuthMethods = []ssh.AuthMethod{
		ssh.Password(""),
		ssh.KeyboardInteractive(blankInteractive),
	}
)

func DumbTransparentProxy(port int, target net.Addr) error {
	var proxyConn *ssh.Client

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	config := &ssh.ServerConfig{
		KeyboardInteractiveCallback: func(conn ssh.ConnMetadata,
			challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {

			user := conn.User()
			// try connecting to the remote host at proxy challenge authentication time
			clientConn, err := ssh.Dial("tcp", target.String(), &ssh.ClientConfig{
				User:            user,
				Timeout:         defaultTimeout,
				HostKeyCallback: acceptAllHostKeys,
				Auth:            defaultAuthMethods,
			})
			if err != nil {
				return nil, err
			}
			proxyConn = clientConn

			// send blank challenge so that the user is not prompted to authenticate
			challenge(user, "", []string{}, []bool{})
			return nil, nil
		},
	}
	hostKey, err := genHostKey()
	if err != nil {
		return err
	}
	config.AddHostKey(hostKey)

	for {
		// accept exactly one connection at a time
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
		if err != nil {
			continue
		}
		// reflect connection level requests from the client; can the server initiate such requests, or just reply?
		go reflectGlobalRequests(proxyConn, reqs)

		handleSshClientChannels(proxyConn, sshConn, chans)
	}
}

func handleSshClientChannels(proxyConn *ssh.Client, client *ssh.ServerConn, nc <-chan ssh.NewChannel) {
	for channelRequest := range nc {
		go handleSshClientChannel(proxyConn, client, channelRequest)
	}
}

func handleSshClientChannel(proxyConn *ssh.Client, _ *ssh.ServerConn, request ssh.NewChannel) {
	proxyChan, proxyReqs, err := proxyConn.OpenChannel(request.ChannelType(), request.ExtraData())
	if err != nil {
		if openChanErr, ok := err.(*ssh.OpenChannelError); ok {
			request.Reject(openChanErr.Reason, openChanErr.Message)
		} else {
			request.Reject(ssh.ConnectionFailed, err.Error())
		}
	}

	clientChan, clientReqs, err := request.Accept()
	if err != nil {
		proxyChan.Close()
		return
	}

	clientClosed := make(chan interface{})
	serverClosed := make(chan interface{})

	// copy data across both channels
	go func() {
		io.Copy(proxyChan, clientChan) // client closed connection for channel writes
		proxyChan.CloseWrite()
		close(clientClosed)
	}()
	go func() {
		io.Copy(clientChan, proxyChan) // server closed connection for channel writes
		clientChan.CloseWrite()
		close(serverClosed)
	}()

	// copy requests across both channels
	go func() {
		reflectRequests(proxyChan, clientReqs) // client closed connection for channel requests
		<-clientClosed
		proxyChan.Close()
	}()
	go func() {
		reflectRequests(clientChan, proxyReqs) // server closed connection for channel requests
		<-serverClosed
		clientChan.Close()
	}()
}

func reflectRequests(recipient ssh.Channel, sender <-chan *ssh.Request) {
	for request := range sender {
		reply, err := recipient.SendRequest(request.Type, request.WantReply, request.Payload)
		if request.WantReply {
			if err != nil {
				request.Reply(false, nil)
			} else {
				// Note: (at least in the Go x.crypto SSH library) payload argument is ignored for channel-specific
				//       requests. This behavior appears to be defined in RFC4254 section 5.4, where clients can send
				//       multiple messages without waiting for responses.
				request.Reply(reply, nil)
			}
		}
	}
}

func reflectGlobalRequests(recipient ssh.Conn, sender <-chan *ssh.Request) {
	for request := range sender {
		reply, payload, err := recipient.SendRequest(request.Type, request.WantReply, request.Payload)
		if request.WantReply {
			if err != nil {
				request.Reply(false, nil)
			} else {
				request.Reply(reply, payload)
			}
		}
	}
}
