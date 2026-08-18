package protocol

import (
	"bytes"
	"testing"
)

func TestSelectorRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		version int
		sel     Selector
		want    Selector // expected after round-trip (may differ due to proto limits)
	}{
		{
			name:    "proto 28 basic transfer",
			version: 28,
			sel:     Selector{Ndx: 0, Iflags: ItemTransfer | ItemMissingData},
			want:    Selector{Ndx: 0, Iflags: ItemTransfer | ItemMissingData},
		},
		{
			name:    "proto 28 with additional iflags (lost on wire)",
			version: 28,
			sel:     Selector{Ndx: 5, Iflags: ItemTransfer | ItemMissingData | ItemIsNew},
			want:    Selector{Ndx: 5, Iflags: ItemTransfer | ItemMissingData}, // proto 28 can't carry iflags
		},
		{
			name:    "proto 29 transfer",
			version: 29,
			sel:     Selector{Ndx: 0, Iflags: ItemTransfer},
			want:    Selector{Ndx: 0, Iflags: ItemTransfer},
		},
		{
			name:    "proto 30 compressed ndx",
			version: 30,
			sel:     Selector{Ndx: 42, Iflags: ItemTransfer},
			want:    Selector{Ndx: 42, Iflags: ItemTransfer},
		},
		{
			name:    "proto 32 with basis type",
			version: 32,
			sel:     Selector{Ndx: 3, Iflags: ItemTransfer | ItemBasisTypeFollows, BasisType: 0x80},
			want:    Selector{Ndx: 3, Iflags: ItemTransfer | ItemBasisTypeFollows, BasisType: 0x80},
		},
		{
			name:    "proto 32 with xname",
			version: 32,
			sel:     Selector{Ndx: 3, Iflags: ItemTransfer | ItemXnameFollows, Xname: "basis-file.txt"},
			want:    Selector{Ndx: 3, Iflags: ItemTransfer | ItemXnameFollows, Xname: "basis-file.txt"},
		},
		{
			name:    "proto 32 with both optional fields",
			version: 32,
			sel:     Selector{Ndx: 3, Iflags: ItemTransfer | ItemBasisTypeFollows | ItemXnameFollows, BasisType: 0x83, Xname: "fuzzy-match.txt"},
			want:    Selector{Ndx: 3, Iflags: ItemTransfer | ItemBasisTypeFollows | ItemXnameFollows, BasisType: 0x83, Xname: "fuzzy-match.txt"},
		},
		{
			name:    "proto 30 ndx done",
			version: 30,
			sel:     Selector{Ndx: NDxDone},
			want:    Selector{Ndx: NDxDone, Iflags: ItemTransfer | ItemMissingData}, // default iflags on read
		},
		{
			name:    "proto 28 ndx done",
			version: 28,
			sel:     Selector{Ndx: NDxDone},
			want:    Selector{Ndx: NDxDone, Iflags: ItemTransfer | ItemMissingData}, // default iflags on read
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			ndx := NewNdxState()

			if err := WriteSelector(&buf, ndx, tt.version, &tt.sel); err != nil {
				t.Fatalf("write: %v", err)
			}

			ndx2 := NewNdxState()
			got, err := ReadSelector(&buf, ndx2, tt.version)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			if got.Ndx != tt.want.Ndx {
				t.Errorf("ndx: got %d, want %d", got.Ndx, tt.want.Ndx)
			}
			if got.Iflags != tt.want.Iflags {
				t.Errorf("iflags: got %d, want %d", got.Iflags, tt.want.Iflags)
			}
			if got.BasisType != tt.want.BasisType {
				t.Errorf("basisType: got %d, want %d", got.BasisType, tt.want.BasisType)
			}
			if got.Xname != tt.want.Xname {
				t.Errorf("xname: got %q, want %q", got.Xname, tt.want.Xname)
			}
		})
	}
}

