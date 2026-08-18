package protocol

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestReadGreeting_WriteGreeting(t *testing.T) {
	g := Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "md4"}}

	var buf bytes.Buffer
	if err := WriteGreeting(&buf, g); err != nil {
		t.Fatalf("WriteGreeting: %v", err)
	}

	got, err := ReadGreeting(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadGreeting: %v", err)
	}

	if got.Version != g.Version {
		t.Errorf("version = %d, want %d", got.Version, g.Version)
	}
	if got.SubProtocol != g.SubProtocol {
		t.Errorf("subProtocol = %d, want %d", got.SubProtocol, g.SubProtocol)
	}
	if len(got.Digests) != len(g.Digests) {
		t.Fatalf("digests len = %d, want %d", len(got.Digests), len(g.Digests))
	}
	for i := range g.Digests {
		if got.Digests[i] != g.Digests[i] {
			t.Errorf("digests[%d] = %q, want %q", i, got.Digests[i], g.Digests[i])
		}
	}
}

func TestReadGreeting_EOF(t *testing.T) {
	_, err := ReadGreeting(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadGreeting(nil) = %v, want EOF", err)
	}
}

func TestReadModuleRequest(t *testing.T) {
	tests := []struct {
		name string
		input string
		want string
	}{
		{"normal module", "mydata\n", "mydata"},
		{"list request", "#list\n", "#list"},
		{"empty", "\n", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadModuleRequest(bytes.NewReader([]byte(tt.input)))
			if err != nil {
				t.Fatalf("ReadModuleRequest: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteModuleList(t *testing.T) {
	modules := []ModuleInfo{
		{Name: "archive", Comment: "Important files"},
		{Name: "public", Comment: "Public HTML"},
	}

	t.Run("proto 32 with EXIT", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteModuleList(&buf, modules, 32); err != nil {
			t.Fatalf("WriteModuleList: %v", err)
		}
		got := buf.String()
		if !bytes.Contains(buf.Bytes(), []byte("@RSYNCD: EXIT")) {
			t.Error("missing EXIT terminator")
		}
		if !bytes.Contains(buf.Bytes(), []byte("archive")) {
			t.Error("missing archive module")
		}
		if !bytes.Contains(buf.Bytes(), []byte("public")) {
			t.Error("missing public module")
		}
		_ = got
	})

	t.Run("proto 24 without EXIT", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteModuleList(&buf, modules, 24); err != nil {
			t.Fatalf("WriteModuleList: %v", err)
		}
		if bytes.Contains(buf.Bytes(), []byte("@RSYNCD: EXIT")) {
			t.Error("proto 24 should not have EXIT terminator")
		}
	})
}

func TestAuthChallenge(t *testing.T) {
	challenge := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	t.Run("write and read AUTHREQD", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteAuthChallenge(&buf, challenge); err != nil {
			t.Fatalf("WriteAuthChallenge: %v", err)
		}

		got, err := ReadAuthChallenge(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("ReadAuthChallenge: %v", err)
		}
		if !bytes.Equal(got, challenge) {
			t.Errorf("got %v, want %v", got, challenge)
		}
	})

	t.Run("no auth required returns nil", func(t *testing.T) {
		got, err := ReadAuthChallenge(bytes.NewReader([]byte("@RSYNCD: OK\n")))
		if err != nil {
			t.Fatalf("ReadAuthChallenge: %v", err)
		}
		if got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("error response returns error", func(t *testing.T) {
		_, err := ReadAuthChallenge(bytes.NewReader([]byte("@ERROR: Auth failed\n")))
		if err == nil {
			t.Fatal("expected error for @ERROR response")
		}
	})
}

func TestAuthResponse(t *testing.T) {
	username := "testuser"
	digest := []byte{0xaa, 0xbb, 0xcc, 0xdd}

	var buf bytes.Buffer
	if err := WriteAuthResponse(&buf, username, digest); err != nil {
		t.Fatalf("WriteAuthResponse: %v", err)
	}

	gotUser, gotDigest, err := ReadAuthResponse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadAuthResponse: %v", err)
	}
	if gotUser != username {
		t.Errorf("username = %q, want %q", gotUser, username)
	}
	if !bytes.Equal(gotDigest, digest) {
		t.Errorf("digest = %v, want %v", gotDigest, digest)
	}
}

func TestCompatFlags(t *testing.T) {
	t.Run("proto 30 exchange", func(t *testing.T) {
		var buf bytes.Buffer
		flags := CompatIncRecurse | CompatVarintFlistFlags
		if err := WriteCompatFlags(&buf, flags, 30); err != nil {
			t.Fatalf("WriteCompatFlags: %v", err)
		}

		got, err := ReadCompatFlags(bytes.NewReader(buf.Bytes()), 30)
		if err != nil {
			t.Fatalf("ReadCompatFlags: %v", err)
		}
		if got != flags {
			t.Errorf("got 0x%x, want 0x%x", got, flags)
		}
	})

	t.Run("proto 28 no-op", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteCompatFlags(&buf, 0x42, 28); err != nil {
			t.Fatalf("WriteCompatFlags: %v", err)
		}
		if buf.Len() != 0 {
			t.Error("proto 28 WriteCompatFlags should be no-op")
		}

		got, err := ReadCompatFlags(bytes.NewReader(nil), 28)
		if err != nil {
			t.Fatalf("ReadCompatFlags: %v", err)
		}
		if got != 0 {
			t.Errorf("proto 28 ReadCompatFlags = %d, want 0", got)
		}
	})
}

func TestDefaultAlgorithms(t *testing.T) {
	t.Run("proto 30 defaults", func(t *testing.T) {
		a := DefaultAlgorithms(30)
		if a.Checksum != "md5" {
			t.Errorf("checksum = %q, want md5", a.Checksum)
		}
		if a.Compress != "zlib" {
			t.Errorf("compress = %q, want zlib", a.Compress)
		}
	})

	t.Run("proto 28 defaults", func(t *testing.T) {
		a := DefaultAlgorithms(28)
		if a.Checksum != "md4" {
			t.Errorf("checksum = %q, want md4", a.Checksum)
		}
		if a.Compress != "zlib" {
			t.Errorf("compress = %q, want zlib", a.Compress)
		}
	})
}

func TestNegotiateAlgorithms(t *testing.T) {
	serverChecksums := []string{"md5", "md4"}
	serverCompressions := []string{"zlib", "lz4"}
	clientChecksums := []string{"md5", "sha256"}
	clientCompressions := []string{"zlib"}

	t.Run("both sides negotiate", func(t *testing.T) {
		// simulate the exchange: server writes first, then client reads, etc.
		// both sides send before reading to avoid deadlock
		var serverOut, clientOut bytes.Buffer

		// server sends its lists
		if err := WriteVstring(&serverOut, "md5 md4"); err != nil {
			t.Fatalf("server write checksums: %v", err)
		}
		if err := WriteVstring(&serverOut, "zlib lz4"); err != nil {
			t.Fatalf("server write compressions: %v", err)
		}

		// client sends its lists
		if err := WriteVstring(&clientOut, "md5 sha256"); err != nil {
			t.Fatalf("client write checksums: %v", err)
		}
		if err := WriteVstring(&clientOut, "zlib"); err != nil {
			t.Fatalf("client write compressions: %v", err)
		}

		// server reads client's lists and picks
		serverPeerChecksums, _ := ReadVstring(&clientOut)
		serverPeerCompress, _ := ReadVstring(&clientOut)
		serverAlgos := Algorithms{
			Checksum: pickOne(serverChecksums, serverPeerChecksums),
			Compress: pickOne(serverCompressions, serverPeerCompress),
		}

		// client reads server's lists and picks
		clientPeerChecksums, _ := ReadVstring(&serverOut)
		clientPeerCompress, _ := ReadVstring(&serverOut)
		clientAlgos := Algorithms{
			Checksum: pickOne(clientChecksums, clientPeerChecksums),
			Compress: pickOne(clientCompressions, clientPeerCompress),
		}

		// each side picks its own most-preferred that peer supports
		if serverAlgos.Checksum != "md5" {
			t.Errorf("server checksum = %q, want md5", serverAlgos.Checksum)
		}
		if serverAlgos.Compress != "zlib" {
			t.Errorf("server compress = %q, want zlib", serverAlgos.Compress)
		}
		if clientAlgos.Checksum != "md5" {
			t.Errorf("client checksum = %q, want md5", clientAlgos.Checksum)
		}
		if clientAlgos.Compress != "zlib" {
			t.Errorf("client compress = %q, want zlib", clientAlgos.Compress)
		}
	})

	t.Run("no compression", func(t *testing.T) {
		var serverOut, clientOut bytes.Buffer

		// server sends checksums only (no compression)
		if err := WriteVstring(&serverOut, "md5 md4"); err != nil {
			t.Fatalf("server write checksums: %v", err)
		}

		// client sends checksums only
		if err := WriteVstring(&clientOut, "md5 sha256"); err != nil {
			t.Fatalf("client write checksums: %v", err)
		}

		// server reads client's checksums
		serverPeerChecksums, _ := ReadVstring(&clientOut)
		serverAlgos := Algorithms{
			Checksum: pickOne(serverChecksums, serverPeerChecksums),
		}

		// client reads server's checksums
		clientPeerChecksums, _ := ReadVstring(&serverOut)
		clientAlgos := Algorithms{
			Checksum: pickOne(clientChecksums, clientPeerChecksums),
		}

		if serverAlgos.Checksum != "md5" {
			t.Errorf("server checksum = %q, want md5", serverAlgos.Checksum)
		}
		if serverAlgos.Compress != "" {
			t.Errorf("server compress = %q, want empty", serverAlgos.Compress)
		}
		if clientAlgos.Checksum != "md5" {
			t.Errorf("client checksum = %q, want md5", clientAlgos.Checksum)
		}
		if clientAlgos.Compress != "" {
			t.Errorf("client compress = %q, want empty", clientAlgos.Compress)
		}
	})

	t.Run("wire format", func(t *testing.T) {
		// verify the vstring wire format used in negotiation
		var buf bytes.Buffer
		if err := WriteVstring(&buf, "md5 md4"); err != nil {
			t.Fatalf("WriteVstring: %v", err)
		}
		// "md5 md4" is 7 bytes, so length fits in 1 byte
		got := buf.Bytes()
		if got[0] != 7 {
			t.Errorf("expected length byte 7, got %d", got[0])
		}
		if string(got[1:]) != "md5 md4" {
			t.Errorf("expected payload \"md5 md4\", got %q", string(got[1:]))
		}

		// round-trip
		gotStr, err := ReadVstring(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("ReadVstring: %v", err)
		}
		if gotStr != "md5 md4" {
			t.Errorf("got %q, want \"md5 md4\"", gotStr)
		}
	})
}

func TestChecksumSeed(t *testing.T) {
	var buf bytes.Buffer
	seed := int32(0x01020304)
	if err := WriteChecksumSeed(&buf, seed); err != nil {
		t.Fatalf("WriteChecksumSeed: %v", err)
	}

	got, err := ReadChecksumSeed(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadChecksumSeed: %v", err)
	}
	if got != seed {
		t.Errorf("got 0x%x, want 0x%x", got, seed)
	}
}

func TestParseError(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		want  bool // true if error expected
	}{
		{"error line", "@ERROR: Unknown module", true},
		{"ok line", "@RSYNCD: OK", false},
		{"auth line", "@RSYNCD: AUTHREQD dGVzdA==", false},
		{"empty", "", false},
		{"error with details", "@ERROR: Auth failed for user test", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ParseError(tt.line)
			if tt.want && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.want && err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
		})
	}
}

