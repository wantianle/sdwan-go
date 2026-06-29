package core

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"encoding/binary"
	"reflect"
	"testing"
)

// =============================================================================
// msgType
// =============================================================================

func TestMsgType(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected byte
	}{
		{
			name:     "valid OPEN packet",
			input:    []byte{MsgOPEN, 0x00, 0x00, 0x00},
			expected: MsgOPEN,
		},
		{
			name:     "valid OPENACK packet",
			input:    []byte{MsgOPENACK, 0x01, 0x02, 0x03},
			expected: MsgOPENACK,
		},
		{
			name:     "valid ECHOREQ packet",
			input:    []byte{MsgECHOREQ},
			expected: MsgECHOREQ,
		},
		{
			name:     "valid DATA packet",
			input:    []byte{MsgDATA, 0xFF},
			expected: MsgDATA,
		},
		{
			name:     "empty packet",
			input:    []byte{},
			expected: 0,
		},
		{
			name:     "nil input",
			input:    nil,
			expected: 0,
		},
		{
			name:     "single byte zero",
			input:    []byte{0x00},
			expected: 0x00,
		},
		{
			name:     "single byte max",
			input:    []byte{0xFF},
			expected: 0xFF,
		},
		{
			name:     "TUNSetup packet",
			input:    []byte{MsgTUNSetup, 0x00, 0x01},
			expected: MsgTUNSetup,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MsgType(tt.input)
			if got != tt.expected {
				t.Errorf("MsgType(%v) = 0x%02X, want 0x%02X", tt.input, got, tt.expected)
			}
		})
	}
}

// =============================================================================
// PktSign and PktVerify
// =============================================================================

func TestPktSignRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		header []byte
	}{
		{
			name:   "standard header all zeros",
			header: make([]byte, 24),
		},
		{
			name:   "OPEN header (msg=0x13, encrypt=1)",
			header: []byte{0x13, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		},
		{
			name:   "ECHOREQ header with session",
			header: func() []byte {
				h := make([]byte, 24)
				h[0] = MsgECHOREQ
				binary.BigEndian.PutUint16(h[2:4], 0x1234)
				binary.BigEndian.PutUint32(h[4:8], 42)
				return h
			}(),
		},
		{
			name:   "header with all bits set in first 8 bytes",
			header: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clone header so we can modify it
			orig := make([]byte, len(tt.header))
			copy(orig, tt.header)

			// Sign the header
			sig := PktSign(tt.header)
			if len(sig) != 16 {
				t.Errorf("PktSign returned %d bytes, want 16", len(sig))
			}

			// Write signature into header
			copy(tt.header[8:24], sig)

			// Verify returns true
			if !PktVerify(tt.header) {
				t.Error("PktVerify returned false for valid signature")
			}

			// Tamper with first byte of signature → verification should fail
			tampered := make([]byte, len(tt.header))
			copy(tampered, tt.header)
			tampered[8] ^= 0x01
			if PktVerify(tampered) {
				t.Error("PktVerify returned true for tampered signature (byte 8)")
			}

			// Tamper with header data → verification should fail
			tampered2 := make([]byte, len(orig))
			copy(tampered2, orig)
			tampered2[0] ^= 0x01
			sig2 := PktSign(tampered2)
			copy(tampered2[8:24], sig2)
			tampered2[0] ^= 0x01 // revert header but keep old sig
			if PktVerify(tampered2) {
				t.Error("PktVerify returned true for header-data mismatch")
			}

			// Verify original header still passes
			if !PktVerify(tt.header) {
				t.Error("PktVerify returned false for original header after tampering checks")
			}
		})
	}
}

