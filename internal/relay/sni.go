package relay

import (
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	recordTypeHandshake    = 0x16
	handshakeClientHello   = 0x01
	extensionServerName    = 0x0000
	serverNameTypeHostName = 0x00
	recordHeaderLength     = 5
	maxServerNameLength    = 255
	initialHelloBufferSize = 1024
)

var (
	// errNeedMore means the buffer ends inside a well-formed prefix; the caller
	// must read more bytes before the ClientHello can be parsed.
	errNeedMore = errors.New("relay: need more bytes")

	// ErrNotTLS means the connection did not start with a TLS handshake record.
	ErrNotTLS = errors.New("relay: not a TLS handshake")

	// ErrNoSNI means the ClientHello carries no server_name extension.
	ErrNoSNI = errors.New("relay: client hello without server_name")

	// ErrMalformed means the ClientHello could not be parsed: truncation,
	// overflow, or a length field that runs past its container.
	ErrMalformed = errors.New("relay: malformed client hello")
)

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// ClientHelloSNI returns the SNI host_name of the TLS ClientHello that starts
// at offset 0 of record. The parser is pure, bounded by len(record), and never
// mutates or copies the record: TLS is not terminated by the relay, so the
// caller replays those exact bytes to the origin.
//
// It returns errNeedMore when record holds only a prefix of the ClientHello and
// ErrNotTLS, ErrNoSNI, or ErrMalformed for data that can never parse.
func ClientHelloSNI(record []byte) (string, error) {
	r := &helloReader{buf: record, remaining: -1}

	messageType, err := r.readByte()
	if err != nil {
		return "", err
	}
	if messageType != handshakeClientHello {
		return "", malformed("handshake message type %d is not a client hello", messageType)
	}
	bodyLength, err := r.readUint24()
	if err != nil {
		return "", err
	}
	r.remaining = bodyLength

	// legacy_version (2) + random (32)
	if err := r.skip(2 + 32); err != nil {
		return "", err
	}
	if err := r.skipLengthPrefixed(1); err != nil { // session_id
		return "", err
	}
	if err := r.skipLengthPrefixed(2); err != nil { // cipher_suites
		return "", err
	}
	if err := r.skipLengthPrefixed(1); err != nil { // compression_methods
		return "", err
	}
	if r.remaining == 0 {
		return "", ErrNoSNI
	}

	extensionsLength, err := r.readUint16()
	if err != nil {
		return "", err
	}
	if extensionsLength > r.remaining {
		return "", malformed("extensions length %d exceeds the client hello body", extensionsLength)
	}

	// Walk the extension list. Unknown extensions are skipped by their
	// declared length, so a client may offer anything it likes.
	for consumed := 0; consumed < extensionsLength; {
		if extensionsLength-consumed < 4 {
			return "", malformed("extension header is truncated")
		}
		extensionType, err := r.readUint16()
		if err != nil {
			return "", err
		}
		size, err := r.readUint16()
		if err != nil {
			return "", err
		}
		consumed += 4
		if size > extensionsLength-consumed {
			return "", malformed("extension %#04x length %d exceeds the extension list", extensionType, size)
		}
		if extensionType != extensionServerName {
			if err := r.skip(size); err != nil {
				return "", err
			}
			consumed += size
			continue
		}
		return r.readServerName(size)
	}
	return "", ErrNoSNI
}

// readServerName walks one server_name extension body of exactly length bytes.
func (r *helloReader) readServerName(length int) (string, error) {
	if length < 2 {
		return "", malformed("server_name extension is truncated")
	}
	listLength, err := r.readUint16()
	if err != nil {
		return "", err
	}
	consumed := 2
	if listLength > length-consumed {
		return "", malformed("server_name list length %d exceeds the extension", listLength)
	}

	var name [maxServerNameLength]byte
	for consumed-2 < listLength {
		if listLength-(consumed-2) < 3 {
			return "", malformed("server_name entry header is truncated")
		}
		nameType, err := r.readByte()
		if err != nil {
			return "", err
		}
		nameLength, err := r.readUint16()
		if err != nil {
			return "", err
		}
		consumed += 3
		if nameLength > listLength-(consumed-2) {
			return "", malformed("server_name entry length %d exceeds the list", nameLength)
		}
		if nameType != serverNameTypeHostName {
			if err := r.skip(nameLength); err != nil {
				return "", err
			}
			consumed += nameLength
			continue
		}
		if nameLength == 0 {
			return "", malformed("server_name host_name is empty")
		}
		if nameLength > len(name) {
			return "", malformed("server_name host_name is %d bytes long", nameLength)
		}
		if err := r.readInto(name[:nameLength]); err != nil {
			return "", err
		}
		consumed += nameLength
		// Consume whatever follows the host_name entry so the extension list
		// bookkeeping stays aligned; the result does not depend on it.
		if err := r.skip(length - consumed); err != nil {
			return "", err
		}
		return string(name[:nameLength]), nil
	}
	return "", ErrNoSNI
}

