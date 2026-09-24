package direct

import (
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/netip"
)

const stunCookie uint32 = 0x2112a442

func bindingRequest() ([]byte, [12]byte, error) {
	var tid [12]byte
	if _, err := rand.Read(tid[:]); err != nil {
		return nil, tid, err
	}
	b := make([]byte, 20)
	binary.BigEndian.PutUint16(b[:2], 1)
	binary.BigEndian.PutUint32(b[4:8], stunCookie)
	copy(b[8:], tid[:])
	return b, tid, nil
}

func parseBindingResponse(b []byte) (netip.AddrPort, [12]byte, bool) {
	var tid [12]byte
	if len(b) < 20 || binary.BigEndian.Uint16(b[:2]) != 0x0101 || binary.BigEndian.Uint32(b[4:8]) != stunCookie || int(binary.BigEndian.Uint16(b[2:4])) != len(b)-20 {
		return netip.AddrPort{}, tid, false
	}
	copy(tid[:], b[8:20])
	for p := b[20:]; len(p) >= 4; {
		kind, n := binary.BigEndian.Uint16(p[:2]), int(binary.BigEndian.Uint16(p[2:4]))
		padded := (n + 3) &^ 3
		if padded+4 > len(p) {
			return netip.AddrPort{}, tid, false
		}
		v := p[4 : 4+n]
		if kind == 0x0020 && len(v) >= 8 {
			length := 4
			if v[1] == 2 {
				length = 16
			}
			if (v[1] == 1 || v[1] == 2) && len(v) == 4+length {
				key := make([]byte, 16)
				binary.BigEndian.PutUint32(key, stunCookie)
				copy(key[4:], tid[:])
				ip := make([]byte, length)
				for i := range ip {
					ip[i] = v[4+i] ^ key[i]
				}
				addr, ok := netip.AddrFromSlice(ip)
				if ok {
					port := binary.BigEndian.Uint16(v[2:4]) ^ uint16(stunCookie>>16)
					if port > 0 {
						return netip.AddrPortFrom(addr.Unmap(), port), tid, true
					}
				}
			}
		}
		p = p[4+padded:]
	}
	return netip.AddrPort{}, tid, false
}

func acceptBinding(b []byte, from net.Addr, pending map[[12]byte]netip.AddrPort) (netip.AddrPort, bool) {
	addr, tid, ok := parseBindingResponse(b)
	if !ok {
		return netip.AddrPort{}, false
	}
	want, exists := pending[tid]
	fromUDP, isUDP := from.(*net.UDPAddr)
	if !exists || !isUDP || fromUDP.Port < 1 || fromUDP.Port > 65535 {
		return netip.AddrPort{}, false
	}
	source, ok := netip.AddrFromSlice(fromUDP.IP)
	if !ok || netip.AddrPortFrom(source.Unmap(), uint16(fromUDP.Port)) != want {
		return netip.AddrPort{}, false
	}
	delete(pending, tid)
	return addr, true
}