func TestPktSignDeterministic(t *testing.T) {
	header := []byte{0x13, 0x01, 0x12, 0x34, 0x00, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	sig1 := PktSign(header)
	sig2 := PktSign(header)

	if !bytes.Equal(sig1, sig2) {
		t.Error("PktSign is not deterministic: same header produced different signatures")
	}
}

// Known test vector: header[0:8] = "mwtest01", then MD5("mwtest01" + "mw")
// MD5 of "mwtest01mw" = ...
// Pre-compute with crypto/md5 so the test is self-documenting.
func TestPktSignKnownVector(t *testing.T) {
	header := make([]byte, 24)
	copy(header[:8], []byte("mwtest01"))

	// Independent computation of expected signature
	h := md5.New()
	h.Write(header[:8])
	h.Write([]byte("mw"))
	expected := h.Sum(nil)

	got := PktSign(header)
	if !bytes.Equal(got, expected) {
		t.Errorf("PktSign known-vector mismatch:\n got: %x\nwant: %x", got, expected)
	}

	// Round-trip verification
	copy(header[8:24], got)
	if !PktVerify(header) {
		t.Error("PktVerify failed for known test vector")
	}
}

func TestPktVerifyEdgeCases(t *testing.T) {
	t.Run("all zeros header", func(t *testing.T) {
		header := make([]byte, 24)
		// Sign and verify
		sig := PktSign(header)
		copy(header[8:24], sig)
		if !PktVerify(header) {
			t.Error("PktVerify failed for all-zeros header")
		}
	})

	t.Run("tampered last signature byte", func(t *testing.T) {
		header := make([]byte, 24)
		sig := PktSign(header)
		copy(header[8:24], sig)
		header[23] ^= 0x01
		if PktVerify(header) {
			t.Error("PktVerify returned true when last byte of signature was tampered")
		}
	})

	t.Run("tampered first header byte", func(t *testing.T) {
		header := []byte{0x13, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		sig := PktSign(header)
		copy(header[8:24], sig)
		header[0] = 0xFF
		if PktVerify(header) {
			t.Error("PktVerify returned true when first header byte was tampered")
		}
	})
}

// =============================================================================
// EncryptPassword
// =============================================================================

func TestEncryptPassword(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
	}{
		{
			name:     "typical username and password",
			username: "testuser",
			password: "secret123",
		},
		{
			name:     "empty password",
			username: "admin",
			password: "",
		},
		{
			name:     "password exactly 16 bytes",
			username: "user1",
			password: "1234567890abcdef",
		},
		{
			name:     "password longer than 16 bytes (truncated)",
			username: "longuser",
			password: "this_is_a_very_long_password_string",
		},
		{
			name:     "single character username",
			username: "a",
			password: "pw",
		},
		{
			name:     "special characters in username",
			username: "user@domain.com",
			password: "p@ssw0rd!",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EncryptPassword(tt.username, tt.password)

			// Must return exactly 16 bytes
			if len(got) != 16 {
				t.Errorf("EncryptPassword returned %d bytes, want 16", len(got))
			}

			// Compute expected value independently using the same algorithm
			h := md5.New()
			h.Write([]byte("mw"))
			h.Write([]byte(tt.username))
			aesKey := h.Sum(nil)

			expectedBlock := make([]byte, 16)
			copy(expectedBlock, tt.password)

			cipher, err := aes.NewCipher(aesKey)
			if err != nil {
				t.Fatalf("aes.NewCipher in test: %v", err)
			}
			cipher.Encrypt(expectedBlock, expectedBlock)

			if !bytes.Equal(got, expectedBlock) {
				t.Errorf("EncryptPassword output mismatch:\n got: %x\nwant: %x", got, expectedBlock)
			}

			// Determinism check: calling twice should produce same result
			got2 := EncryptPassword(tt.username, tt.password)
			if !bytes.Equal(got, got2) {
				t.Error("EncryptPassword is not deterministic")
			}
		})
	}
}

func TestEncryptPasswordDifferentUsersDifferentKeys(t *testing.T) {
	// Same password, different usernames → different output (different AES key)
	result1 := EncryptPassword("userA", "samepass")
	result2 := EncryptPassword("userB", "samepass")

	if bytes.Equal(result1, result2) {
		t.Error("EncryptPassword produced same output for different usernames")
	}
}

