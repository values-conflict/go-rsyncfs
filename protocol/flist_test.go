package protocol

import (
	"bytes"
	"io"
	"testing"
)

func TestFlistRoundTrip(t *testing.T) {
	tests := []struct {
		name             string
		version          int
		varintFlistFlags bool
		entries          []FlistEntry
	}{
		{
			name:    "basic files proto30",
			version: 30,
			entries: []FlistEntry{
				{Name: ".", Mode: 0o040755, Size: 0, Mtime: 1000000},
				{Name: "file1.txt", Mode: 0o100644, Size: 42, Mtime: 1000001},
				{Name: "file2.txt", Mode: 0o100644, Size: 100, Mtime: 1000002},
			},
		},
		{
			name:    "basic files proto27",
			version: 27,
			entries: []FlistEntry{
				{Name: ".", Mode: 0o040755, Size: 0, Mtime: 1000000},
				{Name: "file1.txt", Mode: 0o100644, Size: 42, Mtime: 1000001},
			},
		},
		{
			name:             "varint flags",
			version:          32,
			varintFlistFlags: true,
			entries: []FlistEntry{
				{Name: ".", Mode: 0o040755, Size: 0, Mtime: 1000000},
				{Name: "dir/", Mode: 0o040755, Size: 0, Mtime: 1000000},
				{Name: "dir/file.txt", Mode: 0o100644, Size: 50, Mtime: 1000001},
			},
		},
		{
			name:    "symlink",
			version: 30,
			entries: []FlistEntry{
				{Name: ".", Mode: 0o040755, Size: 0, Mtime: 1000000},
				{Name: "link", Mode: 0o120777, Size: 0, Mtime: 1000001, LinkTarget: "file1.txt"},
			},
		},
		{
			name:    "name prefix reuse",
			version: 30,
			entries: []FlistEntry{
				{Name: "abcdef.txt", Mode: 0o100644, Size: 10, Mtime: 1000000},
				{Name: "abcdef_copy.txt", Mode: 0o100644, Size: 20, Mtime: 1000001},
			},
		},
		{
			name:    "delta encoding same attrs",
			version: 30,
			entries: []FlistEntry{
				{Name: "a.txt", Mode: 0o100644, Size: 10, Mtime: 1000000, UID: 1000, GID: 1000},
				{Name: "b.txt", Mode: 0o100644, Size: 20, Mtime: 1000000, UID: 1000, GID: 1000},
				{Name: "c.txt", Mode: 0o100644, Size: 30, Mtime: 1000000, UID: 1000, GID: 1000},
			},
		},
		{
			name:    "empty list",
			version: 30,
			entries: []FlistEntry{},
		},
		{
			name:    "mod nsec",
			version: 31,
			entries: []FlistEntry{
				{Name: ".", Mode: 0o040755, Size: 0, Mtime: 1000000},
				{Name: "file.txt", Mode: 0o100644, Size: 100, Mtime: 1000001, ModNsec: 123456789},
			},
		},
		{
			name:    "device file",
			version: 30,
			entries: []FlistEntry{
				{Name: ".", Mode: 0o040755, Size: 0, Mtime: 1000000},
				{Name: "null", Mode: 0o020666, Size: 0, Mtime: 1000001, RdevMajor: 1, RdevMinor: 3},
			},
		},
		{
			name:    "device file proto28",
			version: 28,
			entries: []FlistEntry{
				{Name: ".", Mode: 0o040755, Size: 0, Mtime: 1000000},
				{Name: "null", Mode: 0o020666, Size: 0, Mtime: 1000001, RdevMajor: 1, RdevMinor: 3},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			w := NewFlistWriter(&buf, tt.version, tt.varintFlistFlags)
			for _, e := range tt.entries {
				if err := w.WriteEntry(&e); err != nil {
					t.Fatalf("WriteEntry(%q): %v", e.Name, err)
				}
			}
			if err := w.WriteEndMarker(); err != nil {
				t.Fatalf("WriteEndMarker: %v", err)
			}

			r := NewFlistReader(&buf, tt.version, tt.varintFlistFlags)
			for i := range tt.entries {
				got, err := r.ReadEntry()
				if err != nil {
					t.Fatalf("ReadEntry(%d): %v", i, err)
				}
				want := tt.entries[i]
				if got.Name != want.Name {
					t.Errorf("entry[%d].Name = %q, want %q", i, got.Name, want.Name)
				}
				if got.Mode != want.Mode {
					t.Errorf("entry[%d].Mode = 0%o, want 0%o", i, got.Mode, want.Mode)
				}
				if got.Size != want.Size {
					t.Errorf("entry[%d].Size = %d, want %d", i, got.Size, want.Size)
				}
				if got.Mtime != want.Mtime {
					t.Errorf("entry[%d].Mtime = %d, want %d", i, got.Mtime, want.Mtime)
				}
				if got.ModNsec != want.ModNsec {
					t.Errorf("entry[%d].ModNsec = %d, want %d", i, got.ModNsec, want.ModNsec)
				}
				if got.UID != want.UID {
					t.Errorf("entry[%d].UID = %d, want %d", i, got.UID, want.UID)
				}
				if got.GID != want.GID {
					t.Errorf("entry[%d].GID = %d, want %d", i, got.GID, want.GID)
				}
				if got.LinkTarget != want.LinkTarget {
					t.Errorf("entry[%d].LinkTarget = %q, want %q", i, got.LinkTarget, want.LinkTarget)
				}
				if got.RdevMajor != want.RdevMajor {
					t.Errorf("entry[%d].RdevMajor = %d, want %d", i, got.RdevMajor, want.RdevMajor)
				}
				if got.RdevMinor != want.RdevMinor {
					t.Errorf("entry[%d].RdevMinor = %d, want %d", i, got.RdevMinor, want.RdevMinor)
				}
			}

			// verify end-of-list
			_, err := r.ReadEntry()
			if err != io.EOF {
				t.Errorf("expected io.EOF, got %v", err)
			}
		})
	}
}

