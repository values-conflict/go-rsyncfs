package protocol

import (
	"reflect"
	"testing"
)

func TestParseGreeting(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    *Greeting
		wantErr bool
	}{
		{
			"Standard v32",
			"@RSYNCD: 32.0 md5 md4\n",
			&Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "md4"}},
			false,
		},
		{
			"Standard v30",
			"@RSYNCD: 30.0 md5\n",
			&Greeting{Version: 30, SubProtocol: 0, Digests: []string{"md5"}},
			false,
		},
		{
			"Subprotocol",
			"@RSYNCD: 32.1 sha256\n",
			&Greeting{Version: 32, SubProtocol: 1, Digests: []string{"sha256"}},
			false,
		},
		{
			"No digests",
			"@RSYNCD: 30.0\n",
			&Greeting{Version: 30, SubProtocol: 0, Digests: nil},
			false,
		},
		{
			"Invalid prefix",
			"HELLO: 32.0 md5\n",
			nil,
			true,
		},
		{
			"Malformed version",
			"@RSYNCD: 32-0 md5\n",
			nil,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := ParseGreeting(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseGreeting() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if !reflect.DeepEqual(g, tt.want) {
					t.Errorf("ParseGreeting() got %+v, want %+v", g, tt.want)
				}
				// round-trip check: Parse -> String should return the original input (normalized)
				if g.String() != tt.input {
					t.Errorf("RoundTrip failed:\nInput: %q\nGot:   %q", tt.input, g.String())
				}
			}
		})
	}
}

func TestNegotiate(t *testing.T) {
	tests := []struct {
		name       string
		client     Greeting
		server     Greeting
		wantVer    int
		wantSub    byte
		wantDigest string
	}{
		{
			"same version and digests",
			Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "sha1"}},
			Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "sha1"}},
			32, 0, "md5",
		},
		{
			"client newer",
			Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "sha1"}},
			Greeting{Version: 30, SubProtocol: 0, Digests: []string{"md4", "md5"}},
			30, 0, "md5",
		},
		{
			"server newer",
			Greeting{Version: 30, SubProtocol: 0, Digests: []string{"md4", "md5"}},
			Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "sha1"}},
			30, 0, "md5",
		},
		{
			"client preference wins -- reversed digest order",
			Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md4", "md5"}},
			Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "md4"}},
			32, 0, "md4", // client prefers md4, so md4 wins
		},
		{
			"subprotocol mismatch (client newer)",
			Greeting{Version: 32, SubProtocol: 1, Digests: []string{"md5"}},
			Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
			31, 0, "md5",
		},
		{
			"subprotocol mismatch (server newer)",
			Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
			Greeting{Version: 32, SubProtocol: 1, Digests: []string{"md5"}},
			31, 0, "md5",
		},
		{
			"subprotocol mismatch (client older)",
			Greeting{Version: 30, SubProtocol: 1, Digests: []string{"md4"}},
			Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
			29, 0, "md4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ver, sub, dig, err := Negotiate(tt.client, tt.server)
			if err != nil {
				t.Errorf("Negotiate() error = %v", err)
				return
			}
			if ver != tt.wantVer || sub != tt.wantSub || dig != tt.wantDigest {
				t.Errorf("Negotiate() got (%d, %d, %q), want (%d, %d, %q)", ver, sub, dig, tt.wantVer, tt.wantSub, tt.wantDigest)
			}
		})
	}
}