func TestEncryptPasswordEmptyUsername(t *testing.T) {
	// Key = MD5("mw" + "") = MD5("mw")
	result := EncryptPassword("", "test")

	h := md5.New()
	h.Write([]byte("mw"))
	aesKey := h.Sum(nil)

	expectedBlock := make([]byte, 16)
	copy(expectedBlock, "test")
	cipher, _ := aes.NewCipher(aesKey)
	cipher.Encrypt(expectedBlock, expectedBlock)

	if !bytes.Equal(result, expectedBlock) {
		t.Errorf("EncryptPassword with empty username mismatch:\n got: %x\nwant: %x", result, expectedBlock)
	}
}

// =============================================================================
// BuildOpenPacket
// =============================================================================

func TestBuildOpenPacket(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			name: "default config without encrypt",
			config: Config{
				Username: "testuser",
				Password: "testpass",
				MTU:      1436,
				Encrypt:  0,
			},
		},
		{
			name: "config with encrypt flag set",
			config: Config{
				Username: "admin",
				Password: "secret",
				MTU:      1500,
				Encrypt:  1,
			},
		},
		{
			name: "config with max MTU",
			config: Config{
				Username: "u",
				Password: "p",
				MTU:      65535,
				Encrypt:  0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkt := BuildOpenPacket(&tt.config)

			// Minimum size: header(24) + TLV1(4) + TLV2(2+len(username)) + TLV3(2+16)
			// + optional encrypt TLV(3)
			minSize := 24 + 4 + 2 + len(tt.config.Username) + 18
			if tt.config.Encrypt != 0 {
				minSize += 3
			}
			if len(pkt) < minSize {
				t.Errorf("packet too short: got %d bytes, want at least %d", len(pkt), minSize)
			}
			if len(pkt) > 1024 {
				t.Errorf("packet too large: got %d bytes", len(pkt))
			}

			// Header field: msg type = 0x13 (OPEN)
			if pkt[0] != MsgOPEN {
				t.Errorf("pkt[0] = 0x%02X, want 0x13 (MsgOPEN)", pkt[0])
			}

			// Header field: encrypt flag
			if pkt[1] != byte(tt.config.Encrypt) {
				t.Errorf("pkt[1] = 0x%02X, want 0x%02X (encrypt flag)", pkt[1], byte(tt.config.Encrypt))
			}

			// Header field: session_id = 0 (bytes 2-3, big-endian)
			sid := binary.BigEndian.Uint16(pkt[2:4])
			if sid != 0 {
				t.Errorf("session_id = %d, want 0", sid)
			}

			// Header field: seq = 0 (bytes 4-7, big-endian)
			seq := binary.BigEndian.Uint32(pkt[4:8])
			if seq != 0 {
				t.Errorf("seq = %d, want 0", seq)
			}

			// Bytes 8-23: signature (16 bytes), should be valid
			if !PktVerify(pkt[:24]) {
				t.Error("PktVerify failed for BuildOpenPacket header signature")
			}

			// TLV structure: verify we can parse the TLVs
			pos := 24
			foundTLV := map[byte]bool{}
			for pos+1 < len(pkt) {
				tlvType := pkt[pos]
				tlvLen := int(pkt[pos+1])
				if tlvLen < 2 || pos+tlvLen > len(pkt) {
					break
				}
				foundTLV[tlvType] = true
				pos += tlvLen
			}

			// Expected TLVs: type 1 (username), type 2 (encrypted pw), type 3 (MTU)
			if !foundTLV[0x01] {
				t.Error("TLV type 0x01 (username) not found")
			}
			if !foundTLV[0x02] {
				t.Error("TLV type 0x02 (encrypted password) not found")
			}
			if !foundTLV[0x03] {
				t.Error("TLV type 0x03 (MTU) not found")
			}

			// If encrypt flag is non-zero, type 0x08 should be present
			if tt.config.Encrypt != 0 && !foundTLV[0x08] {
				t.Error("TLV type 0x08 (encrypt flag) not found but encrypt is set")
			}
			if tt.config.Encrypt == 0 && foundTLV[0x08] {
				t.Error("TLV type 0x08 (encrypt flag) found but encrypt is not set")
			}

			// Verify TLV 3 (MTU) value
			mtuPos := 24 // MTU TLV is always first
			if pkt[mtuPos] == 0x03 {
				mtuVal := binary.BigEndian.Uint16(pkt[mtuPos+2 : mtuPos+4])
				if mtuVal != uint16(tt.config.MTU) {
					t.Errorf("MTU TLV value = %d, want %d", mtuVal, tt.config.MTU)
				}
			}
		})
	}
}

