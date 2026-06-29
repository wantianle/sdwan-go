package core

import (
	"crypto/aes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
)

// Message types (from reverse engineering)
const (
	MsgOPENACK  byte = 0x12
	MsgOPEN     byte = 0x13
	MsgTUNSetup byte = 0x14
	MsgECHOREQ  byte = 0x15
	MsgECHORESP byte = 0x16
	MsgDATA     byte = 0x18
)

// PktSign generates the 16-byte signature for a packet.
// signature = MD5(header[0:8] + "mw")
func PktSign(header []byte) []byte {
	h := md5.New()
	h.Write(header[:8])
	h.Write([]byte("mw"))
	return h.Sum(nil)
}

// PktSignInPlace writes the 16-byte signature into header[8:24]
func PktSignInPlace(header []byte) {
	copy(header[8:24], PktSign(header))
}

// PktVerify checks if the signature in header[8:24] is valid
func PktVerify(header []byte) bool {
	expected := PktSign(header)
	for i := 0; i < 16; i++ {
		if header[8+i] != expected[i] {
			return false
		}
	}
	return true
}

// EncryptPassword computes the AES-128 encrypted password block.
// aes_key = MD5("mw" + username)
// result = AES-128-ECB encrypt(password block padded to 16 bytes)
func EncryptPassword(username, password string) []byte {
	// Derive AES key: MD5("mw" + username)
	h := md5.New()
	h.Write([]byte("mw"))
	h.Write([]byte(username))
	aesKey := h.Sum(nil) // 16 bytes

	// Pad password to 16 bytes (zero-padded, matching original behavior)
	block := make([]byte, 16)
	copy(block, password)

	// AES-128 encrypt (single block → ECB)
	cipher, err := aes.NewCipher(aesKey)
	if err != nil {
		panic("aes.NewCipher: " + err.Error())
	}
	cipher.Encrypt(block, block)
	return block
}

// BuildOpenPacket constructs the OPEN request (0x13)
func BuildOpenPacket(cfg *Config) []byte {
	// Max size estimation: header(24) + TLVs
	buf := make([]byte, 1024)
	pos := 0

	// ---- Header (24 bytes) ----
	buf[pos] = MsgOPEN    // msg type
	buf[pos+1] = byte(cfg.Encrypt) // encrypt flag
	// pos+2..pos+3: session_id = 0 (not assigned yet)
	// pos+4..pos+7: seq = 0
	pos += 8
	// Signature placeholder (zeros, filled at the end)
	pos += 16 // total header = 24

	// ---- TLV 1: Protocol version / MTU (type=3, len=4) ----
	buf[pos] = 0x03    // type
	buf[pos+1] = 0x04  // length = 4
	binary.BigEndian.PutUint16(buf[pos+2:pos+4], uint16(cfg.MTU))
	pos += 4

	// ---- TLV 2: Username (type=1, len=len(username)+2) ----
	buf[pos] = 0x01                                   // type
	buf[pos+1] = byte(len(cfg.Username) + 2)          // length
	copy(buf[pos+2:], cfg.Username)                    // value
	pos += 2 + len(cfg.Username)

	// ---- TLV 3: Encrypted password (type=2, len=0x12) ----
	encPW := EncryptPassword(cfg.Username, cfg.Password)
	buf[pos] = 0x02    // type
	buf[pos+1] = 0x12  // length = 18
	copy(buf[pos+2:], encPW) // 16 bytes AES block
	pos += 2 + 16

	// ---- TLV 4 (conditional): encrypt flag ----
	if cfg.Encrypt != 0 {
		buf[pos] = 0x08    // type
		buf[pos+1] = 0x03  // length = 3
		buf[pos+2] = byte(cfg.Encrypt)
		pos += 3
	}

	pkt := buf[:pos]

	// Fill in the signature
	PktSignInPlace(pkt[:24])

	return pkt
}

