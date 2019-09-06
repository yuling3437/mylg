// Copyright 2016 Mehrdad Arshad Rad <arshad.rad@gmail.com>. All rights reserved.
// Use of this source code is governed by a MIT license that can
// be found in the LICENSE file.

package icmp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/mehrdadrad/mylg/cli"
	"github.com/mehrdadrad/mylg/ripe"
)

// TCPOption represents TCP option
type TCPOption struct {
	kind   uint8
	length uint8
	data   []byte
}

// MHopResp represents multi hop's responses
type MHopResp []HopResp

// NewTrace creates new trace object
func NewTrace(args string, cfg cli.Config) (*Trace, error) {
	var (
		family    int
		proto     int
		ip, lAddr net.IP
	)
	target, flag := cli.Flag(args)
	forceIPv4 := cli.SetFlag(flag, "4", false).(bool)
	forceIPv6 := cli.SetFlag(flag, "6", false).(bool)
	// show help
	if _, ok := flag["help"]; ok || len(target) < 3 {
		helpTrace()
		return nil, nil
	}
	ips, err := net.LookupIP(target)
	if err != nil {
		return nil, err
	}
	for _, IP := range ips {
		if IsIPv4(IP) && !forceIPv6 {
			ip = IP
			break
		} else if IsIPv6(IP) && !forceIPv4 {
			ip = IP
			break
		}
	}

	if ip == nil {
		return nil, fmt.Errorf("there is not A or AAAA record")
	}

	if lAddr, err = getLocalAddr(ip.String()); err != nil {
		return nil, err
	}

	if IsIPv4(ip) {
		family = syscall.AF_INET
		proto = syscall.IPPROTO_ICMP
	} else {
		family = syscall.AF_INET6
		proto = syscall.IPPROTO_ICMPV6
	}

	UDP := cli.SetFlag(flag, "u", false).(bool)
	TCP := cli.SetFlag(flag, "t", false).(bool)

	t := &Trace{
		host:     target,
		ips:      ips,
		ip:       ip,
		src:      lAddr,
		seq:      1,
		family:   family,
		proto:    proto,
		icmp:     !UDP && !TCP,
		udp:      UDP && !TCP,
		tcp:      TCP && !UDP,
		pSize:    cli.SetFlag(flag, "p", 52).(int),
		uiTheme:  cli.SetFlag(flag, "T", cfg.Trace.Theme).(string),
		wait:     cli.SetFlag(flag, "w", cfg.Trace.Wait).(string),
		resolve:  cli.SetFlag(flag, "n", true).(bool),
		ripe:     cli.SetFlag(flag, "nr", true).(bool),
		realTime: cli.SetFlag(flag, "r", false).(bool),
		maxTTL:   cli.SetFlag(flag, "m", 30).(int),
		count:    cli.SetFlag(flag, "c", -1).(int),
		report:   cli.SetFlag(flag, "R", false).(bool),
		km:       cli.SetFlag(flag, "km", false).(bool),
	}

	// default report's count
	t.setReportDCount(10)

	return t, nil
}

func (h MHopResp) Len() int           { return len(h) }
func (h MHopResp) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h MHopResp) Less(i, j int) bool { return len(h[i].ip) > len(h[j].ip) }

// SetTTL set the IP packat time to live
func (i *Trace) SetTTL(ttl int) {
	i.ttl = ttl
}

// Send tries to send ICMP/UDP IPv4/IPv6 packet
func (i *Trace) Send(port int) (int, int, error) {
	rand.Seed(time.Now().UTC().UnixNano())
	var (
		seq    = i.seq
		id     = 38840 + seq
		sotype int
		proto  int
		err    error
	)

	i.seq++

	if i.icmp && i.ip.To4() != nil {
		sotype = syscall.SOCK_RAW
		proto = syscall.IPPROTO_ICMP
	} else if i.icmp && i.ip.To16() != nil {
		sotype = syscall.SOCK_RAW
		proto = syscall.IPPROTO_ICMPV6
	} else {
		sotype = syscall.SOCK_DGRAM
		proto = syscall.IPPROTO_UDP
	}

	fd, err := syscall.Socket(i.family, sotype, proto)
	if err != nil {
		return id, seq, err
	}
	defer syscall.Close(fd)

	// Set options
	if IsIPv4(i.ip) {
		err = i.Sendv4(id, seq, port)
		return id, seq, err
	} else {
		var b [16]byte
		copy(b[:], i.ip.To16())
		addr := syscall.SockaddrInet6{
			Port:   port,
			ZoneId: 0,
			Addr:   b,
		}

		m, err := icmpV6Message(os.Getpid()&0xffff, seq, i.pSize)
		if err != nil {
			return id, seq, err
		}

		setIPv6HopLimit(fd, i.ttl)

		if err := syscall.Sendto(fd, m, 0, &addr); err != nil {
			return id, seq, err
		}
	}
	return id, seq, nil
}