func TestBuildOpenPacketSignatureIntegrity(t *testing.T) {
	cfg := &Config{
		Username: "signeduser",
		Password: "signedpass",
		MTU:      1436,
	}

	pkt := BuildOpenPacket(cfg)

	// Tamper with payload after signing, header signature should still be valid
	// (signature only covers header, which is unchanged)
	if !PktVerify(pkt[:24]) {
		t.Error("header signature invalid after build")
	}

	// Tamper with header, signature should become invalid
	tampered := make([]byte, len(pkt))
	copy(tampered, pkt)
	tampered[0] = 0xFF
	if PktVerify(tampered[:24]) {
		t.Error("tampered header passed signature check")
	}
}

// =============================================================================
// BuildEchoReq
// =============================================================================

func TestBuildEchoReq(t *testing.T) {
	tests := []struct {
		name      string
		sessionID uint16
		seq       uint32
		timestamp uint64
		pipeID    uint32
		pipeIdx   uint32
		echoCnt   uint32
	}{
		{
			name:      "zero values",
			sessionID: 0,
			seq:       0,
			timestamp: 0,
			pipeID:    0,
			pipeIdx:   0,
			echoCnt:   0,
		},
		{
			name:      "typical values",
			sessionID: 0x1234,
			seq:       42,
			timestamp: 1719876543123456,
			pipeID:    1,
			pipeIdx:   0,
			echoCnt:   5,
		},
		{
			name:      "max uint16 session",
			sessionID: 0xFFFF,
			seq:       0xFFFFFFFF,
			timestamp: 0xFFFFFFFFFFFFFFFF,
			pipeID:    0xFFFFFFFF,
			pipeIdx:   0xFFFFFFFF,
			echoCnt:   0xFFFFFFFF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkt := BuildEchoReq(tt.sessionID, tt.seq, tt.timestamp, tt.pipeID, tt.pipeIdx, tt.echoCnt)

			// Packet must be exactly 60 bytes
			if len(pkt) != 60 {
				t.Errorf("packet length = %d, want 60", len(pkt))
			}

			// buf[0] = MsgECHOREQ (0x15)
			if pkt[0] != MsgECHOREQ {
				t.Errorf("pkt[0] = 0x%02X, want 0x15 (MsgECHOREQ)", pkt[0])
			}

			// buf[1] = 0x00
			if pkt[1] != 0x00 {
				t.Errorf("pkt[1] = 0x%02X, want 0x00", pkt[1])
			}

			// sessionID at buf[2:4] (big-endian)
			gotSID := binary.BigEndian.Uint16(pkt[2:4])
			if gotSID != tt.sessionID {
				t.Errorf("sessionID in header = 0x%04X, want 0x%04X", gotSID, tt.sessionID)
			}

			// seq at buf[4:8] (big-endian)
			gotSeq := binary.BigEndian.Uint32(pkt[4:8])
			if gotSeq != tt.seq {
				t.Errorf("seq in header = %d, want %d", gotSeq, tt.seq)
			}

			// Header signature (buf[8:24]) should be valid
			if !PktVerify(pkt[:24]) {
				t.Error("PktVerify failed for BuildEchoReq header")
			}

			// timestamp at buf[24:32] (little-endian, microsecond)
			gotTS := binary.LittleEndian.Uint64(pkt[24:32])
			if gotTS != tt.timestamp {
				t.Errorf("timestamp = %d, want %d", gotTS, tt.timestamp)
			}

			// pipeID at buf[32:36] (little-endian)
			gotPipeID := binary.LittleEndian.Uint32(pkt[32:36])
			if gotPipeID != tt.pipeID {
				t.Errorf("pipeID = %d, want %d", gotPipeID, tt.pipeID)
			}

			// pipeIdx at buf[36:40] (little-endian)
			gotPipeIdx := binary.LittleEndian.Uint32(pkt[36:40])
			if gotPipeIdx != tt.pipeIdx {
				t.Errorf("pipeIdx = %d, want %d", gotPipeIdx, tt.pipeIdx)
			}

			// echoCnt at buf[40:44] (little-endian)
			gotEchoCnt := binary.LittleEndian.Uint32(pkt[40:44])
			if gotEchoCnt != tt.echoCnt {
				t.Errorf("echoCnt = %d, want %d", gotEchoCnt, tt.echoCnt)
			}

			// Magic "TDRi" at offset 48
			magic := string(pkt[48:52])
			if magic != "TDRi" {
				t.Errorf("magic at offset 48 = %q, want %q", magic, "TDRi")
			}

			// sessionID at offset 52 (big-endian, as uint32)
			gotSID2 := binary.BigEndian.Uint32(pkt[52:56])
			if gotSID2 != uint32(tt.sessionID) {
				t.Errorf("sessionID at offset 52 = %d, want %d", gotSID2, tt.sessionID)
			}
		})
	}
}

