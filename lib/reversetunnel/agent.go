/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package reversetunnel sets up persistent reverse tunnel
// between remote site and teleport proxy, when site agents
// dial to teleport proxy's socket and teleport proxy can connect
// to any server through this tunnel.
package reversetunnel

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/codahale/lunk"
	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
)

// Agent is a reverse tunnel running on site and
type Agent struct {
	log             *log.Entry
	addr            utils.NetAddr
	elog            lunk.EventLogger
	clt             *auth.TunClient
	domainName      string
	waitC           chan bool
	disconnectC     chan bool
	conn            ssh.Conn
	hostKeyCallback utils.HostKeyCallback
	authMethods     []ssh.AuthMethod
}

// AgentOption specifies parameter that could be passed to Agents
type AgentOption func(a *Agent) error

// SetEventLogger sets structured logger for the agent
func SetEventLogger(e lunk.EventLogger) AgentOption {
	return func(s *Agent) error {
		s.elog = e
		return nil
	}
}

// NewAgent returns a new reverse tunnel agent
func NewAgent(addr utils.NetAddr, domainName string, signers []ssh.Signer,
	clt *auth.TunClient, options ...AgentOption) (*Agent, error) {

	a := &Agent{
		log: log.WithFields(log.Fields{
			"module": "agent",
			"remote": addr,
		}),
		clt:         clt,
		addr:        addr,
		domainName:  domainName,
		waitC:       make(chan bool),
		disconnectC: make(chan bool, 10),
		authMethods: []ssh.AuthMethod{ssh.PublicKeys(signers...)},
	}
	a.hostKeyCallback = a.checkHostSignature
	for _, o := range options {
		if err := o(a); err != nil {
			return nil, err
		}
	}
	if a.elog == nil {
		a.elog = utils.NullEventLogger
	}
	return a, nil
}

// NewHangoutAgent returns a new "Hangout"-flavored version of reverse
// tunnel agent
func NewHangoutAgent(addr utils.NetAddr, hangoutID string,
	authMethods []ssh.AuthMethod,
	hostKeyCallback utils.HostKeyCallback,
	clt *auth.TunClient, options ...AgentOption) (*Agent, error) {

	a := &Agent{
		log: log.WithFields(log.Fields{
			"module": "reversetunnel.agent",
			"remote": addr,
		}),
		clt:             clt,
		addr:            addr,
		domainName:      hangoutID,
		waitC:           make(chan bool),
		disconnectC:     make(chan bool, 10),
		authMethods:     authMethods,
		hostKeyCallback: hostKeyCallback,
	}
	for _, o := range options {
		if err := o(a); err != nil {
			return nil, err
		}
	}
	if a.elog == nil {
		a.elog = utils.NullEventLogger
	}
	return a, nil
}

// Start starts agent that attempts to connect to remote server part
func (a *Agent) Start() error {
	if err := a.reconnect(); err != nil {
		return trace.Wrap(err)
	}
	go a.handleDisconnect()
	return nil
}

func (a *Agent) handleDisconnect() {
	a.log.Infof("handle disconnects")
	for {
		select {
		case <-a.disconnectC:
			a.log.Infof("detected disconnect, reconnecting")
			a.reconnect()
		}
	}
}

func (a *Agent) reconnect() error {
	var err error
	i := 0
	for {
		i++
		if err = a.connect(); err != nil {
			a.log.Infof("connect attempt %v: %v", i, err)
			time.Sleep(time.Duration(min(i, 10)) * time.Second)
			continue
		}
		return nil
	}
}

// Wait waits until all outstanding operations are completed
func (a *Agent) Wait() error {
	<-a.waitC
	return nil
}

// String returns debug-friendly
func (a *Agent) String() string {
	return fmt.Sprintf("tunagent(remote=%v)", a.addr)
}

func (a *Agent) checkHostSignature(hostport string, remote net.Addr, key ssh.PublicKey) error {
	cert, ok := key.(*ssh.Certificate)
	if !ok {
		return trace.Errorf("expected certificate")
	}
	cas, err := a.clt.GetCertAuthorities(services.HostCA)
	if err != nil {
		return trace.Wrap(err, "failed to fetch remote certs")
	}
	for _, ca := range cas {
		a.log.Infof("checking %v against host %v", ca.ID())
		checkers, err := ca.Checkers()
		if err != nil {
			return trace.Errorf("error parsing key: %v", err)
		}
		for _, checker := range checkers {
			if sshutils.KeysEqual(checker, cert.SignatureKey) {
				a.log.Infof("matched key %v for %v", ca.ID(), hostport)
				return nil
			}
		}
	}
	return trace.Wrap(&teleport.NotFoundError{
		Message: "no matching keys found when checking server's host signature",
	})
}

