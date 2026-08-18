package protocol

import (
	"bytes"
	"io"
	"testing"
)

func TestReadArgs_NullTerminated(t *testing.T) {
	// proto >= 30: null-terminated
	input := []byte(".\x00--server\x00--sender\x00-vle.ifxCIvu\x00.\x00\x00")
	args, err := ReadArgs(bytes.NewReader(input), 30)
	if err != nil {
		t.Fatalf("ReadArgs: %v", err)
	}
	want := []string{".", "--server", "--sender", "-vle.ifxCIvu", "."}
	if len(args) != len(want) {
		t.Fatalf("got %d args, want %d: %v", len(args), len(want), args)
	}
	for i, got := range args {
		if got != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestReadArgs_NewlineTerminated(t *testing.T) {
	// proto < 30: newline-terminated
	input := []byte(".\n--server\n--sender\n-vle.ifxCIvu\n.\n\n")
	args, err := ReadArgs(bytes.NewReader(input), 29)
	if err != nil {
		t.Fatalf("ReadArgs: %v", err)
	}
	want := []string{".", "--server", "--sender", "-vle.ifxCIvu", "."}
	if len(args) != len(want) {
		t.Fatalf("got %d args, want %d: %v", len(args), len(want), args)
	}
	for i, got := range args {
		if got != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestReadArgs_EmptyList(t *testing.T) {
	// just a double delimiter = empty list
	for _, tc := range []struct {
		name  string
		input []byte
		ver   int
	}{
		{"null", []byte("\x00\x00"), 30},
		{"newline", []byte("\n\n"), 29},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := ReadArgs(bytes.NewReader(tc.input), tc.ver)
			if err != nil {
				t.Fatalf("ReadArgs: %v", err)
			}
			if len(args) != 0 {
				t.Fatalf("got %d args, want 0: %v", len(args), args)
			}
		})
	}
}

func TestReadArgs_SingleArg(t *testing.T) {
	input := []byte(".\x00\x00")
	args, err := ReadArgs(bytes.NewReader(input), 30)
	if err != nil {
		t.Fatalf("ReadArgs: %v", err)
	}
	if len(args) != 1 || args[0] != "." {
		t.Fatalf("got %v, want [.]", args)
	}
}

func TestReadArgs_Truncated(t *testing.T) {
	// no double delimiter = EOF
	input := []byte(".\x00--server\x00")
	_, err := ReadArgs(bytes.NewReader(input), 30)
	if err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

func TestWriteArgs_NullTerminated(t *testing.T) {
	var buf bytes.Buffer
	args := []string{".", "--server", "--sender", "-vle.ifxCIvu", "."}
	if err := WriteArgs(&buf, args, 30); err != nil {
		t.Fatalf("WriteArgs: %v", err)
	}
	want := []byte(".\x00--server\x00--sender\x00-vle.ifxCIvu\x00.\x00\x00")
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got %q, want %q", buf.Bytes(), want)
	}
}

func TestWriteArgs_NewlineTerminated(t *testing.T) {
	var buf bytes.Buffer
	args := []string{".", "--server", "--sender", "-vle.ifxCIvu", "."}
	if err := WriteArgs(&buf, args, 29); err != nil {
		t.Fatalf("WriteArgs: %v", err)
	}
	want := []byte(".\n--server\n--sender\n-vle.ifxCIvu\n.\n\n")
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got %q, want %q", buf.Bytes(), want)
	}
}

func TestWriteArgs_EmptyList(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteArgs(&buf, nil, 30); err != nil {
		t.Fatalf("WriteArgs: %v", err)
	}
	want := []byte("\x00\x00")
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got %q, want %q", buf.Bytes(), want)
	}
}