// =============================================================================
// ParseSessionID
// =============================================================================

func TestParseSessionID(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected uint16
	}{
		{
			name:     "session ID 0x1234",
			data:     []byte{0x00, 0x00, 0x12, 0x34},
			expected: 0x1234,
		},
		{
			name:     "session ID 0x0000",
			data:     []byte{0x00, 0x00, 0x00, 0x00},
			expected: 0x0000,
		},
		{
			name:     "session ID 0xFFFF",
			data:     []byte{0x00, 0x00, 0xFF, 0xFF},
			expected: 0xFFFF,
		},
		{
			name:     "session ID from larger packet",
			data:     []byte{0x12, 0x00, 0xAB, 0xCD, 0x00, 0x00, 0x00, 0x01},
			expected: 0xABCD,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSessionID(tt.data)
			if got != tt.expected {
				t.Errorf("ParseSessionID(%x) = 0x%04X, want 0x%04X", tt.data, got, tt.expected)
			}
		})
	}
}

// =============================================================================
// ParseOPENACKSeq
// =============================================================================

func TestParseOPENACKSeq(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected uint32
	}{
		{
			name:     "seq 42",
			data:     []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2A},
			expected: 42,
		},
		{
			name:     "seq 0",
			data:     []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			expected: 0,
		},
		{
			name:     "seq max uint32",
			data:     []byte{0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF},
			expected: 0xFFFFFFFF,
		},
		{
			name:     "seq from larger packet",
			data:     append([]byte{0x12, 0x00, 0x00, 0x00}, []byte{0xDE, 0xAD, 0xBE, 0xEF}...),
			expected: 0xDEADBEEF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseOPENACKSeq(tt.data)
			if got != tt.expected {
				t.Errorf("ParseOPENACKSeq(%x) = %d, want %d", tt.data, got, tt.expected)
			}
		})
	}
}

// =============================================================================
// ParseOPENACK
// =============================================================================