func TestSelectorCompressedNdxState(t *testing.T) {
	// sequential indices should encode efficiently with compressed NDX
	sels := []*Selector{
		{Ndx: 0, Iflags: ItemTransfer},
		{Ndx: 1, Iflags: ItemTransfer},
		{Ndx: 2, Iflags: ItemTransfer},
		{Ndx: 100, Iflags: ItemTransfer},
	}

	var buf bytes.Buffer
	ndx := NewNdxState()
	for _, sel := range sels {
		if err := WriteSelector(&buf, ndx, 30, sel); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// verify the wire output is compact (compressed NDX should keep state)
	data := buf.Bytes()
	if len(data) > 30 {
		t.Errorf("wire output too large for 4 sequential selectors: %d bytes", len(data))
	}

	// round-trip through separate NdxState
	ndx2 := NewNdxState()
	for i, want := range sels {
		got, err := ReadSelector(&buf, ndx2, 30)
		if err != nil {
			t.Fatalf("read selector %d: %v", i, err)
		}
		if got.Ndx != want.Ndx {
			t.Errorf("selector %d: ndx = %d, want %d", i, got.Ndx, want.Ndx)
		}
	}
}

func TestSelectorProtoVersionDifferences(t *testing.T) {
	sel := &Selector{Ndx: 5, Iflags: ItemTransfer}

	// proto 28: no iflags on wire, defaults to ItemTransfer|ItemMissingData
	var buf28 bytes.Buffer
	if err := WriteSelector(&buf28, NewNdxState(), 28, sel); err != nil {
		t.Fatalf("write proto 28: %v", err)
	}
	got28, err := ReadSelector(&buf28, NewNdxState(), 28)
	if err != nil {
		t.Fatalf("read proto 28: %v", err)
	}
	if got28.Iflags != ItemTransfer|ItemMissingData {
		t.Errorf("proto 28 default iflags: got %d, want %d", got28.Iflags, ItemTransfer|ItemMissingData)
	}

	// proto 29: iflags on wire as uint16 LE
	var buf29 bytes.Buffer
	if err := WriteSelector(&buf29, NewNdxState(), 29, sel); err != nil {
		t.Fatalf("write proto 29: %v", err)
	}
	got29, err := ReadSelector(&buf29, NewNdxState(), 29)
	if err != nil {
		t.Fatalf("read proto 29: %v", err)
	}
	if got29.Iflags != sel.Iflags {
		t.Errorf("proto 29 iflags: got %d, want %d", got29.Iflags, sel.Iflags)
	}

	// proto 30: compressed NDX + iflags
	var buf30 bytes.Buffer
	if err := WriteSelector(&buf30, NewNdxState(), 30, sel); err != nil {
		t.Fatalf("write proto 30: %v", err)
	}
	got30, err := ReadSelector(&buf30, NewNdxState(), 30)
	if err != nil {
		t.Fatalf("read proto 30: %v", err)
	}
	if got30.Iflags != sel.Iflags {
		t.Errorf("proto 30 iflags: got %d, want %d", got30.Iflags, sel.Iflags)
	}
}

func TestSelectorNdxDoneNoIflags(t *testing.T) {
	// NDX_DONE should not have iflags written (even for proto >= 29)
	sel := Selector{Ndx: NDxDone}

	var buf bytes.Buffer
	if err := WriteSelector(&buf, NewNdxState(), 30, &sel); err != nil {
		t.Fatalf("write: %v", err)
	}

	// for proto 30, NDX_DONE is single byte 0x00
	data := buf.Bytes()
	if len(data) != 1 || data[0] != 0 {
		t.Errorf("NDX_DONE wire output: got %v, want [0x00]", data)
	}
}

func TestSelectorEmptyXname(t *testing.T) {
	// xname can be empty string
	sel := Selector{
		Ndx:    0,
		Iflags: ItemTransfer | ItemXnameFollows,
		Xname:  "",
	}

	var buf bytes.Buffer
	if err := WriteSelector(&buf, NewNdxState(), 30, &sel); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ReadSelector(&buf, NewNdxState(), 30)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Xname != "" {
		t.Errorf("xname: got %q, want empty string", got.Xname)
	}
}

func TestSelectorTruncatedRead(t *testing.T) {
	// incomplete wire data should return an error
	var buf bytes.Buffer
	// write only NDX (compressed, single byte) but no iflags
	ndx := NewNdxState()
	if err := ndx.WriteNdx(&buf, 0); err != nil {
		t.Fatalf("write ndx: %v", err)
	}

	_, err := ReadSelector(&buf, NewNdxState(), 30)
	if err == nil {
		t.Error("expected error reading truncated selector")
	}
}