func (a *Agent) connect() error {
	if a.addr.IsEmpty() {
		err := trace.Wrap(
			teleport.BadParameter("addr",
				"reverse tunnel cannot be created: target address is empty"))
		a.log.Error(err)
		return err
	}
	a.log.Infof("agent connect")

	var c *ssh.Client
	var err error
	for _, authMethod := range a.authMethods {
		c, err = ssh.Dial(a.addr.AddrNetwork, a.addr.Addr, &ssh.ClientConfig{
			User:            a.domainName,
			Auth:            []ssh.AuthMethod{authMethod},
			HostKeyCallback: a.hostKeyCallback,
		})
		if c != nil {
			break
		}
	}
	if c == nil {
		a.log.Errorf("connect err: %v", err)
		return trace.Wrap(err)
	}

	a.conn = c

	go a.startHeartbeat()
	go a.handleAccessPoint(c.HandleChannelOpen(chanAccessPoint))
	go a.handleTransport(c.HandleChannelOpen(chanTransport))

	a.log.Infof("connection established")
	return nil
}

func (a *Agent) handleAccessPoint(newC <-chan ssh.NewChannel) {
	for {
		nch := <-newC
		if nch == nil {
			a.log.Infof("connection closed, return")
			return
		}
		a.log.Infof("got access point request: %v", nch.ChannelType())
		ch, req, err := nch.Accept()
		if err != nil {
			a.log.Errorf("failed to accept request: %v", err)
		}
		go a.proxyAccessPoint(ch, req)
	}
}

func (a *Agent) handleTransport(newC <-chan ssh.NewChannel) {
	for {
		nch := <-newC
		if nch == nil {
			a.log.Infof("connection closed, returing")
			return
		}
		a.log.Infof("got transport request: %v", nch.ChannelType())
		ch, req, err := nch.Accept()
		if err != nil {
			a.log.Errorf("failed to accept request: %v", err)
		}
		go a.proxyTransport(ch, req)
	}
}

func (a *Agent) proxyAccessPoint(ch ssh.Channel, req <-chan *ssh.Request) {
	defer ch.Close()

	conn, err := a.clt.GetDialer()()
	if err != nil {
		a.log.Errorf("%v error dialing: %v", a, err)
		return
	}

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer conn.Close()
		io.Copy(conn, ch)
	}()

	go func() {
		defer wg.Done()
		defer conn.Close()
		io.Copy(ch, conn)
	}()

	wg.Wait()
}

func (a *Agent) proxyTransport(ch ssh.Channel, reqC <-chan *ssh.Request) {
	defer ch.Close()

	var req *ssh.Request
	select {
	case req = <-reqC:
		if req == nil {
			a.log.Infof("connection closed, returning")
			return
		}
	case <-time.After(10 * time.Second):
		a.log.Errorf("timeout waiting for dial")
		return
	}

	server := string(req.Payload)
	log.Infof("got out of band request %v", server)

	conn, err := net.Dial("tcp", server)
	if err != nil {
		log.Errorf("failed to dial: %v, err: %v", server, err)
		return
	}
	req.Reply(true, []byte("connected"))

	a.log.Infof("successfully dialed to %v, start proxying", server)

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(conn, ch)
	}()

	go func() {
		defer wg.Done()
		io.Copy(ch, conn)
	}()

	wg.Wait()
}

func (a *Agent) startHeartbeat() {
	defer func() {
		a.disconnectC <- true
		a.log.Infof("sent disconnect message")
	}()

	hb, reqC, err := a.conn.OpenChannel(chanHeartbeat, nil)
	if err != nil {
		a.log.Errorf("failed to open channel: %v", err)
		return
	}

	closeC := make(chan bool)
	errC := make(chan error, 2)

	go func() {
		for {
			select {
			case <-closeC:
				a.log.Infof("asked to exit")
				return
			default:
			}
			_, err := hb.SendRequest("ping", false, nil)
			if err != nil {
				a.log.Errorf("failed to send heartbeat: %v", err)
				errC <- err
				return
			}
			time.Sleep(heartbeatPeriod)
		}
	}()

	go func() {
		for {
			select {
			case <-closeC:
				log.Infof("asked to exit")
				return
			case req := <-reqC:
				if req == nil {
					errC <- trace.Errorf("heartbeat: connection closed")
					return
				}
				a.log.Infof("got out of band request: %v", req)
			}
		}
	}()

	a.log.Infof("got error: %v", <-errC)
	close(closeC)
}

const (
	chanHeartbeat   = "teleport-heartbeat"
	chanAccessPoint = "teleport-access-point"
	chanTransport   = "teleport-transport"

	chanTransportDialReq = "teleport-transport-dial"

	heartbeatPeriod = 5 * time.Second
)

const (
	// RemoteSiteStatusOffline indicates that site is considered as
	// offline, since it has missed a series of heartbeats
	RemoteSiteStatusOffline = "offline"
	// RemoteSiteStatusOnline indicates that site is sending heartbeats
	// at expected interval
	RemoteSiteStatusOnline = "online"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
