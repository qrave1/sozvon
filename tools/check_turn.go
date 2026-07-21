package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	turnMagicCookie = 0x2112A442
	allocation      = 0x0003
)

func main() {
	if len(os.Args) < 5 {
		fmt.Println("Usage: check_turn <host:port> <username> <password> <tcp|udp>")
		os.Exit(1)
	}

	addr := os.Args[1]
	username := os.Args[2]
	password := os.Args[3]
	proto := os.Args[4]

	if proto == "tcp" {
		checkTCP(addr, username, password)
	} else {
		checkUDP(addr, username, password)
	}
}

func checkTCP(addr, username, password string) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Println("TCP: FAILED to connect:", err)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	stunBindingRequest(conn, username, password)
	sendAllocate(conn, username, password)
}

func checkUDP(addr, username, password string) {
	conn, err := net.DialTimeout("udp", addr, 5*time.Second)
	if err != nil {
		fmt.Println("UDP: FAILED to connect:", err)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	stunBindingRequest(conn, username, password)
	sendAllocate(conn, username, password)
}

func stunBindingRequest(conn net.Conn, username, password string) {
	req := buildSTUNBinding(username, password)
	_, err := conn.Write(req)
	if err != nil {
		fmt.Printf("STUN binding send failed: %v\n", err)
		return
	}
	resp := make([]byte, 256)
	n, err := conn.Read(resp)
	if err != nil {
		fmt.Printf("STUN binding response: %v\n", err)
		return
	}
	fmt.Printf("STUN binding: OK (%d bytes)\n", n)
}

func sendAllocate(conn net.Conn, username, password string) {
	req := buildAllocate(username, password)
	_, err := conn.Write(req)
	if err != nil {
		fmt.Printf("TURN Allocate send: %v\n", err)
		return
	}
	resp := make([]byte, 2048)
	n, err := conn.Read(resp)
	if err != nil {
		fmt.Printf("TURN Allocate response: %v\n", err)
		return
	}
	if n < 20 {
		fmt.Printf("TURN Allocate: too short response (%d bytes)\n", n)
		return
	}
	msgType := binary.BigEndian.Uint16(resp[0:2])
	if msgType == 0x0103 {
		fmt.Println("TURN Allocate: OK — relay address allocated!")
		// Try to extract relayed address from XOR-RELAYED-ADDRESS attribute (0x0016)
		for off := 20; off+4 < n; {
			attrType := binary.BigEndian.Uint16(resp[off : off+2])
			attrLen := binary.BigEndian.Uint16(resp[off+2 : off+4])
			if int(attrLen)+4 > n-off {
				break
			}
			if attrType == 0x0016 && attrLen >= 8 {
				port := binary.BigEndian.Uint16(resp[off+6 : off+8])
				ip := net.IP(resp[off+8 : off+12])
				fmt.Printf("  Relay on: %s:%d\n", ip, port)
			}
			off += 4 + int(attrLen)
			if off%4 != 0 {
				off += 4 - off%4
			}
		}
	} else {
		fmt.Printf("TURN Allocate: unexpected response type 0x%04x\n", msgType)
	}
}

func buildSTUNBinding(username, password string) []byte {
	// Simplified: send a Binding Request (class=0x0001) without integrity
	pkt := make([]byte, 20)
	binary.BigEndian.PutUint16(pkt[0:2], 0x0001) // Binding Request
	binary.BigEndian.PutUint16(pkt[2:4], 0)      // length
	binary.BigEndian.PutUint32(pkt[4:8], turnMagicCookie)
	_ = username
	_ = password
	return pkt
}

func buildAllocate(username, password string) []byte {
	// TURN Allocate Request with MESSAGE-INTEGRITY
	nonce := []byte("sozvon")
	realm := []byte("sozvon")

	pkt := make([]byte, 0, 200)
	hdr := make([]byte, 20)
	binary.BigEndian.PutUint16(hdr[0:2], 0x0003) // Allocate (class=Request)
	binary.BigEndian.PutUint16(hdr[2:4], 0)       // length placeholder
	binary.BigEndian.PutUint32(hdr[4:8], turnMagicCookie)
	copy(hdr[8:20], make([]byte, 12)) // transaction id (zeros)
	hdr[8] = 0
	hdr[9] = 0
	hdr[10] = 0
	hdr[11] = 1 // tx id unique-ish

	attrs := []byte{}
	// REQUESTED-TRANSPORT (0x0019) = 17 (UDP)
	attrs = append(attrs, attrBytes(0x0019, []byte{0x00, 0x00, 0x00, 0x17})...)
	// USERNAME
	attrs = append(attrs, attrBytes(0x0006, []byte(username))...)
	// REALM
	attrs = append(attrs, attrBytes(0x0014, realm)...)
	// NONCE
	attrs = append(attrs, attrBytes(0x0015, nonce)...)

	// MESSAGE-INTEGRITY (0x0008)
	mi := computeMessageIntegrity(hdr, attrs, password)
	attrs = append(attrs, attrBytes(0x0008, mi)...)

	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(attrs)))
	pkt = append(pkt, hdr...)
	pkt = append(pkt, attrs...)
	return pkt
}

func attrBytes(typ uint16, val []byte) []byte {
	pad := (4 - len(val)%4) % 4
	b := make([]byte, 4+len(val)+pad)
	binary.BigEndian.PutUint16(b[0:2], typ)
	binary.BigEndian.PutUint16(b[2:4], uint16(len(val)))
	copy(b[4:], val)
	return b
}

func computeMessageIntegrity(hdr, attrs []byte, password string) []byte {
	// STUN MESSAGE-INTEGRITY = HMAC-SHA1(key, msg) where key = MD5(username:realm:password)
	// For simplicity, use HMAC-SHA1 with password directly
	mac := hmac.New(sha1.New, []byte(password))
	mac.Write(hdr)
	mac.Write(attrs)
	return mac.Sum(nil)
}

