package direct

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"wanctl/internal/protocol"
)

var defaultSTUNServers = []string{
	"stun.miwifi.com:3478", "stun.chat.bilibili.com:3478", "stun.hitv.com:3478",
	"stun.l.google.com:19302", "global.stun.twilio.com:3478", "stun.cloudflare.com:3478",
}

// Settings keeps incident-response and test controls out of the user-facing CLI.
// A non-nil empty STUNServers list suppresses all STUN traffic.
type Settings struct {
	MinBytes      int64
	Hold          time.Duration
	Budget        time.Duration
	AckWait       time.Duration
	NoProgress    time.Duration
	STUNServers   []string
	AllowLoopback bool
	OpenSocket    func(network string, addr *net.UDPAddr) (*net.UDPConn, error)
}

func (s Settings) Effective() Settings {
	if s.MinBytes == 0 {
		s.MinBytes = 8 << 20
	}
	if s.Hold == 0 {
		s.Hold = 30 * time.Second
	}
	if s.Budget == 0 {
		s.Budget = 2500 * time.Millisecond
	}
	if s.AckWait == 0 {
		s.AckWait = 5 * time.Second
	}
	if s.NoProgress == 0 {
		s.NoProgress = 30 * time.Second
	}
	if s.STUNServers == nil {
		s.STUNServers = defaultSTUNServers
	}
	if s.OpenSocket == nil {
		s.OpenSocket = net.ListenUDP
	}
	return s
}

type packet struct {
	data []byte
	from net.Addr
	sock *socket
}

type socket struct {
	udp    *net.UDPConn
	tr     *quic.Transport
	family string
}

type Endpoint struct {
	settings       Settings
	sockets        []*socket
	cert           tls.Certificate
	info           protocol.DirectInfo
	packets        chan packet
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	mu             sync.Mutex
	listeners      []*quic.Listener
	listenerCancel context.CancelFunc
	accepted       map[*quic.Conn]struct{}
	closedToNew    bool
	closeOnce      sync.Once
}

func (e *Endpoint) Info() *protocol.DirectInfo {
	info := e.info
	info.Candidates = append([]string(nil), info.Candidates...)
	return &info
}

func Open(ctx context.Context, settings Settings) (_ *Endpoint, err error) {
	s := settings.Effective()
	e := &Endpoint{settings: s, packets: make(chan packet, 128), accepted: make(map[*quic.Conn]struct{})}
	readerCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	defer func() {
		if err != nil {
			e.Close()
		}
	}()
	hosts, hasV6 := interfaceHosts(s.AllowLoopback)
	if s.AllowLoopback {
		kept := hosts[:0]
		for _, h := range hosts {
			if h.addr.IsLoopback() {
				kept = append(kept, h)
			}
		}
		hosts = kept
		for _, h := range hosts {
			if h.addr.Is6() {
				hasV6 = true
			}
		}
	}
	defaults := make([]netip.Addr, 0, 2)
	if !s.AllowLoopback {
		if ip, ok := defaultRouteIP("udp4", "1.1.1.1:53"); ok {
			defaults = append(defaults, ip)
		}
		if ip, ok := defaultRouteIP("udp6", "[2606:4700:4700::1111]:53"); ok {
			defaults = append(defaults, ip)
			hasV6 = true
		}
	}
	open := func(network string, ip net.IP) error {
		udp, openErr := s.OpenSocket(network, &net.UDPAddr{IP: ip})
		if openErr != nil {
			return openErr
		}
		tr := &quic.Transport{Conn: udp}
		cancelled, stop := context.WithCancel(context.Background())
		stop()
		_, _, _ = tr.ReadNonQUICPacket(cancelled, make([]byte, 1))
		sock := &socket{udp: udp, tr: tr, family: network}
		e.sockets = append(e.sockets, sock)
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.readPackets(readerCtx, sock) }()
		return nil
	}
	ip4 := net.IPv4zero
	if s.AllowLoopback {
		ip4 = net.IPv4(127, 0, 0, 1)
	}
	if err = open("udp4", ip4); err != nil {
		return nil, err
	}
	if hasV6 {
		ip6 := net.IPv6zero
		if s.AllowLoopback {
			ip6 = net.IPv6loopback
		}
		_ = open("udp6", ip6) // IPv4 remains usable if IPv6 binding is unavailable.
	}
	reflexive := e.gatherSTUN(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	port4, port6 := uint16(e.sockets[0].udp.LocalAddr().(*net.UDPAddr).Port), uint16(0)
	for _, sock := range e.sockets {
		if sock.family == "udp6" {
			port6 = uint16(sock.udp.LocalAddr().(*net.UDPAddr).Port)
		}
	}
	defaultPairs := make([]netip.AddrPort, 0, len(defaults))
	for _, ip := range defaults {
		port := port4
		if ip.Is6() {
			port = port6
		}
		defaultPairs = append(defaultPairs, netip.AddrPortFrom(ip, port))
	}
	if port6 == 0 {
		kept := hosts[:0]
		for _, h := range hosts {
			if h.addr.Is4() {
				kept = append(kept, h)
			}
		}
		hosts = kept
	}
	e.info.Candidates = orderCandidatePairs(defaultPairs, reflexive, hosts, port4, port6, s.AllowLoopback)
	if len(e.info.Candidates) == 0 {
		return e, nil
	}
	e.cert, e.info.CertSHA256, err = newCertificate()
	if err != nil {
		return nil, err
	}
	return e, nil
}

