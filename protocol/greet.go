package protocol

import (
	"fmt"
	"strconv"
	"strings"
)

// Greeting represents the rsync daemon greeting exchange information.
type Greeting struct {
	Version     int
	SubProtocol byte
	Digests     []string // supported auth digest algorithms in preference order
}

// ParseGreeting parses a raw greeting line from an rsync server or client.
// Format: "@RSYNCD: <version>.<subprotocol> <digests...>\n"
func ParseGreeting(line string) (*Greeting, error) {
	line = strings.TrimSpace(line)

	afterPrefix, ok := strings.CutPrefix(line, "@RSYNCD:")
	if !ok {
		return nil, fmt.Errorf("invalid greeting prefix: %s", line)
	}

	parts := strings.Fields(afterPrefix)
	if len(parts) < 1 {
		return nil, fmt.Errorf("greeting missing version information")
	}

	// Parse version and subprotocol (e.g., "32.0")
	verParts := strings.SplitN(parts[0], ".", 3)
	if len(verParts) != 2 {
		return nil, fmt.Errorf("invalid version format: %s", parts[0])
	}

	version, err := strconv.Atoi(verParts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid protocol version: %w", err)
	}

	subProtoStr := verParts[1]
	if len(subProtoStr) != 1 || subProtoStr[0] < '0' || subProtoStr[0] > '9' {
		return nil, fmt.Errorf("invalid subprotocol format: %s", subProtoStr)
	}
	subProtocol := subProtoStr[0] - '0'

	var digests []string
	if len(parts) > 1 {
		digests = parts[1:]
	}

	return &Greeting{
		Version:     version,
		SubProtocol: subProtocol,
		Digests:     digests,
	}, nil
}

// ApplyDefaults fills in missing fields with sensible defaults:
// version [CurrentProtocolVersion], subprotocol 0, digests ["md5", "md4"].
func (g *Greeting) ApplyDefaults() {
	if g.Version == 0 {
		g.Version = CurrentProtocolVersion
		g.SubProtocol = 0
	}
	if len(g.Digests) == 0 {
		g.Digests = []string{"md5", "md4"}
	}
}

// String formats the greeting back to the wire format.
func (g *Greeting) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "@RSYNCD: %d.%d", g.Version, g.SubProtocol)
	if len(g.Digests) > 0 {
		sb.WriteByte(' ')
		sb.WriteString(strings.Join(g.Digests, " "))
	}
	sb.WriteByte('\n')
	return sb.String()
}

// Negotiate picks the best common version and digest between two greetings.
//
// Digest negotiation follows upstream rsync exactly:
//
//	"Each side sends their list of valid names to the other side and then both sides pick the first name in the client's list that is also in the server's list."
//
// (compat.c:send_negotiate_str)
//
// The caller must pass the client's greeting first and the server's greeting second, regardless of which side is calling.
func Negotiate(client, server Greeting) (version int, subProtocol byte, digest string, err error) {
	// version negotiation matches upstream rsync's exchange_protocols logic
	if client.Version > server.Version {
		version = server.Version
		subProtocol = server.SubProtocol
		if server.SubProtocol != 0 {
			version--
			subProtocol = 0 // downgrade to stable version of the lower protocol
		}
	} else if client.Version == server.Version {
		version = client.Version
		subProtocol = client.SubProtocol
		if client.SubProtocol != server.SubProtocol {
			version--
			subProtocol = 0 // downgrade to stable version of the lower protocol
		}
	} else { // client.Version < server.Version
		// if we are the older version and have a non-zero subprotocol, downgrade by one
		version = client.Version
		subProtocol = client.SubProtocol
		if client.SubProtocol != 0 {
			version--
			subProtocol = 0 // downgrade to stable version of the lower protocol
		}
	}

	if version < MinProtocolVersion {
		return 0, 0, "", fmt.Errorf("negotiated protocol version %d is too low (min %d)", version, MinProtocolVersion)
	}

	// digest negotiation: client preference wins. pick the first algorithm in the client's list that also appears in the server's list
Loop:
	for _, cd := range client.Digests {
		for _, sd := range server.Digests {
			if cd == sd {
				digest = cd
				break Loop
			}
		}
	}

	// fallback if no common digest found
	if digest == "" {
		if version >= 30 {
			digest = "md5"
		} else {
			digest = "md4"
		}
	}

	return version, subProtocol, digest, nil
}