// ReadClientHello reads from conn until the TLS ClientHello can be parsed, then
// returns the raw bytes read and the SNI they advertise. The deadline is set
// from timeout and cleared before returning, so the caller can copy freely
// afterwards. prefix is written verbatim to the origin: no byte is lost,
// buffered, or reordered.
func ReadClientHello(conn net.Conn, maxBytes int, timeout time.Duration) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxClientHelloBytes
	}
	if timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	buffer := make([]byte, 0, min(initialHelloBufferSize, maxBytes))
	for {
		if len(buffer) == cap(buffer) {
			if cap(buffer) >= maxBytes {
				return buffer, "", malformed("client hello exceeds %d bytes", maxBytes)
			}
			grown := make([]byte, len(buffer), min(cap(buffer)*2, maxBytes))
			copy(grown, buffer)
			buffer = grown
		}

		read, err := conn.Read(buffer[len(buffer):cap(buffer)])
		if read > 0 {
			buffer = buffer[:len(buffer)+read]
			sni, parseErr := ClientHelloSNI(buffer)
			if parseErr == nil {
				return buffer, sni, nil
			}
			if !errors.Is(parseErr, errNeedMore) {
				return buffer, "", parseErr
			}
		}
		if err != nil {
			return buffer, "", fmt.Errorf("%w: %w", ErrMalformed, err)
		}
	}
}

// helloReader walks TLS record payloads as one logical byte stream, so a
// ClientHello split across records parses without copying or concatenating.
type helloReader struct {
	buf       []byte // all record bytes seen so far
	payload   []byte // unread bytes of the current record
	next      int    // offset of the next record header in buf
	remaining int    // unread handshake body bytes; -1 while unbounded
}

// fill advances to the next record payload, returning errNeedMore when buf
// holds no complete record yet.
func (r *helloReader) fill() error {
	for len(r.payload) == 0 {
		if r.next+recordHeaderLength > len(r.buf) {
			return errNeedMore
		}
		if contentType := r.buf[r.next]; contentType != recordTypeHandshake {
			return fmt.Errorf("%w: record content type %#02x", ErrNotTLS, contentType)
		}
		length := int(r.buf[r.next+3])<<8 | int(r.buf[r.next+4])
		if length == 0 {
			return malformed("empty handshake record")
		}
		if r.next+recordHeaderLength+length > len(r.buf) {
			return errNeedMore
		}
		r.payload = r.buf[r.next+recordHeaderLength : r.next+recordHeaderLength+length]
		r.next += recordHeaderLength + length
	}
	return nil
}

func (r *helloReader) readByte() (byte, error) {
	if r.remaining == 0 {
		return 0, malformed("client hello body ends inside a field")
	}
	if err := r.fill(); err != nil {
		return 0, err
	}
	value := r.payload[0]
	r.payload = r.payload[1:]
	if r.remaining > 0 {
		r.remaining--
	}
	return value, nil
}

func (r *helloReader) readUint16() (int, error) {
	high, err := r.readByte()
	if err != nil {
		return 0, err
	}
	low, err := r.readByte()
	if err != nil {
		return 0, err
	}
	return int(high)<<8 | int(low), nil
}

func (r *helloReader) readUint24() (int, error) {
	high, err := r.readByte()
	if err != nil {
		return 0, err
	}
	middle, err := r.readByte()
	if err != nil {
		return 0, err
	}
	low, err := r.readByte()
	if err != nil {
		return 0, err
	}
	return int(high)<<16 | int(middle)<<8 | int(low), nil
}

func (r *helloReader) readInto(dst []byte) error {
	for i := range dst {
		value, err := r.readByte()
		if err != nil {
			return err
		}
		dst[i] = value
	}
	return nil
}

// skip discards n bytes, following the handshake body bound when it is set.
func (r *helloReader) skip(n int) error {
	for n > 0 {
		if r.remaining == 0 {
			return malformed("client hello body ends inside a vector")
		}
		if err := r.fill(); err != nil {
			return err
		}
		step := min(n, len(r.payload))
		if r.remaining > 0 {
			step = min(step, r.remaining)
		}
		r.payload = r.payload[step:]
		n -= step
		if r.remaining > 0 {
			r.remaining -= step
		}
	}
	return nil
}

// skipLengthPrefixed skips a length-prefixed vector whose length field is
// size bytes wide, as used by session_id (1), cipher_suites (2), and
// compression_methods (1).
func (r *helloReader) skipLengthPrefixed(size int) error {
	var length int
	switch size {
	case 1:
		value, err := r.readByte()
		if err != nil {
			return err
		}
		length = int(value)
	case 2:
		value, err := r.readUint16()
		if err != nil {
			return err
		}
		length = value
	default:
		return malformed("unsupported vector length width %d", size)
	}
	return r.skip(length)
}
