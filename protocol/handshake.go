package protocol

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ReadGreeting reads a greeting line from r and parses it.
func ReadGreeting(r io.Reader) (*Greeting, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, fmt.Errorf("read greeting: %w", err)
	}
	return ParseGreeting(line)
}

// WriteGreeting writes a greeting line to w.
func WriteGreeting(w io.Writer, g Greeting) error {
	_, err := io.WriteString(w, g.String())
	return err
}

// readLine reads from r until a newline character is encountered.
// The returned string does not include the trailing newline.
func readLine(r io.Reader) (string, error) {
	var buf strings.Builder
	for {
		b, err := readOne(r)
		if err != nil {
			return "", err
		}
		if b == '\n' {
			break
		}
		buf.WriteByte(b)
	}
	return buf.String(), nil
}

// writeLine writes s followed by a newline to w.
func writeLine(w io.Writer, s string) error {
	if _, err := io.WriteString(w, s); err != nil {
		return err
	}
	_, err := w.Write([]byte{'\n'})
	return err
}

// ReadModuleRequest reads the module name (#list or actual module).
func ReadModuleRequest(r io.Reader) (string, error) {
	return readLine(r)
}

// WriteModuleRequest writes a module name (#list or actual module) as a newline-terminated line.
func WriteModuleRequest(w io.Writer, moduleName string) error {
	return writeLine(w, moduleName)
}

// ModuleInfo holds the name and comment of a single rsync module.
type ModuleInfo struct {
	Name    string
	Comment string
}

// WriteModuleList writes the tab-separated module listing followed by EXIT.
// For proto < 25, the EXIT terminator is omitted (connection close signals end).
func WriteModuleList(w io.Writer, modules []ModuleInfo, version int) error {
	for _, m := range modules {
		line := fmt.Sprintf("%-15s\t%s\n", m.Name, m.Comment)
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	if version >= 25 {
		_, err := io.WriteString(w, "@RSYNCD: EXIT\n")
		return err
	}
	return nil
}

// ReadAuthChallenge reads the server's response to a module request and
// returns the base64-decoded auth challenge.  It returns nil, nil if the
// server sends @RSYNCD: OK (authenticated or no auth required), and an
// error for an @ERROR: line or an @RSYNCD: EXIT.
//
// Lines that are none of the above are MOTD (or module-listing) lines the
// daemon may interleave before the answer; the upstream client reads and
// drops them (start_inband_exchange's response loop), so this skips them
// and keeps reading.
func ReadAuthChallenge(r io.Reader) ([]byte, error) {
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}

		if err := ParseError(line); err != nil {
			return nil, err
		}

		if strings.HasPrefix(line, "@RSYNCD: OK") {
			return nil, nil
		}

		if strings.HasPrefix(line, "@RSYNCD: EXIT") {
			return nil, fmt.Errorf("server exited the module connection: %s", line)
		}

		if challenge, ok := strings.CutPrefix(line, "@RSYNCD: AUTHREQD "); ok {
			challenge = strings.TrimSpace(challenge)
			data, err := base64.StdEncoding.DecodeString(challenge)
			if err != nil {
				return nil, fmt.Errorf("decode auth challenge: %w", err)
			}
			return data, nil
		}

		// MOTD line: skip and read the next response
	}
}

// WriteAuthChallenge writes an AUTHREQD line with base64-encoded challenge.
func WriteAuthChallenge(w io.Writer, challenge []byte) error {
	challengeB64 := base64.StdEncoding.EncodeToString(challenge)
	_, err := io.WriteString(w, fmt.Sprintf("@RSYNCD: AUTHREQD %s\n", challengeB64))
	return err
}

// WriteAuthOK writes the @RSYNCD: OK response.
func WriteAuthOK(w io.Writer) error {
	_, err := io.WriteString(w, "@RSYNCD: OK\n")
	return err
}

// ReadAuthResponse reads the username and base64-encoded digest from the client.
func ReadAuthResponse(r io.Reader) (username string, digest []byte, err error) {
	line, err := readLine(r)
	if err != nil {
		return "", nil, err
	}

	parts := strings.SplitN(line, " ", 2)
	if len(parts) < 2 {
		return "", nil, fmt.Errorf("invalid auth response format: %s", line)
	}

	data, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", nil, fmt.Errorf("decode auth digest: %w", err)
	}
	return parts[0], data, nil
}

// WriteAuthResponse writes the username and base64-encoded digest to the server.
func WriteAuthResponse(w io.Writer, username string, digest []byte) error {
	digestB64 := base64.StdEncoding.EncodeToString(digest)
	_, err := io.WriteString(w, fmt.Sprintf("%s %s\n", username, digestB64))
	return err
}