func TestParseOPENACK(t *testing.T) {
	t.Run("full OPENACK with all TLVs", func(t *testing.T) {
		// Build a mock OPENACK response
		// Header (24 bytes): msg=0x12, encrypt=0, session_id=0x0001, seq=1, signature
		buf := make([]byte, 256)
		pos := 0

		buf[0] = MsgOPENACK // 0x12
		buf[1] = 0x00
		binary.BigEndian.PutUint16(buf[2:4], 0x0001) // session_id
		binary.BigEndian.PutUint32(buf[4:8], 1)       // seq
		pos += 8
		copy(buf[pos:pos+16], make([]byte, 16)) // signature placeholder
		// Actually sign the header
		PktSignInPlace(buf[:24])
		pos = 24

		// TLV type=3 (MTU): length=4, value=2 bytes big-endian
		buf[pos] = 0x03
		buf[pos+1] = 0x04
		binary.BigEndian.PutUint16(buf[pos+2:pos+4], 1436)
		pos += 4

		// TLV type=4 (LocalIP): length=6, value=4 bytes IP
		buf[pos] = 0x04
		buf[pos+1] = 0x06
		buf[pos+2] = 10
		buf[pos+3] = 0
		buf[pos+4] = 0
		buf[pos+5] = 2
		pos += 6

		// TLV type=5 (GatewayIP): length=10, value=4 bytes IP + 6 bytes MAC
		buf[pos] = 0x05
		buf[pos+1] = 0x0A
		buf[pos+2] = 10
		buf[pos+3] = 0
		buf[pos+4] = 0
		buf[pos+5] = 1
		buf[pos+6] = 0xAA
		buf[pos+7] = 0xBB
		buf[pos+8] = 0xCC
		buf[pos+9] = 0xDD
		buf[pos+10] = 0xEE
		buf[pos+11] = 0xFF
		pos += 10

		// TLV type=6 (DNS): length=6, value=4 bytes IP
		buf[pos] = 0x06
		buf[pos+1] = 0x06
		buf[pos+2] = 8
		buf[pos+3] = 8
		buf[pos+4] = 8
		buf[pos+5] = 8
		pos += 6

		data := buf[:pos]
		result := ParseOPENACK(data)

		if result == nil {
			t.Fatal("ParseOPENACK returned nil")
		}

		if result.MTU != 1436 {
			t.Errorf("MTU = %d, want 1436", result.MTU)
		}
		if result.LocalIP != "10.0.0.2" {
			t.Errorf("LocalIP = %q, want %q", result.LocalIP, "10.0.0.2")
		}
		if result.GatewayIP != "10.0.0.1" {
			t.Errorf("GatewayIP = %q, want %q", result.GatewayIP, "10.0.0.1")
		}
		if result.DNSIP != "8.8.8.8" {
			t.Errorf("DNSIP = %q, want %q", result.DNSIP, "8.8.8.8")
		}
	})

	t.Run("OPENACK with DNS but no GatewayIP", func(t *testing.T) {
		buf := make([]byte, 256)
		buf[0] = MsgOPENACK
		buf[1] = 0x00
		binary.BigEndian.PutUint16(buf[2:4], 0x0042)
		binary.BigEndian.PutUint32(buf[4:8], 100)
		PktSignInPlace(buf[:24])

		// Only MTU and DNS TLVs
		pos := 24
		buf[pos] = 0x03
		buf[pos+1] = 0x04
		binary.BigEndian.PutUint16(buf[pos+2:pos+4], 1500)
		pos += 4

		buf[pos] = 0x06
		buf[pos+1] = 0x06
		buf[pos+2] = 1
		buf[pos+3] = 1
		buf[pos+4] = 1
		buf[pos+5] = 1
		pos += 6

		data := buf[:pos]
		result := ParseOPENACK(data)

		if result == nil {
			t.Fatal("ParseOPENACK returned nil")
		}
		if result.MTU != 1500 {
			t.Errorf("MTU = %d, want 1500", result.MTU)
		}
		if result.DNSIP != "1.1.1.1" {
			t.Errorf("DNSIP = %q, want %q", result.DNSIP, "1.1.1.1")
		}
		if result.GatewayIP != "" {
			t.Errorf("GatewayIP = %q, want empty", result.GatewayIP)
		}
		if result.LocalIP != "" {
			t.Errorf("LocalIP = %q, want empty", result.LocalIP)
		}
	})

	t.Run("empty OPENACK (header only)", func(t *testing.T) {
		buf := make([]byte, 24)
		buf[0] = MsgOPENACK
		buf[1] = 0x00
		binary.BigEndian.PutUint16(buf[2:4], 0x0001)
		binary.BigEndian.PutUint32(buf[4:8], 1)
		PktSignInPlace(buf[:24])

		result := ParseOPENACK(buf)

		if result == nil {
			t.Fatal("ParseOPENACK returned nil")
		}
		// All fields should be zero/empty
		if result.MTU != 0 {
			t.Errorf("MTU = %d, want 0", result.MTU)
		}
		if result.LocalIP != "" {
			t.Errorf("LocalIP = %q, want empty", result.LocalIP)
		}
		if result.GatewayIP != "" {
			t.Errorf("GatewayIP = %q, want empty", result.GatewayIP)
		}
		if result.DNSIP != "" {
			t.Errorf("DNSIP = %q, want empty", result.DNSIP)
		}
	})

	t.Run("OPENACK with malformed TLV (length too short)", func(t *testing.T) {
		buf := make([]byte, 256)
		buf[0] = MsgOPENACK
		buf[1] = 0x00
		binary.BigEndian.PutUint16(buf[2:4], 1)
		binary.BigEndian.PutUint32(buf[4:8], 1)
		PktSignInPlace(buf[:24])

		// TLV with length=1 (invalid: need at least 2 for type+length)
		pos := 24
		buf[pos] = 0x03
		buf[pos+1] = 0x01 // length=1, which is < 2 → should break loop
		pos += 1 // won't be consumed fully

		data := buf[:pos+1]
		result := ParseOPENACK(data)

		if result == nil {
			t.Fatal("ParseOPENACK returned nil")
		}
		// Should not panic; result should be mostly empty
		if result.MTU != 0 {
			t.Errorf("MTU = %d, want 0 (malformed TLV should be skipped)", result.MTU)
		}
	})

	t.Run("OPENACK with TLV exceeding buffer", func(t *testing.T) {
		buf := make([]byte, 30)
		buf[0] = MsgOPENACK
		buf[1] = 0x00
		binary.BigEndian.PutUint16(buf[2:4], 1)
		binary.BigEndian.PutUint32(buf[4:8], 1)
		PktSignInPlace(buf[:24])

		// TLV at pos 24 claiming length=100 (exceeds buffer)
		pos := 24
		buf[pos] = 0x03
		buf[pos+1] = 100 // length=100, but buffer is only 30 bytes
		pos += 2

		data := buf[:pos]
		result := ParseOPENACK(data)

		if result == nil {
			t.Fatal("ParseOPENACK returned nil")
		}
		// Should break loop gracefully without panic
	})

	t.Run("OPENACK with truncated TLV value", func(t *testing.T) {
		// TLV type=4 (LocalIP) claims length=6 but only has 5 bytes total
		// (needs 6: 1 byte type + 1 byte len + 4 bytes IP)
		buf := make([]byte, 24)
		buf[0] = MsgOPENACK
		buf[1] = 0x00
		binary.BigEndian.PutUint16(buf[2:4], 1)
		binary.BigEndian.PutUint32(buf[4:8], 1)
		PktSignInPlace(buf[:24])

		// Only 2 extra bytes after header, but TLV says length=6
		data := append(buf, 0x04, 0x06, 0x0A) // type=4, len=6, but only 3 bytes after header
		result := ParseOPENACK(data)

		if result == nil {
			t.Fatal("ParseOPENACK returned nil")
		}
		// Should not panic; LocalIP should be empty (length check fails)
		if result.LocalIP != "" {
			t.Errorf("LocalIP = %q, want empty (truncated TLV)", result.LocalIP)
		}
	})

	t.Run("error response (msg type 0x12 but with error flags)", func(t *testing.T) {
		// Sometimes OPENACK can have an error flag in byte 1
		buf := make([]byte, 24)
		buf[0] = MsgOPENACK
		buf[1] = 0xFF // error flag
		binary.BigEndian.PutUint16(buf[2:4], 0)
		binary.BigEndian.PutUint32(buf[4:8], 0)
		PktSignInPlace(buf[:24])

		result := ParseOPENACK(buf)

		if result == nil {
			t.Fatal("ParseOPENACK returned nil for error response")
		}
		// No TLVs, all fields empty/zero
		if result.MTU != 0 || result.LocalIP != "" {
			t.Error("ParseOPENACK should return default values for error/empty response")
		}
	})
}

