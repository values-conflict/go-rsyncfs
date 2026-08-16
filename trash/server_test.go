package rsyncfs

import (
	"bytes"
	"testing"
	"testing/fstest"
)

func TestServerAddModule(t *testing.T) {
	m1 := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{"file": {Data: []byte("hello")}},
	}
	s, err := NewServer(m1)
	if err != nil {
		t.Fatalf("failed to create server with module: %v", err)
	}

	// Duplicate name should fail during creation
	_, err = NewServer(m1, m1)
	if err == nil {
		t.Error("expected error when creating server with duplicate modules, got nil")
	}
	_ = s // avoid unused variable if needed, but we are just testing the constructor now
}

func TestServerFormatError(t *testing.T) {
	s, err := NewServer()
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	msg := "Unknown module"
	want := "@ERROR: Unknown module\n"
	if got := s.formatError(msg); got != want {
		t.Errorf("formatError(%q) = %q, want %q", msg, got, want)
	}
}

func TestServerSendError(t *testing.T) {
	s, err := NewServer()
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	buf := bytes.NewBuffer(nil)
	msg := "Something went wrong"
	if err := s.SendError(buf, msg); err != nil {
		t.Fatalf("SendError failed: %v", err)
	}
	want := "@ERROR: Something went wrong\n"
	if buf.String() != want {
		t.Errorf("SendError wrote %q, want %q", buf.String(), want)
	}
}
