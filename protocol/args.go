package protocol

import (
	"io"
	"strings"
)

// ReadArgs reads null-terminated (proto >= 30) or newline-terminated (proto < 30)
// rsync command-line arguments.  Terminated by double delimiter.
func ReadArgs(r io.Reader, version int) ([]string, error) {
	delim := byte('\n')
	if version >= 30 {
		delim = 0
	}
	return readDelimitedArgs(r, delim)
}

// readDelimitedArgs reads arguments separated by delim, terminated by double delim.
func readDelimitedArgs(r io.Reader, delim byte) ([]string, error) {
	var args []string
	var current strings.Builder

	for {
		b, err := readOne(r)
		if err != nil {
			return nil, err
		}
		if b == delim {
			if current.Len() == 0 {
				// empty string before delimiter = second delimiter = termination
				break
			}
			args = append(args, current.String())
			current.Reset()
		} else {
			current.WriteByte(b)
		}
	}

	return args, nil
}

func readOne(r io.Reader) (byte, error) {
	var buf [1]byte
	_, err := io.ReadFull(r, buf[:])
	if err != nil {
		return 0, err
	}
	return buf[0], nil
}

// WriteArgs writes arguments in the appropriate format for the protocol version.
func WriteArgs(w io.Writer, args []string, version int) error {
	delim := byte('\n')
	if version >= 30 {
		delim = 0
	}

	for _, arg := range args {
		if _, err := io.WriteString(w, arg); err != nil {
			return err
		}
		if err := writeOne(w, delim); err != nil {
			return err
		}
	}

	// double delimiter terminates the list
	// when args is empty, write both delimiters; otherwise the loop
	// already wrote the first delimiter after the last arg
	if len(args) == 0 {
		if err := writeOne(w, delim); err != nil {
			return err
		}
	}
	return writeOne(w, delim)
}

func writeOne(w io.Writer, b byte) error {
	_, err := w.Write([]byte{b})
	return err
}

// ExtractClientInfo extracts the client_info feature flags from the -e argument
// in the argument list.  The 'e' flag is embedded in combined short options
// (e.g., "-vlogDtpr.eiLsfxCIvu"), and everything after the 'e' is the
// client_info string.  Only short-option arguments (starting with "-" but not
// "--") are searched, matching the upstream behavior where client_info is
// carried in the combined flags argument.
// Returns "" if no -e argument is found.
func ExtractClientInfo(args []string) string {
	for _, arg := range args {
		// only search short-option arguments, not long options like --server
		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			continue
		}
		idx := strings.IndexByte(arg, 'e')
		if idx >= 0 && len(arg) > idx+1 {
			return arg[idx+1:]
		}
	}
	return ""
}

// ResolveCompatFlags builds the server's compat flags based on its capabilities
// (serverCaps) and the client's advertised feature flags (clientInfo).  Only
// flags that both sides support are set in the result.
func ResolveCompatFlags(serverCaps int, clientInfo string) int {
	flags := 0

	for _, ch := range clientInfo {
		var flag int
		switch ch {
		case 'i':
			flag = CompatIncRecurse
		case 'L':
			flag = CompatSymlinkTimes
		case 's':
			flag = CompatSymlinkIconv
		case 'f':
			flag = CompatSafeFlist
		case 'x':
			flag = CompatAvoidXattrOptim
		case 'C':
			flag = CompatChksumSeedFix
		case 'I':
			flag = CompatInplacePartialDir
		case 'v':
			flag = CompatVarintFlistFlags
		case 'u':
			flag = CompatId0Names
		default:
			continue
		}
		if serverCaps&flag != 0 {
			flags |= flag
		}
	}

	return flags
}
