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
	Digests     []string // Supported auth digest algorithms in preference order.
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
func Negotiate(local, remote Greeting) (version int, subProtocol byte, digest string, err error) {
	// Version negotiation matches upstream rsync's exchange_protocols logic.
	if local.Version > remote.Version {
		version = remote.Version
		subProtocol = remote.SubProtocol
		if remote.SubProtocol != 0 {
			version--
			subProtocol = 0 // Downgrade to stable version of the lower protocol
		}
	} else if local.Version == remote.Version {
		version = local.Version
		subProtocol = local.SubProtocol
		if local.SubProtocol != remote.SubProtocol {
			version--
			subProtocol = 0 // Downgrade to stable version of the lower protocol
		}
	} else { // local.Version < remote.Version
		// If we are the older version and have a non-zero subprotocol, downgrade by one.
		version = local.Version
		subProtocol = local.SubProtocol
		if local.SubProtocol != 0 {
			version--
			subProtocol = 0 // Downgrade to stable version of the lower protocol
		}
	}

	if version < 20 {
		return 0, 0, "", fmt.Errorf("negotiated protocol version %d is too low (min 20)", version)
	}

	// Digest negotiation: pick the first algorithm that appears in both lists.
Loop:
	for _, ld := range local.Digests {
		for _, rd := range remote.Digests {
			if ld == rd {
				digest = ld
				break Loop
			}
		}
	}

	// Fallback if no common digest found
	if digest == "" {
		if version >= 30 {
			digest = "md5"
		} else {
			digest = "md4"
		}
	}

	return version, subProtocol, digest, nil
}