// Send tries to send ICMP/UDP IPv4 packet
func (i *Trace) Sendv4(id, seq, rport int) error {
	var (
		b     []byte
		proto int
		lport int
	)

	switch {
	case i.icmp:
		proto = 1 // icmp v4
		b, _ = icmpV4Message(os.Getpid()&0xffff, seq, i.pSize)
	case i.udp:
		proto = 17 // udp
		lport = 64000 + rand.Intn(3)*100
		b = udpMessage(lport, rport, i.pSize, true)
	case i.tcp:
		proto = 6 // tcp
		b = tcpMessage(0, 33434, 64, true)
		setTCPCheckSum(i.src, i.ip, b)
	}

	c, err := net.ListenPacket(fmt.Sprintf("ip4:%d", proto), "0.0.0.0")
	if err != nil {
		return err
	}
	defer c.Close()

	p, err := ipv4.NewRawConn(c)
	if err != nil {
		return err
	}

	h := &ipv4.Header{
		Version:  ipv4.Version,
		Len:      ipv4.HeaderLen,
		TotalLen: ipv4.HeaderLen + len(b),
		Protocol: proto,
		ID:       id,
		TTL:      i.ttl,
		Dst:      i.ip.To4(),
	}

	if err := p.WriteTo(h, b, nil); err != nil {
		return err
	}
	return nil
}