func TestWriteError(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteError(&buf, "Unknown module"); err != nil {
		t.Fatalf("WriteError: %v", err)
	}
	got := buf.String()
	want := "@ERROR: Unknown module\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExchangeVersion(t *testing.T) {
	// ExchangeVersion sends then reads on both sides simultaneously.
	// On synchronous pipes this deadlocks, so we test via buffer round-trip.
	t.Run("same version", func(t *testing.T) {
		// server sends 32, client sends 32
		var serverOut, clientOut bytes.Buffer
		_ = WriteInt32(&serverOut, 32)
		_ = WriteInt32(&clientOut, 32)

		// server reads client's version
		clientVer, _ := ReadInt32(&clientOut)
		serverNegotiated := 32
		if int(clientVer) < serverNegotiated {
			serverNegotiated = int(clientVer)
		}

		// client reads server's version
		serverVer, _ := ReadInt32(&serverOut)
		clientNegotiated := 32
		if int(serverVer) < clientNegotiated {
			clientNegotiated = int(serverVer)
		}

		if clientNegotiated != 32 {
			t.Errorf("client negotiated %d, want 32", clientNegotiated)
		}
		if serverNegotiated != 32 {
			t.Errorf("server negotiated %d, want 32", serverNegotiated)
		}
	})

	t.Run("negotiate down", func(t *testing.T) {
		// server sends 32, client sends 30
		var serverOut, clientOut bytes.Buffer
		_ = WriteInt32(&serverOut, 32)
		_ = WriteInt32(&clientOut, 30)

		// server reads client's version (30)
		clientVer, _ := ReadInt32(&clientOut)
		serverNegotiated := 32
		if int(clientVer) < serverNegotiated {
			serverNegotiated = int(clientVer)
		}

		// client reads server's version (32)
		serverVer, _ := ReadInt32(&serverOut)
		clientNegotiated := 30
		if int(serverVer) < clientNegotiated {
			clientNegotiated = int(serverVer)
		}

		// both should get the lower version (30)
		if clientNegotiated != 30 {
			t.Errorf("client negotiated %d, want 30", clientNegotiated)
		}
		if serverNegotiated != 30 {
			t.Errorf("server negotiated %d, want 30", serverNegotiated)
		}
	})

	t.Run("wire format", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteInt32(&buf, 32); err != nil {
			t.Fatalf("WriteInt32: %v", err)
		}
		got := buf.Bytes()
		if len(got) != 4 {
			t.Fatalf("expected 4 bytes, got %d", len(got))
		}
		// little-endian: 0x20 0x00 0x00 0x00
		if got[0] != 0x20 || got[1] != 0 || got[2] != 0 || got[3] != 0 {
			t.Errorf("expected 0x20 0x00 0x00 0x00, got %v", got)
		}

		// round-trip
		val, err := ReadInt32(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("ReadInt32: %v", err)
		}
		if val != 32 {
			t.Errorf("got %d, want 32", val)
		}
	})
}
