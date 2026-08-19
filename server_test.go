package rsyncfs

import (
	"errors"
	"testing"
	"testing/fstest"
)

func TestNewServer(t *testing.T) {
	t.Run("empty modules", func(t *testing.T) {
		s, err := NewServer()
		if err != nil {
			t.Fatalf("NewServer() returned error: %v", err)
		}
		if s == nil {
			t.Fatal("NewServer() returned nil server")
		}
		if len(s.modules) != 0 {
			t.Errorf("expected 0 modules, got %d", len(s.modules))
		}
	})

	t.Run("single module", func(t *testing.T) {
		m := &ServerModule{
			Name:    "testmod",
			Comment: "a test module",
			FS:      fstest.MapFS{"file": {Data: []byte("hello")}},
		}
		s, err := NewServer(m)
		if err != nil {
			t.Fatalf("NewServer() returned error: %v", err)
		}
		if len(s.modules) != 1 {
			t.Fatalf("expected 1 module, got %d", len(s.modules))
		}
		if s.modules["testmod"] != m {
			t.Error("module not stored correctly")
		}
	})

	t.Run("multiple modules", func(t *testing.T) {
		m1 := &ServerModule{Name: "alpha", FS: fstest.MapFS{}}
		m2 := &ServerModule{Name: "beta", FS: fstest.MapFS{}}
		m3 := &ServerModule{Name: "gamma", FS: fstest.MapFS{}}

		s, err := NewServer(m1, m2, m3)
		if err != nil {
			t.Fatalf("NewServer() returned error: %v", err)
		}
		if len(s.modules) != 3 {
			t.Fatalf("expected 3 modules, got %d", len(s.modules))
		}
		for name, want := range map[string]*ServerModule{
			"alpha": m1, "beta": m2, "gamma": m3,
		} {
			if got := s.modules[name]; got != want {
				t.Errorf("modules[%q] = %p, want %p", name, got, want)
			}
		}
	})

	t.Run("duplicate module name", func(t *testing.T) {
		m1 := &ServerModule{Name: "dup", FS: fstest.MapFS{}}
		m2 := &ServerModule{Name: "dup", FS: fstest.MapFS{}}

		_, err := NewServer(m1, m2)
		if err == nil {
			t.Fatal("expected error for duplicate module name, got nil")
		}
		if got := err.Error(); got != `duplicate module name "dup"` {
			t.Errorf("error = %q, want %q", got, `duplicate module name "dup"`)
		}
	})

	t.Run("duplicate in middle of list", func(t *testing.T) {
		_, err := NewServer(
			&ServerModule{Name: "a", FS: fstest.MapFS{}},
			&ServerModule{Name: "b", FS: fstest.MapFS{}},
			&ServerModule{Name: "a", FS: fstest.MapFS{}},
		)
		if err == nil {
			t.Fatal("expected error for duplicate module name, got nil")
		}
	})
}

func TestServerModuleAuthCallback(t *testing.T) {
	t.Run("nil callback means no auth", func(t *testing.T) {
		m := &ServerModule{
			Name: "open",
			FS:   fstest.MapFS{},
		}
		if m.AuthCallback != nil {
			t.Error("AuthCallback should be nil by default")
		}
	})

	t.Run("callback returns valid digest", func(t *testing.T) {
		expected := []byte("expected-digest-bytes")
		m := &ServerModule{
			Name: "secure",
			FS:   fstest.MapFS{},
			AuthCallback: func(username string, challenge []byte) ([]byte, error) {
				if username != "admin" {
					return nil, errors.New("unknown user")
				}
				return expected, nil
			},
		}
		got, err := m.AuthCallback("admin", []byte("challenge"))
		if err != nil {
			t.Fatalf("AuthCallback returned error: %v", err)
		}
		if string(got) != string(expected) {
			t.Errorf("got %q, want %q", got, expected)
		}
	})

	t.Run("callback rejects bad username", func(t *testing.T) {
		m := &ServerModule{
			Name: "secure",
			FS:   fstest.MapFS{},
			AuthCallback: func(username string, challenge []byte) ([]byte, error) {
				return nil, errors.New("auth failed")
			},
		}
		_, err := m.AuthCallback("nobody", nil)
		if err == nil {
			t.Fatal("expected error from AuthCallback, got nil")
		}
	})
}

func TestServerGreetingDefaults(t *testing.T) {
	s, err := NewServer()
	if err != nil {
		t.Fatalf("NewServer() returned error: %v", err)
	}
	// zero-value Greeting should have zero values before ApplyDefaults
	if s.Greeting.Version != 0 {
		t.Errorf("Greeting.Version = %d, want 0", s.Greeting.Version)
	}
	if len(s.Greeting.Digests) != 0 {
		t.Errorf("Greeting.Digests = %v, want nil", s.Greeting.Digests)
	}
}
