//     Copyright (C) 2020-2021, IrineSistiana
//
//     This file is part of mosdns.
//
//     mosdns is free software: you can redistribute it and/or modify
//     it under the terms of the GNU General Public License as published by
//     the Free Software Foundation, either version 3 of the License, or
//     (at your option) any later version.
//
//     mosdns is distributed in the hope that it will be useful,
//     but WITHOUT ANY WARRANTY; without even the implied warranty of
//     MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//     GNU General Public License for more details.
//
//     You should have received a copy of the GNU General Public License
//     along with this program.  If not, see <https://www.gnu.org/licenses/>.

package fastforward

import (
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/IrineSistiana/mosdns/dispatcher/handler"
	"github.com/IrineSistiana/mosdns/dispatcher/utils"
	"github.com/miekg/dns"
	"go.uber.org/zap"
	"net"
	"sync"
	"testing"
	"time"
)

func newUDPTCPTestServer(t testing.TB, handler dns.Handler) (addr string, shutdownFunc func()) {
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpAddr := udpConn.LocalAddr().String()
	udpServer := dns.Server{
		PacketConn: udpConn,
		Handler:    handler,
	}
	go udpServer.ActivateAndServe()

	l, err := net.Listen("tcp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	tcpServer := dns.Server{
		Listener:      l,
		Handler:       handler,
		MaxTCPQueries: -1,
	}
	go tcpServer.ActivateAndServe()

	return udpAddr, func() {
		udpServer.Shutdown()
		tcpServer.Shutdown()
	}
}

func newTCPTestServer(t testing.TB, handler dns.Handler) (addr string, shutdownFunc func()) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr := l.Addr().String()
	tcpServer := dns.Server{
		Listener:      l,
		Handler:       handler,
		MaxTCPQueries: -1,
	}
	go tcpServer.ActivateAndServe()
	return tcpAddr, func() {
		tcpServer.Shutdown()
	}
}

func newDoTTestServer(t testing.TB, handler dns.Handler) (addr string, shutdownFunc func()) {
	serverName := "test"
	cert, err := utils.GenerateCertificate(serverName)
	tlsConfig := new(tls.Config)
	tlsConfig.Certificates = []tls.Certificate{cert}
	tlsListener, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	doTAddr := tlsListener.Addr().String()
	doTServer := dns.Server{
		Net:           "tcp-tls",
		Listener:      tlsListener,
		TLSConfig:     tlsConfig,
		Handler:       handler,
		MaxTCPQueries: -1,
	}
	go doTServer.ActivateAndServe()
	return doTAddr, func() {
		doTServer.Shutdown()
	}
}

type newTestServerFunc func(t testing.TB, handler dns.Handler) (addr string, shutdownFunc func())

func Test_fastUpstream(t *testing.T) {
	m := map[string]newTestServerFunc{
		"udp": newUDPTCPTestServer,
		"tcp": newTCPTestServer,
		"dot": newDoTTestServer,
	}

	// TODO: add test for doh
	// TODO: add test for socks5

	// server config
	for protocol, f := range m {
		for _, bigMsg := range [...]bool{true, false} {
			for _, latency := range [...]time.Duration{0, time.Millisecond * 10} {

				// client specific
				for _, idleTimeout := range [...]uint{0, 1} {
					for _, isTCPClient := range [...]bool{false, true} {

						testName := fmt.Sprintf(
							"test: protocol: %s, bigMsg: %v, latency: %s, idleTimeout: %d, isTCPClient: %v",
							protocol,
							bigMsg,
							latency,
							idleTimeout,
							isTCPClient,
						)

						t.Run(testName, func(t *testing.T) {
							addr, shutdownServer := f(t, &vServer{
								latency: latency,
								bigMsg:  bigMsg,
							})
							defer shutdownServer()

							c := &UpstreamConfig{
								Protocol:           protocol,
								Addr:               addr,
								ServerName:         "test",
								IdleTimeout:        idleTimeout,
								InsecureSkipVerify: true,
								Timeout:            1,
							}

							u, err := newFastUpstream(c, zap.NewNop(), nil)
							if err != nil {
								t.Fatal(err)
							}
							if err := testUpstream(u, isTCPClient); err != nil {
								t.Fatal(err)
							}
						})
					}
				}
			}
		}

	}
}