func TestArgs_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		ver  int
		args []string
	}{
		{"null-single", 30, []string{"."}},
		{"null-full", 30, []string{".", "--server", "--sender", "-vle.ifxCIvu", "."}},
		{"newline-single", 29, []string{"."}},
		{"newline-full", 29, []string{".", "--server", "--sender", "-vle.ifxCIvu", "."}},
		{"null-empty", 30, nil},
		{"newline-empty", 29, nil},
		{"null-with-spaces", 30, []string{".", "arg with spaces", "--option=value"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteArgs(&buf, tt.args, tt.ver); err != nil {
				t.Fatalf("WriteArgs: %v", err)
			}
			got, err := ReadArgs(&buf, tt.ver)
			if err != nil {
				t.Fatalf("ReadArgs: %v", err)
			}
			if len(got) != len(tt.args) {
				t.Fatalf("got %d args, want %d: %v", len(got), len(tt.args), got)
			}
			for i, want := range tt.args {
				if got[i] != want {
					t.Errorf("args[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

func TestExtractClientInfo(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantInfo string
	}{
		{
			name:     "standard",
			args:     []string{".", "--server", "--sender", "-vlogDtpr.eiLsfxCIvu", "."},
			wantInfo: "iLsfxCIvu",
		},
		{
			name:     "no-e-flag",
			args:     []string{".", "--server", "--sender", "-vl", "."},
			wantInfo: "",
		},
		{
			name:     "e-at-end",
			args:     []string{".", "--server", "--sender", "-vle", "."},
			wantInfo: "",
		},
		{
			name:     "e-only-flags",
			args:     []string{".", "--server", "--sender", "-eiLsfxCIvu", "."},
			wantInfo: "iLsfxCIvu",
		},
		{
			name:     "empty-list",
			args:     []string{"."},
			wantInfo: "",
		},
		{
			name:     "first-arg-is-dot",
			args:     []string{"."},
			wantInfo: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractClientInfo(tt.args)
			if got != tt.wantInfo {
				t.Errorf("ExtractClientInfo(%v) = %q, want %q", tt.args, got, tt.wantInfo)
			}
		})
	}
}

func TestResolveCompatFlags(t *testing.T) {
	allCaps := CompatIncRecurse | CompatSymlinkTimes | CompatSymlinkIconv |
		CompatSafeFlist | CompatAvoidXattrOptim | CompatChksumSeedFix |
		CompatInplacePartialDir | CompatVarintFlistFlags | CompatId0Names

	tests := []struct {
		name       string
		serverCaps int
		clientInfo string
		want       int
	}{
		{
			name:       "all-flags",
			serverCaps: allCaps,
			clientInfo: "iLsfxCIvu",
			want:       allCaps,
		},
		{
			name:       "empty-client-info",
			serverCaps: allCaps,
			clientInfo: "",
			want:       0,
		},
		{
			name:       "empty-server-caps",
			serverCaps: 0,
			clientInfo: "iLsfxCIvu",
			want:       0,
		},
		{
			name:       "subset-server-caps",
			serverCaps: CompatIncRecurse | CompatVarintFlistFlags,
			clientInfo: "iLsfxCIvu",
			want:       CompatIncRecurse | CompatVarintFlistFlags,
		},
		{
			name:       "subset-client-info",
			serverCaps: allCaps,
			clientInfo: "iv",
			want:       CompatIncRecurse | CompatVarintFlistFlags,
		},
		{
			name:       "unknown-flag-ignored",
			serverCaps: allCaps,
			clientInfo: "iLz",
			want:       CompatIncRecurse | CompatSymlinkTimes,
		},
		{
			name:       "single-flag-i",
			serverCaps: allCaps,
			clientInfo: "i",
			want:       CompatIncRecurse,
		},
		{
			name:       "single-flag-v",
			serverCaps: allCaps,
			clientInfo: "v",
			want:       CompatVarintFlistFlags,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveCompatFlags(tt.serverCaps, tt.clientInfo)
			if got != tt.want {
				t.Errorf("ResolveCompatFlags(0x%x, %q) = 0x%x, want 0x%x",
					tt.serverCaps, tt.clientInfo, got, tt.want)
			}
		})
	}
}