// SetReadDeadLine sets rx timeout
func (i *Trace) SetReadDeadLine() error {
	timeout, err := time.ParseDuration(i.wait)
	if err != nil {
		return err
	}
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	return syscall.SetsockoptTimeval(i.fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
}

// SetWriteDeadLine sets tx timeout
func (i *Trace) SetWriteDeadLine() error {
	tv := syscall.NsecToTimeval(1e6 * DefaultTXTimeout)
	return syscall.SetsockoptTimeval(i.fd, syscall.SOL_SOCKET, syscall.SO_SNDTIMEO, &tv)
}

// SetDeadLine sets tx/rx timeout
func (i *Trace) SetDeadLine() error {
	err := i.SetReadDeadLine()
	if err != nil {
		return err
	}
	err = i.SetWriteDeadLine()
	if err != nil {
		return err
	}
	return nil
}

// Bind starts to listen for ICMP reply
func (i *Trace) Bind() error {
	var err error

	i.fd, err = syscall.Socket(i.family, syscall.SOCK_RAW, i.proto)
	if err != nil {
		return os.NewSyscallError("bind.socket", err)
	}

	err = i.SetDeadLine()
	if err != nil {
		println(err.Error())
	}

	if i.family == syscall.AF_INET {
		addr := syscall.SockaddrInet4{
			Port: 0,
			Addr: [4]byte{},
		}

		if err := syscall.Bind(i.fd, &addr); err != nil {
			return os.NewSyscallError("bindv4", err)
		}
	} else {
		addr := syscall.SockaddrInet6{
			Port:   0,
			ZoneId: 0,
			Addr:   [16]byte{},
		}

		if err := syscall.Bind(i.fd, &addr); err != nil {
			return os.NewSyscallError("bindv6", err)
		}

	}
	return nil
}

// Recv gets the replied icmp packet
func (i *Trace) Recv(id, seq int) (ICMPResp, error) {
	var (
		b    = make([]byte, 512)
		ts   = time.Now()
		resp ICMPResp
		wID  bool
		wSeq bool
		wDst bool
		wV4  bool
	)

	for {
		n, from, err := syscall.Recvfrom(i.fd, b, 0)

		if err != nil {
			du, _ := time.ParseDuration(i.wait)
			if err == syscall.EAGAIN && time.Since(ts) < du {
				continue
			}
			return resp, err
		}

		b = b[:n]

		if len(i.ip.To4()) == net.IPv4len {
			resp = icmpV4RespParser(b)
			wID = resp.typ == IPv4ICMPTypeEchoReply && id != resp.id
			wSeq = seq != resp.seq
			wDst = resp.ip.dst.String() != i.ip.String()
			wV4 = (id != resp.ip.id && resp.ip.id != 0)
		} else {
			resp = icmpV6RespParser(b)
			resp.src = net.IP(from.(*syscall.SockaddrInet6).Addr[:])
			wID = resp.typ == IPv6ICMPTypeEchoReply && id != resp.id
			wSeq = seq != resp.seq
			wDst = resp.ip.dst.String() != i.ip.String()
			wV4 = false
		}

		if (i.icmp && wSeq) || (!i.icmp && (wDst || wID || wV4)) {
			du, _ := time.ParseDuration(i.wait)
			if time.Since(ts) < du {
				continue
			}
			return resp, fmt.Errorf("wrong response")
		}

		break
	}
	return resp, nil
}

// NextHop pings the specific hop by set TTL
func (i *Trace) NextHop(hop int) HopResp {
	rand.Seed(time.Now().UTC().UnixNano())
	var (
		r        = HopResp{num: hop}
		dnsCache = make(map[string][]string)
		port     = 33434
		name     []string
		ok       bool
	)
	i.SetTTL(hop)
	begin := time.Now()

	id, seq, err := i.Send(port)
	if err != nil {
		return HopResp{num: hop, err: err}
	}

	resp, err := i.Recv(id, seq)
	if err != nil {
		r = HopResp{hop, "", "", 0, false, nil, Whois{}}
		return r
	}

	elapsed := time.Since(begin)

	if i.resolve {
		if name, ok = dnsCache[resp.src.String()]; !ok {
			name, _ = lookupAddr(resp.src)
			dnsCache[resp.src.String()] = name
		}
	}
	if len(name) > 0 {
		r = HopResp{hop, name[0], resp.src.String(), elapsed.Seconds() * 1e3, false, nil, Whois{}}
	} else {
		r = HopResp{hop, "", resp.src.String(), elapsed.Seconds() * 1e3, false, nil, Whois{}}
	}
	// reached to the target
	for _, h := range i.ips {
		if resp.src.String() == h.String() {
			r.last = true
			break
		}
	}
	return r
}

// Run provides trace based on the other methods
func (i *Trace) Run(retry int) (chan []HopResp, error) {
	var (
		c = make(chan []HopResp, 1)
		r []HopResp
	)

	if err := i.Bind(); err != nil {
		return c, err
	}

	go func() {
		defer func() {
			if err := recover(); err != nil {
				syscall.Close(i.fd)
			}
		}()
	LOOP:
		for h := 1; h <= i.maxTTL; h++ {
			for n := 0; n < retry; n++ {
				hop := i.NextHop(h)
				r = append(r, hop)
				if hop.err != nil {
					break
				}
			}
			if i.ripe {
				i.addWhois(r[:])
			}
			c <- r
			for _, R := range r {
				if R.last || R.err != nil {
					break LOOP
				}
			}
			r = r[:0]
		}
		close(c)
		syscall.Close(i.fd)
	}()
	return c, nil
}

// MRun provides trace all hops in loop
func (i *Trace) MRun() (chan HopResp, error) {
	var (
		c      = make(chan HopResp, 1)
		ASN    = make(map[string]Whois, 100)
		maxTTL = i.maxTTL
		MU     sync.Mutex
		count  int
	)

	if err := i.Bind(); err != nil {
		return c, err
	}

	go func() {
		defer func() {
			if err := recover(); err != nil {
				syscall.Close(i.fd)
			}
		}()
	LOOP:
		for {
			for h := 1; h <= maxTTL; h++ {
				hop := i.NextHop(h)
				if w, ok := ASN[hop.ip]; ok {
					hop.whois = w
				} else if hop.ip != "" {
					go func(ASN map[string]Whois) {
						MU.Lock()
						defer MU.Unlock()
						w, _ := whois(hop.ip)
						ASN[hop.ip] = w
					}(ASN)
				}

				c <- hop

				if hop.last && maxTTL == i.maxTTL {
					maxTTL = h
				}
				time.Sleep(100 * time.Millisecond)
			}
			count++
			if i.count > 0 && count >= i.count {
				break LOOP
			}
			time.Sleep(1 * time.Second)
		}
		close(c)
	}()
	return c, nil
}

// Marshal encodes hop response
func (h *HopResp) Marshal() string {
	return fmt.Sprintf(`{ "Id": %d, "Hop": "%s", "IP" : "%s", "Elapsed": %.3f, "Holder": "%s", "ASN": %.0f, "Last": %t }`,
		h.num,
		h.hop,
		h.ip,
		h.elapsed,
		h.whois.holder,
		h.whois.asn,
		h.last,
	)
}

// routerChange detects if the router changed
// to another one
func routerChange(router, b string) bool {
	if b != "" {
		bRouter := strings.Fields(b)
		if len(bRouter) < 2 {
			return false
		}
		hop := strings.Split(b, "] ")
		if len(hop) < 2 {
			return false
		}
		if strings.Fields(hop[1])[0] != router {
			return true
		}
	}
	return false
}

// Print prints out trace result in normal or terminal mode
func (i *Trace) Print() {
// 	if i.realTime {
// 		if rep, err := i.TermUI(); err != nil {
// 			fmt.Println(err.Error())
// 		} else if rep != "" {
// 			fmt.Println(rep)
// 		}
// 	} else {
// 		i.PrintPretty()
// 	}
}

// PrintPretty prints out trace result
func (i *Trace) PrintPretty() {
	var (
		counter int
		sigCh   = make(chan os.Signal, 1)
		resp    = make(chan []HopResp, 1)
		err     error
	)

	if resp, err = i.Run(3); err != nil {
		println(err.Error())
		return
	}

	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	// header
	fmt.Printf("trace route to %s (%s), %d hops max\n", i.host, i.ip, i.maxTTL)
LOOP:
	for {
		select {
		case r, ok := <-resp:
			if !ok {
				break LOOP
			}
			for _, R := range r {
				if R.err != nil {
					println(R.err.Error())
					break LOOP
				}
			}
			counter++
			sort.Sort(MHopResp(r))
			// there is not any load balancing and there is at least a timeout
			if r[0].ip != r[1].ip && (r[1].elapsed == 0 || r[2].elapsed == 0) {
				fmt.Printf("%-2d %s", counter, fmtHops(r, 1))
				continue
			}
			// there is not any load balancing and there is at least a timeout
			if r[1].ip != r[2].ip && (r[0].elapsed == 0 || r[1].elapsed == 0) {
				fmt.Printf("%-2d %s", counter, fmtHops(r, 1))
				continue
			}
			// there is not any load balancing and there is at least a timeout
			if r[0].ip == r[1].ip && r[0].elapsed != 0 && r[2].elapsed == 0 {
				fmt.Printf("%-2d %s %s", counter, fmtHops(r[0:2], 0), fmtHops(r[2:3], 1))
				continue
			}

			// load balance between three routes
			if r[0].ip != r[1].ip && r[1].ip != r[2].ip {
				fmt.Printf("%-2d %s   %s   %s", counter, fmtHops(r[0:1], 1), fmtHops(r[1:2], 1), fmtHops(r[2:3], 1))
				continue
			}
			// load balance between two routes
			if r[0].ip == r[1].ip && r[1].ip != r[2].ip {
				fmt.Printf("%-2d %s   %s", counter, fmtHops(r[0:2], 1), fmtHops(r[2:3], 1))
				continue
			}
			// load balance between two routes
			if r[0].ip != r[1].ip && r[1].ip == r[2].ip {
				fmt.Printf("%-2d %s   %s", counter, fmtHops(r[0:1], 1), fmtHops(r[1:3], 1))
				continue
			}
			// there is not any load balancing
			if r[0].ip == r[1].ip && r[1].ip == r[2].ip {
				fmt.Printf("%-2d %s", counter, fmtHops(r, 1))
			}
			//fmt.Printf("%#v\n", r)
		case <-sigCh:
			close(resp)
			break LOOP
		}
	}
}

func fmtHops(m []HopResp, newLine int) string {
	var (
		timeout = false
		msg     string
	)
	for _, r := range m {
		if (msg == "" || timeout) && r.hop != "" {
			if r.whois.asn != 0 {
				msg += fmt.Sprintf("%s (%s) [ASN %.0f/%s] ", r.hop, r.ip, r.whois.asn, strings.Fields(r.whois.holder)[0])
			} else {
				msg += fmt.Sprintf("%s (%s) ", r.hop, r.ip)
			}
		}
		if (msg == "" || timeout) && r.hop == "" && r.elapsed != 0 {
			if r.whois.asn != 0 {
				msg += fmt.Sprintf("%s [ASN %.0f/%s] ", r.ip, r.whois.asn, strings.Fields(r.whois.holder)[0])
			} else {
				msg += fmt.Sprintf("%s ", r.ip)
			}
		}
		if r.elapsed != 0 {
			msg += fmt.Sprintf("%.3f ms ", r.elapsed)
			timeout = false
		} else {
			msg += "* "
			timeout = true
		}
	}
	if newLine == 1 {
		msg += "\n"
	}
	return msg
}

// addWhois adds whois info to response if available
func (i *Trace) addWhois(R []HopResp) {
	var (
		ips = make(map[string]Whois, 3)
		w   Whois
		err error
	)

	for _, r := range R {
		ips[r.ip] = Whois{}
	}

	for ip := range ips {
		if ip == "" {
			continue
		}

		w, err = whois(ip)

		if err != nil {
			continue
		}

		ips[ip] = w
	}

	for i := range R {
		R[i].whois = ips[R[i].ip]
	}
}

// setReportDCount set the report default count
func (i *Trace) setReportDCount(c int) {
	if i.report && i.count < 0 {
		i.count = c
	}
}

// whois returns prefix whois info from RIPE
func whois(ip string) (Whois, error) {
	var resp Whois

	_, net, err := net.ParseCIDR(ip + "/24")
	if err != nil {
		ip = net.String()
	}

	r := new(ripe.Prefix)
	r.Set(ip)
	r.GetData()
	data, ok := r.Data["data"].(map[string]interface{})
	if !ok {
		return Whois{}, fmt.Errorf("data not available")
	}
	asns := data["asns"].([]interface{})
	for _, h := range asns {
		resp.holder = h.(map[string]interface{})["holder"].(string)
		resp.asn = h.(map[string]interface{})["asn"].(float64)
	}
	return resp, nil
}

func min(a, b float64) float64 {
	if a == 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}
func avg(a, b float64) float64 {
	if a != 0 {
		return (a + b) / 2
	}
	return b
}
func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func lookupAddr(ip net.IP) ([]string, error) {
	var (
		c = make(chan []string, 1)
		r []string
	)

	go func() {
		n, _ := net.LookupAddr(ip.String())
		c <- n
	}()
	select {
	case r = <-c:
		return r, nil
	case <-time.After(1 * time.Second):
		return r, fmt.Errorf("lookup.addr timeout")
	}
}

func calcStatistics(s *Stats, elapsed float64) {
	s.min = min(s.min, elapsed)
	s.avg = avg(s.avg, elapsed)
	s.max = max(s.max, elapsed)
}

func udpMessage(lport, rport, pSize int, isIPv4 bool) []byte {
	var (
		buf = new(bytes.Buffer)
	)

	if isIPv4 {
		pSize = pSize - (20 + 8)
	} else {
		pSize = pSize - (40 + 8)
	}

	binary.Write(buf, binary.BigEndian, uint16(lport))
	binary.Write(buf, binary.BigEndian, uint16(rport))
	binary.Write(buf, binary.BigEndian, uint16(8+pSize))
	binary.Write(buf, binary.BigEndian, uint16(0))

	if pSize > 0 {
		buf.Write(make([]byte, pSize))
	}

	return buf.Bytes()
}

func tcpMessage(lport, rport, pSize uint16, isIPv4 bool) []byte {
	var (
		buf        = new(bytes.Buffer)
		tcpOptions []TCPOption
		offset     = 8 + 3
	)

	binary.Write(buf, binary.BigEndian, lport)
	binary.Write(buf, binary.BigEndian, rport)

	binary.Write(buf, binary.BigEndian, rand.Uint32())
	binary.Write(buf, binary.BigEndian, uint32(0))

	mix := uint16(offset)<<12 |
		uint16(0)<<9 | // 3 bits reserved
		uint16(0)<<6 | // 3 bits ECN
		uint16(2) //  6 bits control bits (000010, SYN bit set)
	binary.Write(buf, binary.BigEndian, mix)

	binary.Write(buf, binary.BigEndian, uint16(0xFFFF))
	binary.Write(buf, binary.BigEndian, uint16(0x0000))
	binary.Write(buf, binary.BigEndian, uint16(0))

	// set MSS
	tcpOptions = append(tcpOptions, TCPOption{
		kind:   2,
		length: 4,
		data:   []byte{0x05, 0xb4},
	})
	// window scale
	tcpOptions = append(tcpOptions, TCPOption{
		kind:   3,
		length: 3,
		data:   []byte{0x05},
	})
	// Selective Acknowledgement permitted
	tcpOptions = append(tcpOptions, TCPOption{
		kind:   4,
		length: 2,
		data:   []byte{0x1},
	})
	// Timestamp
	tb := new(bytes.Buffer)
	now := time.Now().Unix()
	binary.Write(tb, binary.BigEndian, uint64(now))

	tcpOptions = append(tcpOptions, TCPOption{
		kind:   8,
		length: 10,
		data:   tb.Bytes(),
	})
	// End of option list
	tcpOptions = append(tcpOptions, TCPOption{
		kind:   0,
		length: 0,
		data:   []byte{},
	})

	for _, option := range tcpOptions {
		binary.Write(buf, binary.BigEndian, option.kind)
		if option.length > 1 {
			binary.Write(buf, binary.BigEndian, option.length)
			binary.Write(buf, binary.BigEndian, option.data)
		}
	}

	tcp := buf.Bytes()
	pad := int(offset)*4 - len(tcp)

	for i := 0; i < pad; i++ {
		tcp = append(tcp, 0)
	}

	return tcp
}

func setTCPCheckSum(src, dst net.IP, tcpHeader []byte) {
	var psdHeader = make([]byte, 0, 12)
	var header = make([]byte, 0, 12+len(tcpHeader))
	var pbuf = bytes.NewBuffer(psdHeader)
	pbuf.Write([]byte(src))
	pbuf.Write([]byte(dst))
	var (
		pd      uint8  = 0
		prot    uint8  = 6
		tcpSize uint16 = uint16(len(tcpHeader))
	)
	binary.Write(pbuf, binary.BigEndian, pd)
	binary.Write(pbuf, binary.BigEndian, prot)
	binary.Write(pbuf, binary.BigEndian, tcpSize)

	header = append(header, pbuf.Bytes()...)
	header = append(header, tcpHeader...)

	var (
		checksum uint16 = checkSum(header)
		high     uint8  = uint8(checksum >> 8)
		low      uint8  = uint8(checksum)
	)
	tcpHeader[17] = low
	tcpHeader[16] = high
}

func checkSum(data []byte) uint16 {
	var sum uint32 = 0
	var length = len(data) & (^1)
	for i := 0; i < length; i += 2 {
		sum += (uint32(data[i]) << 8) + uint32(data[i+1])
	}
	if len(data)&1 != 0 {
		sum += uint32(data[length])
	}
	for (sum >> 16) > 0 {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return uint16(^sum)
}

func helpTrace() {
	fmt.Println(`
    usage:
          trace IP address / domain name [options]
    options:
          -r             Real-time response time at each point along the way
          -n             Do not try to map IP addresses to host names
          -nr            Do not try to map IP addresses to ASN,Holder (RIPE NCC)
          -m MAX_TTL     Set the maximum number of hops
          -4             Forces the trace command to use IPv4 (target should be hostname)
          -6             Forces the trace command to use IPv6 (target should be hostname)
          -T             Set the real-time terminal theme (dark|light)
          -c             Set the number of pings sent
          -p             Set the packet size in bytes inclusive headers (default 52 bytes)
          -u             Use UDP datagram instead of ICMP
          -R             Prints results of real-time trace, when completed
    Example:
          trace 8.8.8.8
          trace freebsd.org -r
	`)

}
