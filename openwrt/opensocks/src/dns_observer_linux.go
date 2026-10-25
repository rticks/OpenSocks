//go:build linux

package main

import (
	"syscall"
	"time"
)

// observeDNSAnswers passively learns addresses for every subdomain of the
// configured service suffixes. It does not sit in the DNS path, so a failure
// here can never break client name resolution.
//
// The Linux implementation sniffs LAN DNS responses with an AF_PACKET socket and
// a kernel BPF filter; the parsing helpers it uses live in dns_observer.go and
// are portable so they can be unit-tested on any platform.
func observeDNSAnswers() {
	const (
		afPacket       = 17
		ethPIP         = 0x0800
		soBindToDevice = 25
	)
	device := detectLANDevice()
	trustedIP, trustedMAC := lanIdentity(device)
	if trustedIP == nil || len(trustedMAC) != 6 {
		logf("dns observer unavailable: LAN identity could not be verified")
		return
	}
	fd, err := syscall.Socket(afPacket, syscall.SOCK_RAW, int(htons(ethPIP)))
	if err != nil {
		logf("dns observer unavailable: %v", err)
		return
	}
	defer syscall.Close(fd)
	_ = syscall.SetsockoptString(fd, syscall.SOL_SOCKET, soBindToDevice, device)
	if err := attachDNSResponseFilter(fd); err != nil {
		logf("dns observer disabled because the kernel filter could not be attached: %v", err)
		return
	}

	type learning struct {
		group string
		ips   []string
	}
	learned := make(chan learning, 128)
	go func() {
		pending := map[string]map[string]bool{}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case item := <-learned:
				if pending[item.group] == nil {
					pending[item.group] = map[string]bool{}
				}
				for _, ip := range item.ips {
					pending[item.group][ip] = true
				}
			case <-ticker.C:
				flushLearnedDNS(pending)
				pending = map[string]map[string]bool{}
			}
		}
	}()
	buf := make([]byte, 8192)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil || n < 42 {
			continue
		}
		names, ips := dnsIPv4Answers(buf[:n], trustedIP, trustedMAC)
		if len(ips) > 0 {
			if chinaRouteForDNSNames(names) {
				select {
				case learned <- learning{group: "", ips: ips}:
				default:
				}
			}
			for _, group := range serviceGroupsForDNSNames(names) {
				select {
				case learned <- learning{group: group, ips: ips}:
				default:
				}
			}
		}
	}
}