// ReadCompatFlags reads the compat flags varint from r (proto >= 30).
// Returns 0 for proto < 30.
func ReadCompatFlags(r io.Reader, version int) (int, error) {
	if version < 30 {
		return 0, nil
	}
	v, err := ReadVarint(r)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// WriteCompatFlags writes the compat flags varint to w (proto >= 30).
// No-op for proto < 30.
func WriteCompatFlags(w io.Writer, flags int, version int) error {
	if version < 30 {
		return nil
	}
	return WriteVarint(w, int32(flags))
}

// Algorithms holds the negotiated result for both algorithm categories.
type Algorithms struct {
	Checksum string // e.g. "md5"
	Compress string // e.g. "zlib" (empty if compression not negotiated)
}

// DefaultAlgorithms returns the default algorithms for the given protocol
// version without any wire exchange.  Checksum is "md5" (proto >= 30) or "md4"
// (proto < 30); compression is always "zlib".  No data is sent or received.
func DefaultAlgorithms(version int) Algorithms {
	checksum := "md4"
	if version >= 30 {
		checksum = "md5"
	}
	return Algorithms{Checksum: checksum, Compress: "zlib"}
}

// NegotiateAlgorithms performs the full vstring exchange for both checksums
// and compression in a single call.  Both sides send their lists before
// reading the peer's lists to avoid deadlock.  Each side picks its own
// most-preferred algorithm that also appears in the peer's list (not the
// client's first acceptable choice).  When both sides emit their list in
// table (strongest-first) order, they converge on the strongest mutual
// choice; a peer that front-loads a weaker name only desyncs itself.
//
// myChecksums is always required.  myCompressions is only used when
// compression is enabled; pass nil to skip compression negotiation.
func NegotiateAlgorithms(rw io.ReadWriter, myChecksums []string, myCompressions []string) (Algorithms, error) {
	// send our lists first to avoid deadlock
	checksumList := strings.Join(myChecksums, " ")
	if err := WriteVstring(rw, checksumList); err != nil {
		return Algorithms{}, fmt.Errorf("send checksum list: %w", err)
	}

	sentCompression := len(myCompressions) > 0
	if sentCompression {
		compressionList := strings.Join(myCompressions, " ")
		if err := WriteVstring(rw, compressionList); err != nil {
			return Algorithms{}, fmt.Errorf("send compression list: %w", err)
		}
	}

	// read peer's lists
	peerChecksums, err := ReadVstring(rw)
	if err != nil {
		return Algorithms{}, fmt.Errorf("read peer checksum list: %w", err)
	}

	var peerCompressions string
	if sentCompression {
		peerCompressions, err = ReadVstring(rw)
		if err != nil {
			return Algorithms{}, fmt.Errorf("read peer compression list: %w", err)
		}
	}

	// pick our most-preferred algorithm that the peer also supports
	result := Algorithms{
		Checksum: pickOne(myChecksums, peerChecksums),
	}
	if sentCompression {
		result.Compress = pickOne(myCompressions, peerCompressions)
	}
	return result, nil
}

// pickOne picks the first algorithm in myAlgos that also appears in peerList.
// peerList is a space-separated string of algorithm names.
func pickOne(myAlgos []string, peerList string) string {
	peerSet := make(map[string]struct{})
	for _, a := range strings.Fields(peerList) {
		peerSet[a] = struct{}{}
	}
	for _, a := range myAlgos {
		if _, ok := peerSet[a]; ok {
			return a
		}
	}
	return ""
}

// ReadChecksumSeed reads the 4-byte LE checksum seed.
func ReadChecksumSeed(r io.Reader) (int32, error) {
	return ReadInt32(r)
}

// WriteChecksumSeed writes the 4-byte LE checksum seed.
func WriteChecksumSeed(w io.Writer, seed int32) error {
	return WriteInt32(w, seed)
}

// ParseError checks if line is an @ERROR: response.  Returns nil if not an
// error line, or an error with the message text otherwise.
func ParseError(line string) error {
	msg, ok := strings.CutPrefix(line, "@ERROR: ")
	if !ok {
		return nil
	}
	return errors.New(msg)
}

// WriteError writes an @ERROR: line to w.
func WriteError(w io.Writer, msg string) error {
	_, err := io.WriteString(w, fmt.Sprintf("@ERROR: %s\n", msg))
	return err
}

// ExchangeVersion performs the binary version exchange used by SSH/rsh
// transport.  Sends our version, reads remote version, returns the
// negotiated version (lower of the two).
func ExchangeVersion(rw io.ReadWriter, ourVersion int) (int, error) {
	if err := WriteInt32(rw, int32(ourVersion)); err != nil {
		return 0, fmt.Errorf("write version: %w", err)
	}
	theirVersion, err := ReadInt32(rw)
	if err != nil {
		return 0, fmt.Errorf("read version: %w", err)
	}
	if ourVersion < int(theirVersion) {
		return ourVersion, nil
	}
	return int(theirVersion), nil
}