// BuildEchoReq constructs a heartbeat request (0x15)
func BuildEchoReq(sessionID uint16, seq uint32, timestamp uint64, pipeID, pipeIdx uint32, echoCnt uint32) []byte {
	buf := make([]byte, 60)

	// Header
	buf[0] = MsgECHOREQ
	buf[1] = 0x00
	binary.BigEndian.PutUint16(buf[2:4], sessionID)
	binary.BigEndian.PutUint32(buf[4:8], seq)
	PktSignInPlace(buf[:24])

	// Echo-specific data
	binary.LittleEndian.PutUint64(buf[24:32], timestamp) // microsecond timestamp
	binary.LittleEndian.PutUint32(buf[32:36], pipeID)     // pipeid
	binary.LittleEndian.PutUint32(buf[36:40], pipeIdx)    // pipeidx
	binary.LittleEndian.PutUint32(buf[40:44], echoCnt)    // echorespcnt
	// 4 unused bytes at [44:48] (original leaves these uninitialized)
	// Magic "TDRi" at offset 48 (checked by original code: pktsign_return + 0x18)
	copy(buf[48:52], []byte("TDRi"))
	// session_id (network byte order) at offset 52
	binary.BigEndian.PutUint32(buf[52:56], uint32(sessionID))
	// offset 56-59: reserved (0)

	return buf
}

// ParseSessionID extracts the 2-byte session ID from an OPENACK header (bytes 2-3)
func ParseSessionID(data []byte) uint16 {
	return binary.BigEndian.Uint16(data[2:4])
}

// ParseOPENACKSeq extracts the 4-byte sequence number from OPENACK header (bytes 4-7)
func ParseOPENACKSeq(data []byte) uint32 {
	return binary.BigEndian.Uint32(data[4:8])
}

// OPENACKResult holds parsed TUN configuration from the OPENACK response
type OPENACKResult struct {
	LocalIP    string
	GatewayIP  string
	DNSIP      string
	MTU        uint16
	GateMAC    []byte // 6 bytes gateway MAC
}

// ParseOPENACK extracts TUN config from an OPENACK response.
//
// The original C code assembles IPs as uint32 via:
//
//	(ptr+5)<<24 | (ptr+4)<<16 | (ptr+3)<<8 | (ptr+2)
//
// This produces a "byte-swapped" uint32 that, when stored in x86
// little-endian memory and fed to inet_ntop, produces the correct
// dotted-decimal IP. The wire bytes are already in the right order.
//
// We skip the round-trip and read the bytes directly.
func ParseOPENACK(data []byte) *OPENACKResult {
	r := &OPENACKResult{}
	pos := 24 // skip 24-byte header
	for pos+1 < len(data) {
		t := data[pos]
		length := int(data[pos+1])
		if length < 2 || pos+length > len(data) {
			break
		}
		// value starts at pos+2, has length-2 bytes
		valueStart := pos + 2

		switch t {
		case 3: // MTU (2 bytes big-endian)
			if length >= 4 {
				r.MTU = uint16(data[valueStart])<<8 | uint16(data[valueStart+1])
			}
		case 4: // Local TUN IP (4 bytes, wire-order)
			if length >= 6 {
				r.LocalIP = fmt.Sprintf("%d.%d.%d.%d",
					data[valueStart], data[valueStart+1], data[valueStart+2], data[valueStart+3])
			}
		case 5: // Gateway IP (4 bytes)
			if length >= 10 {
				r.GatewayIP = fmt.Sprintf("%d.%d.%d.%d",
					data[valueStart], data[valueStart+1], data[valueStart+2], data[valueStart+3])
			}
		case 6: // DNS IP (4 bytes)
			if length >= 6 {
				r.DNSIP = fmt.Sprintf("%d.%d.%d.%d",
					data[valueStart], data[valueStart+1], data[valueStart+2], data[valueStart+3])
			}
		}
		pos += length // advance to next TLV
	}
	return r
}

// msgType returns the message type byte from packet data
func MsgType(data []byte) byte {
	if len(data) < 1 {
		return 0
	}
	return data[0]
}