func testUpstream(u *fastUpstream, isTCPClient bool) error {
	wg := sync.WaitGroup{}
	errs := make([]error, 0)
	errsLock := sync.Mutex{}
	logErr := func(err error) {
		errsLock.Lock()
		errs = append(errs, err)
		errsLock.Unlock()
	}
	errsToString := func() string {
		s := fmt.Sprintf("%d err(s) occured during the test: ", len(errs))
		for i := range errs {
			s = s + errs[i].Error() + "|"
		}
		return s
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			q := new(dns.Msg)
			q.SetQuestion("example.com.", dns.TypeA)
			qCtx := handler.NewContext(q, nil)
			qCtx.SetTCPClient(isTCPClient)
			r, err := u.Exchange(qCtx)
			if err != nil {
				logErr(err)
				return
			}
			if r.Id != q.Id {
				logErr(dns.ErrId)
				return
			}
		}()
	}

	wg.Wait()
	if len(errs) != 0 {
		return errors.New(errsToString())
	}
	return nil
}

type vServer struct {
	latency time.Duration
	bigMsg  bool // with 1kb padding
}

var padding = make([]byte, 1024)

func (s *vServer) ServeDNS(w dns.ResponseWriter, q *dns.Msg) {
	r := new(dns.Msg)
	r.SetReply(q)
	if s.bigMsg {
		r.SetEdns0(dns.MaxMsgSize, false)
		opt := r.IsEdns0()
		opt.Option = append(opt.Option, &dns.EDNS0_PADDING{Padding: padding})
	}

	time.Sleep(s.latency)
	w.WriteMsg(r)
}

func Benchmark_transport(b *testing.B) {
	m := map[string]newTestServerFunc{
		"tcp": newTCPTestServer,
		"tls": newDoTTestServer,
	}

	for protocol, f := range m {
		for _, idleTimout := range [...]uint{0, 1} {
			name := fmt.Sprintf("protocol: %s, CR: %v", protocol, idleTimout != 0)
			b.Run(name+" fast_forward", func(b *testing.B) {
				addr, shutdownFunc := f(b, &vServer{})
				defer shutdownFunc()
				c := &UpstreamConfig{
					Protocol:           protocol,
					Addr:               addr,
					IdleTimeout:        idleTimout,
					ServerName:         "test",
					InsecureSkipVerify: true,
					Timeout:            1,
					MaxConns:           5,
				}

				u, err := newFastUpstream(c, zap.NewNop(), nil)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkTransport(b, u)
				//b.Logf("%d conn(s) opened", u.tcpTransport.connOpened)
			})
		}
	}
}

func Benchmark_dnsproxy(b *testing.B) {
	m := map[string]newTestServerFunc{
		"tcp": newTCPTestServer,
		"tls": newDoTTestServer,
	}

	for protocol, f := range m {
		name := fmt.Sprintf("protocol: %s", protocol)
		b.Run(name+" dnsproxy", func(b *testing.B) {
			addr, shutdownFunc := f(b, &vServer{})
			defer shutdownFunc()

			u, err := upstream.AddressToUpstream(protocol+"://"+addr, upstream.Options{
				InsecureSkipVerify: true,
			})
			if err != nil {
				b.Fatal(err)
			}
			benchmarkTransport(b, &trustedUpstream{dnsproxyUpstream: u})
		})
	}
}

type trustedUpstream struct {
	dnsproxyUpstream upstream.Upstream
	trusted          bool
}

func (u *trustedUpstream) Address() string {
	return u.dnsproxyUpstream.Address()
}

func (u *trustedUpstream) Exchange(qCtx *handler.Context) (*dns.Msg, error) {
	return u.dnsproxyUpstream.Exchange(qCtx.Q())
}

func (u *trustedUpstream) Trusted() bool {
	return u.trusted
}

func benchmarkTransport(b *testing.B, u utils.Upstream) {
	b.ReportAllocs()
	b.ResetTimer()
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	qCtx := handler.NewContext(q, nil)
	b.SetParallelism(4)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r, err := u.Exchange(qCtx)
			if err != nil {
				b.Fatal(err)
			}
			if r.Id != q.Id {
				b.Fatal()
			}
		}
	})
}