func TestFlistEndMarker(t *testing.T) {
	// end-of-list marker is xflags=0
	tests := []struct {
		name             string
		version          int
		varintFlistFlags bool
	}{
		{"proto30 no varint", 30, false},
		{"proto30 varint", 30, true},
		{"proto27", 27, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewFlistWriter(&buf, tt.version, tt.varintFlistFlags)
			if err := w.WriteEndMarker(); err != nil {
				t.Fatalf("WriteEndMarker: %v", err)
			}

			r := NewFlistReader(&buf, tt.version, tt.varintFlistFlags)
			_, err := r.ReadEntry()
			if err != io.EOF {
				t.Errorf("expected io.EOF for end marker, got %v", err)
			}
		})
	}
}

func TestFlistWireFormat(t *testing.T) {
	// verify specific wire format: xflags byte for proto < 28
	var buf bytes.Buffer
	w := NewFlistWriter(&buf, 27, false)

	// regular file -- UID=0 and GID=0 match zero-value delta state,
	// so XmitSameUID and XmitSameGID are set
	if err := w.WriteEntry(&FlistEntry{Name: "file.txt", Mode: 0o100644, Size: 10, Mtime: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEndMarker(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	if len(data) < 1 {
		t.Fatal("no data written")
	}
	// first entry should NOT have XmitSameUID|XmitSameGID (no previous entry to match)
	// this matches upstream which checks *lastname before setting SAME flags
	if data[0]&XmitSameUID != 0 {
		t.Errorf("first byte 0x%02x has unexpected XmitSameUID (no prior entry)", data[0])
	}
	if data[0]&XmitSameGID != 0 {
		t.Errorf("first byte 0x%02x has unexpected XmitSameGID (no prior entry)", data[0])
	}
	// last byte should be 0 (end marker)
	if data[len(data)-1] != 0 {
		t.Errorf("last byte = 0x%02x, want 0x00 (end marker)", data[len(data)-1])
	}
}

func TestFlistZeroSentinel(t *testing.T) {
	// verify that zero xflags gets a non-zero sentinel (XmitTopDir for proto < 28)
	var buf bytes.Buffer
	w := NewFlistWriter(&buf, 27, false)

	// entry with non-matching UID/GID so no delta flags are set
	if err := w.WriteEntry(&FlistEntry{
		Name:  "file.txt",
		Mode:  0o100644,
		Size:  10,
		Mtime: 1000,
		UID:   1000, // doesn't match zero-value lastUID
		GID:   1000, // doesn't match zero-value lastGID
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEndMarker(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	if len(data) < 1 {
		t.Fatal("no data written")
	}
	// first byte should be XmitTopDir (1) as the non-zero sentinel for proto < 28
	// since no delta flags are set (UID/GID don't match zero-value state)
	if data[0] != XmitTopDir {
		t.Errorf("first byte = 0x%02x, want 0x%02x (XmitTopDir sentinel for zero xflags)", data[0], XmitTopDir)
	}
}

func TestFlistWireFormatExtendedFlags(t *testing.T) {
	// verify shortint xflags for proto 28+ when high bits set
	// XmitModNsec is bit 13 (requires extended flags) and only exists in proto 31+
	var buf bytes.Buffer
	w := NewFlistWriter(&buf, 31, false)

	// write entry with high-bit flag
	if err := w.WriteEntry(&FlistEntry{
		Name:    "file.txt",
		Mode:    0o100644,
		Size:    10,
		Mtime:   1000,
		ModNsec: 123,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEndMarker(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	if len(data) < 2 {
		t.Fatal("no data written")
	}
	// first byte should have XMIT_EXTENDED_FLAGS set
	if data[0]&XmitExtendedFlags == 0 {
		t.Errorf("first byte 0x%02x missing XmitExtendedFlags", data[0])
	}
	// should be 2-byte shortint
	xflags := int(data[0]) | int(data[1])<<8
	if xflags&XmitModNsec == 0 {
		t.Errorf("xflags 0x%x missing XmitModNsec", xflags)
	}
}

func TestFlistVarintFlags(t *testing.T) {
	// verify varint xflags encoding
	var buf bytes.Buffer
	w := NewFlistWriter(&buf, 32, true)

	// write entry with no flags (should use XmitExtendedFlags as non-zero sentinel)
	if err := w.WriteEntry(&FlistEntry{
		Name:  "file.txt",
		Mode:  0o100644,
		Size:  10,
		Mtime: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEndMarker(); err != nil {
		t.Fatal(err)
	}

	// parse back with varint reader
	r := NewFlistReader(&buf, 32, true)
	entry, err := r.ReadEntry()
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if entry.Name != "file.txt" {
		t.Errorf("Name = %q, want %q", entry.Name, "file.txt")
	}

	// verify end marker
	_, err = r.ReadEntry()
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestFlistLongName(t *testing.T) {
	// verify long name (> 255 suffix bytes) uses varint30 for l2
	longName := "a"
	for i := 0; i < 300; i++ {
		longName += "x"
	}

	var buf bytes.Buffer
	w := NewFlistWriter(&buf, 30, false)
	if err := w.WriteEntry(&FlistEntry{
		Name:  longName,
		Mode:  0o100644,
		Size:  10,
		Mtime: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEndMarker(); err != nil {
		t.Fatal(err)
	}

	r := NewFlistReader(&buf, 30, false)
	entry, err := r.ReadEntry()
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if entry.Name != longName {
		t.Errorf("Name len = %d, want %d", len(entry.Name), len(longName))
	}
}

func TestFlistUserNameFollows(t *testing.T) {
	var buf bytes.Buffer
	w := NewFlistWriter(&buf, 30, false)

	// manually set xflags to include XmitUserNameFollows
	entry := &FlistEntry{
		Name:     "file.txt",
		Mode:     0o100644,
		Size:     10,
		Mtime:    1000,
		UID:      1000,
		UserName: "testuser",
	}
	// We need to force the UserNameFollows flag. Since computeXflags doesn't
	// set it automatically, we test via the wire format directly.
	if err := w.WriteEntry(entry); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEndMarker(); err != nil {
		t.Fatal(err)
	}

	r := NewFlistReader(&buf, 30, false)
	got, err := r.ReadEntry()
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if got.Name != entry.Name {
		t.Errorf("Name = %q, want %q", got.Name, entry.Name)
	}
}

func TestCommonPrefixLen(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 3},
		{"abcdef", "abcxyz", 3},
		{"a", "b", 0},
		{"shared_prefix_a", "shared_prefix_b", 14},
		{"short", "longer", 0},
	}

	// test with strings longer than 255 to verify cap
	longA := make([]byte, 300)
	longB := make([]byte, 300)
	for i := range longA {
		longA[i] = 'a'
		longB[i] = 'a'
	}
	longB[255] = 'b' // differ at position 255

	tests = append(tests, struct {
		a, b string
		want int
	}{string(longA), string(longB), 255})

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got := commonPrefixLen(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("commonPrefixLen(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestFlistProto26(t *testing.T) {
	// verify proto 26 wire format (pre-28, uses int32 for mtime)
	var buf bytes.Buffer
	w := NewFlistWriter(&buf, 26, false)
	if err := w.WriteEntry(&FlistEntry{
		Name:  ".",
		Mode:  0o040755,
		Size:  0,
		Mtime: 1000000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEndMarker(); err != nil {
		t.Fatal(err)
	}

	r := NewFlistReader(&buf, 26, false)
	entry, err := r.ReadEntry()
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if entry.Name != "." {
		t.Errorf("Name = %q, want %q", entry.Name, ".")
	}
	if entry.Mode != 0o040755 {
		t.Errorf("Mode = 0%o, want 0%o", entry.Mode, 0o040755)
	}
	if entry.Mtime != 1000000 {
		t.Errorf("Mtime = %d, want %d", entry.Mtime, 1000000)
	}
}