func defaultRouteIP(network, target string) (netip.Addr, bool) {
	addr, err := net.ResolveUDPAddr(network, target)
	if err != nil {
		return netip.Addr{}, false
	}
	conn, err := net.DialUDP(network, nil, addr)
	if err != nil {
		return netip.Addr{}, false
	}
	defer conn.Close()
	ip, ok := netip.AddrFromSlice(conn.LocalAddr().(*net.UDPAddr).IP)
	return ip.Unmap(), ok && usableAddr(ip, false)
}

func (e *Endpoint) readPackets(ctx context.Context, sock *socket) {
	buf := make([]byte, 1500)
	for {
		n, from, err := sock.tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		pkt := packet{data: append([]byte(nil), buf[:n]...), from: from, sock: sock}
		select {
		case e.packets <- pkt:
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (e *Endpoint) gatherSTUN(ctx context.Context) []netip.AddrPort {
	if len(e.settings.STUNServers) == 0 {
		return nil
	}
	gatherCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	type target struct {
		sock *socket
		addr *net.UDPAddr
	}
	targets := make(chan target, len(e.settings.STUNServers)*len(e.sockets))
	for _, sock := range e.sockets {
		for _, server := range e.settings.STUNServers {
			go func(sock *socket, server string) {
				host, portText, err := net.SplitHostPort(server)
				if err != nil {
					return
				}
				port, err := strconv.Atoi(portText)
				if err != nil || port < 1 || port > 65535 {
					return
				}
				family := "ip4"
				if sock.family == "udp6" {
					family = "ip6"
				}
				ips, err := net.DefaultResolver.LookupNetIP(gatherCtx, family, host)
				if err != nil {
					return
				}
				for _, ip := range ips {
					ip = ip.Unmap()
					if (sock.family == "udp4") != ip.Is4() {
						continue
					}
					addr := &net.UDPAddr{IP: net.IP(ip.AsSlice()), Port: port}
					select {
					case targets <- target{sock, addr}:
					case <-gatherCtx.Done():
					}
					return
				}
			}(sock, server)
		}
	}
	pending := map[[12]byte]netip.AddrPort{}
	var out []netip.AddrPort
	seen := map[netip.AddrPort]bool{}
	v4, v6 := 0, 0
	for {
		select {
		case target := <-targets:
			request, tid, err := bindingRequest()
			if err != nil {
				continue
			}
			ip, ok := netip.AddrFromSlice(target.addr.IP)
			if !ok {
				continue
			}
			pending[tid] = netip.AddrPortFrom(ip.Unmap(), uint16(target.addr.Port))
			if _, err := target.sock.tr.WriteTo(request, target.addr); err != nil {
				delete(pending, tid)
			}
		case pkt := <-e.packets:
			if ap, ok := acceptBinding(pkt.data, pkt.from, pending); ok && usableAddr(ap.Addr(), false) && !seen[ap] {
				if ap.Addr().Is4() {
					if v4 >= 2 {
						continue
					}
					v4++
				} else {
					if v6 >= 1 {
						continue
					}
					v6++
				}
				out = append(out, ap)
				seen[ap] = true
			}
		case <-gatherCtx.Done():
			return out
		}
	}
}

func (e *Endpoint) socketFor(ap netip.AddrPort) *socket {
	family := "udp4"
	if ap.Addr().Is6() {
		family = "udp6"
	}
	for _, sock := range e.sockets {
		if sock.family == family {
			return sock
		}
	}
	return nil
}

func (e *Endpoint) CloseListeners() {
	e.CloseListenersExcept(nil)
}

// CloseListenersExcept closes every authenticated connection except the one
// whose attach selected the operation.
func (e *Endpoint) CloseListenersExcept(selected *quic.Conn) {
	e.mu.Lock()
	e.closedToNew = true
	listeners := e.listeners
	e.listeners = nil
	cancel := e.listenerCancel
	e.listenerCancel = nil
	var losers []*quic.Conn
	for conn := range e.accepted {
		if conn != selected {
			losers = append(losers, conn)
		}
	}
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
	for _, conn := range losers {
		conn.CloseWithError(1, "another connection selected")
	}
}

func (e *Endpoint) Close() {
	e.closeOnce.Do(func() {
		e.cancel()
		e.CloseListeners()
		for _, sock := range e.sockets {
			_ = sock.tr.Close()
			_ = sock.udp.Close()
		}
		e.wg.Wait()
	})
}

type AcceptResult struct {
	Conn *quic.Conn
	Err  error
}

// Listen starts authenticated QUIC listeners and bounded punching on the same sockets.
func (e *Endpoint) Listen(ctx context.Context, offer *protocol.DirectInfo) (<-chan AcceptResult, error) {
	if offer == nil {
		return nil, errors.New("direct: missing offer")
	}
	peers := validateCandidates(offer.Candidates, e.settings.AllowLoopback)
	out := make(chan AcceptResult, 8)
	if len(peers) == 0 {
		return out, nil
	}
	listenCtx, cancelListen := context.WithCancel(ctx)
	e.mu.Lock()
	e.listenerCancel = cancelListen
	e.mu.Unlock()
	allowed := map[netip.Addr]bool{}
	for _, ap := range peers {
		allowed[ap.Addr()] = true
	}
	for _, sock := range e.sockets {
		sock.tr.VerifySourceAddress = func(addr net.Addr) bool {
			udp, ok := addr.(*net.UDPAddr)
			if !ok {
				return true
			}
			ip, ok := netip.AddrFromSlice(udp.IP)
			return !ok || !allowed[ip.Unmap()]
		}
		ln, err := sock.tr.Listen(verifyTLSConfig(e.cert, offer.CertSHA256, true), quicConfig())
		if err != nil {
			e.CloseListeners()
			return nil, err
		}
		e.mu.Lock()
		e.listeners = append(e.listeners, ln)
		e.mu.Unlock()
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			for {
				conn, err := ln.Accept(listenCtx)
				if err != nil {
					return
				}
				e.mu.Lock()
				closed := e.closedToNew
				full := len(e.accepted) >= 8
				if !closed && !full {
					e.accepted[conn] = struct{}{}
				}
				e.mu.Unlock()
				if closed {
					conn.CloseWithError(1, "operation selected")
					return
				}
				if full {
					conn.CloseWithError(1, "candidate connection limit")
					continue
				}
				select {
				case out <- AcceptResult{Conn: conn}:
				case <-listenCtx.Done():
					conn.CloseWithError(0, "")
					return
				}
			}
		}()
	}
	probeCtx, stopProbe := context.WithTimeout(listenCtx, 5*time.Second)
	e.wg.Add(1)
	go func() { defer e.wg.Done(); defer stopProbe(); e.probe(probeCtx, peers) }()
	return out, nil
}

func (e *Endpoint) probe(ctx context.Context, peers []netip.AddrPort) {
	for round := 0; ; round++ {
		var probe [64]byte
		if _, err := rand.Read(probe[:]); err != nil {
			return
		}
		probe[0] &= 0x3f
		for _, ap := range peers {
			sock := e.socketFor(ap)
			if sock == nil {
				continue
			}
			addr := net.UDPAddrFromAddrPort(ap)
			_, _ = sock.tr.WriteTo(probe[:], addr)
		}
		limit := 10 * (round + 1) * (round + 1)
		if limit > 200 {
			limit = 200
		}
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return
		}
		pause := time.Duration(10+binary.LittleEndian.Uint64(random[:])%uint64(limit)) * time.Millisecond
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Dial returns only the winning authenticated connection after all losing dials exit.
func (e *Endpoint) Dial(ctx context.Context, offer *protocol.DirectInfo) (*quic.Conn, error) {
	if offer == nil {
		return nil, errors.New("direct: missing offer")
	}
	peers := validateCandidates(offer.Candidates, e.settings.AllowLoopback)
	if len(peers) == 0 {
		return nil, errors.New("direct: no valid peer candidates")
	}
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.probe(dialCtx, peers) }()
	type outcome struct {
		conn *quic.Conn
		err  error
	}
	out := make(chan outcome, len(peers))
	var wg sync.WaitGroup
	started := 0
	for _, ap := range peers {
		sock := e.socketFor(ap)
		if sock == nil {
			continue
		}
		started++
		wg.Add(1)
		go func(sock *socket, ap netip.AddrPort) {
			defer wg.Done()
			conn, err := sock.tr.Dial(dialCtx, net.UDPAddrFromAddrPort(ap), verifyTLSConfig(e.cert, offer.CertSHA256, false), quicConfig())
			out <- outcome{conn, err}
		}(sock, ap)
	}
	if started == 0 {
		return nil, errors.New("direct: no matching UDP transport")
	}
	var winner *quic.Conn
	var last error
	for i := 0; i < started; i++ {
		select {
		case result := <-out:
			if result.conn != nil && winner == nil {
				winner = result.conn
				cancel()
			} else if result.conn != nil {
				result.conn.CloseWithError(0, "")
			}
			if result.err != nil {
				last = result.err
			}
		case <-ctx.Done():
			cancel()
			i = started
		}
		if winner != nil {
			break
		}
	}
	cancel()
	wg.Wait()
	close(out)
	for result := range out {
		if result.conn != nil && result.conn != winner {
			result.conn.CloseWithError(0, "")
		}
	}
	if winner != nil {
		return winner, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("direct: all dials failed: %w", last)
}