// =============================================================================
// ParseSessionID + ParseOPENACKSeq integration test
// =============================================================================

func TestSessionIDSeqRoundTrip(t *testing.T) {
	// Simulate: build a response header, then parse it back
	buf := make([]byte, 24)
	buf[0] = MsgOPENACK
	binary.BigEndian.PutUint16(buf[2:4], 0xABCD)
	binary.BigEndian.PutUint32(buf[4:8], 0x12345678)
	PktSignInPlace(buf[:24])

	sid := ParseSessionID(buf)
	if sid != 0xABCD {
		t.Errorf("ParseSessionID = 0x%04X, want 0xABCD", sid)
	}

	seq := ParseOPENACKSeq(buf)
	if seq != 0x12345678 {
		t.Errorf("ParseOPENACKSeq = 0x%08X, want 0x12345678", seq)
	}
}

// =============================================================================
// Message type constants test
// =============================================================================

func TestMessageConstants(t *testing.T) {
	// Verify all message type constants are distinct and non-zero
	types := map[string]byte{
		"MsgOPENACK":  MsgOPENACK,
		"MsgOPEN":     MsgOPEN,
		"MsgTUNSetup": MsgTUNSetup,
		"MsgECHOREQ":  MsgECHOREQ,
		"MsgECHORESP": MsgECHORESP,
		"MsgDATA":     MsgDATA,
	}

	seen := make(map[byte]string)
	for name, v := range types {
		if v == 0 {
			t.Errorf("%s is zero", name)
		}
		if prev, ok := seen[v]; ok {
			t.Errorf("%s and %s have same value 0x%02X", name, prev, v)
		}
		seen[v] = name
	}

	// Verify specific expected values (from reverse engineering)
	if MsgOPENACK != 0x12 {
		t.Errorf("MsgOPENACK = 0x%02X, want 0x12", MsgOPENACK)
	}
	if MsgOPEN != 0x13 {
		t.Errorf("MsgOPEN = 0x%02X, want 0x13", MsgOPEN)
	}
	if MsgECHOREQ != 0x15 {
		t.Errorf("MsgECHOREQ = 0x%02X, want 0x15", MsgECHOREQ)
	}
	if MsgECHORESP != 0x16 {
		t.Errorf("MsgECHORESP = 0x%02X, want 0x16", MsgECHORESP)
	}
	if MsgDATA != 0x18 {
		t.Errorf("MsgDATA = 0x%02X, want 0x18", MsgDATA)
	}
}

// =============================================================================
// OPENACKResult type checks
// =============================================================================

func TestOPENACKResultType(t *testing.T) {
	// Verify OPENACKResult struct has expected fields
	r := OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
		DNSIP:     "8.8.8.8",
		MTU:       1436,
		GateMAC:   []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
	}

	rt := reflect.TypeOf(r)
	fieldNames := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		fieldNames[rt.Field(i).Name] = true
	}

	expectedFields := []string{"LocalIP", "GatewayIP", "DNSIP", "MTU", "GateMAC"}
	for _, f := range expectedFields {
		if !fieldNames[f] {
			t.Errorf("OPENACKResult missing field %q", f)
		}
	}

	if r.MTU != 1436 {
		t.Errorf("MTU field: got %d", r.MTU)
	}
	if len(r.GateMAC) != 6 {
		t.Errorf("GateMAC field: length %d, want 6", len(r.GateMAC))
	}
}
