// Package main: minimal protobuf wire-format + Connect-RPC framing helpers.
//
// We do not have the upstream .proto files, so instead of code generation we
// encode/decode the exact fields reverse-engineered from captured traffic using
// the raw protobuf wire format. This keeps the provider dependency-free and lets
// us match the bytes the real Windsurf/Devin client sends.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
)

// pw is a tiny protobuf wire-format writer.
type pw struct{ b bytes.Buffer }

func (w *pw) tag(field int, wire int) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(field)<<3|uint64(wire))
	w.b.Write(tmp[:n])
}

func (w *pw) uvarint(v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	w.b.Write(tmp[:n])
}

// varint writes a wire-type 0 field.
func (w *pw) varint(field int, v uint64) {
	if v == 0 {
		return // proto3 default omission keeps requests closest to the wire capture
	}
	w.tag(field, 0)
	w.uvarint(v)
}

// varintAlways writes a wire-type 0 field even when zero.
func (w *pw) varintAlways(field int, v uint64) {
	w.tag(field, 0)
	w.uvarint(v)
}

// str writes a wire-type 2 string field (skips empty).
func (w *pw) str(field int, s string) {
	if s == "" {
		return
	}
	w.tag(field, 2)
	w.uvarint(uint64(len(s)))
	w.b.WriteString(s)
}

// bytesField writes a wire-type 2 length-delimited field (skips empty).
func (w *pw) bytesField(field int, data []byte) {
	if len(data) == 0 {
		return
	}
	w.tag(field, 2)
	w.uvarint(uint64(len(data)))
	w.b.Write(data)
}

// msg writes a nested message (skips empty).
func (w *pw) msg(field int, sub []byte) {
	w.bytesField(field, sub)
}

// raw appends already-encoded field bytes (tag+len+value) verbatim.
func (w *pw) raw(b []byte) { w.b.Write(b) }

func (w *pw) bytes() []byte { return w.b.Bytes() }

// ---- reader ----

// pr is a tiny protobuf wire-format reader over a byte slice.
type pr struct {
	b []byte
	i int
}

func newPR(b []byte) *pr { return &pr{b: b} }

func (r *pr) eof() bool { return r.i >= len(r.b) }

// next returns the next field number, wire type, and payload.
// For wire type 2 the payload is the raw sub-slice; for varint it is nil and the
// value is returned via v.
func (r *pr) next() (field int, wire int, sub []byte, v uint64, err error) {
	key, err := r.readUvarint()
	if err != nil {
		return
	}
	field = int(key >> 3)
	wire = int(key & 7)
	switch wire {
	case 0:
		v, err = r.readUvarint()
	case 1:
		if r.i+8 > len(r.b) {
			err = io.ErrUnexpectedEOF
			return
		}
		r.i += 8
	case 5:
		if r.i+4 > len(r.b) {
			err = io.ErrUnexpectedEOF
			return
		}
		r.i += 4
	case 2:
		var ln uint64
		ln, err = r.readUvarint()
		if err != nil {
			return
		}
		if r.i+int(ln) > len(r.b) {
			err = io.ErrUnexpectedEOF
			return
		}
		sub = r.b[r.i : r.i+int(ln)]
		r.i += int(ln)
	default:
		err = fmt.Errorf("codeium proto: unsupported wire type %d", wire)
	}
	return
}

func (r *pr) readUvarint() (uint64, error) {
	v, n := binary.Uvarint(r.b[r.i:])
	if n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	r.i += n
	return v, nil
}

// ---- gzip helpers ----

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

// ---- Connect-RPC enveloping ----
//
// Connect stream frames are: 1 flag byte + 4-byte big-endian length + message.
// flag bit0 (0x01) => message body is compressed with the negotiated codec (gzip).
// flag bit1 (0x02) => end-of-stream trailer frame (JSON).

const (
	connectFlagCompressed = 0x01
	connectFlagEndStream  = 0x02
)

// encodeEnvelope wraps a single message body into one Connect frame.
func encodeEnvelope(body []byte, compressed bool) ([]byte, error) {
	flag := byte(0)
	payload := body
	if compressed {
		gz, err := gzipBytes(body)
		if err != nil {
			return nil, err
		}
		payload = gz
		flag |= connectFlagCompressed
	}
	out := make([]byte, 5+len(payload))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out, nil
}

// envelopeReader incrementally decodes Connect frames from a stream.
type envelopeReader struct {
	r io.Reader
}

func newEnvelopeReader(r io.Reader) *envelopeReader {
	return &envelopeReader{r: r}
}

// frame holds a single decoded Connect frame.
type frame struct {
	end        bool
	compressed bool
	body       []byte
}

// read returns the next frame, decompressing when needed. io.EOF signals the end.
func (e *envelopeReader) read() (*frame, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(e.r, header); err != nil {
		return nil, err
	}
	flag := header[0]
	ln := binary.BigEndian.Uint32(header[1:5])
	body := make([]byte, ln)
	if _, err := io.ReadFull(e.r, body); err != nil {
		return nil, err
	}
	f := &frame{
		end:        flag&connectFlagEndStream != 0,
		compressed: flag&connectFlagCompressed != 0,
		body:       body,
	}
	if f.compressed && len(body) > 0 {
		dec, err := gunzipBytes(body)
		if err != nil {
			return nil, err
		}
		f.body = dec
	}
	return f, nil
}
