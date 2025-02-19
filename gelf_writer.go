// Copyright 2012 SocialCode. All rights reserved.
// Use of this source code is governed by the MIT
// license that can be found in the LICENSE file.

package graylog

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

type GELFWriter interface {
	WriteMessage(m *Message) (err error)
}

// UDPWriter implements io.Writer and is used to send both discrete
// messages to a graylog2 server, or data from a stream-oriented
// interface (like the functions in log).
type UDPWriter struct {
	mu               sync.Mutex
	conn             net.Conn
	hostname         string
	Facility         string // defaults to current process name
	CompressionLevel int    // one of the consts from compress/flate
	CompressionType  CompressType

	zw                 writerCloserResetter
	zwCompressionLevel int
	zwCompressionType  CompressType
}

// What compression type the writer should use when sending messages
// to the graylog2 server
type CompressType int

const (
	CompressGzip CompressType = iota
	CompressZlib
	NoCompress
)

// Message represents the contents of the GELF message.  It is gzipped
// before sending.
type Message struct {
	Version  string                 `json:"version"`
	Host     string                 `json:"host"`
	Short    string                 `json:"short_message"`
	Full     string                 `json:"full_message"`
	TimeUnix float64                `json:"timestamp"`
	Level    int32                  `json:"level"`
	Facility string                 `json:"facility"`
	File     string                 `json:"file"`
	Line     int                    `json:"line"`
	Extra    map[string]interface{} `json:"-"`
}

type innerMessage Message //against circular (Un)MarshalJSON

// Used to control GELF chunking.  Should be less than (MTU - len(UDP
// header)).
//
// TODO: generate dynamically using Path MTU Discovery?
const (
	ChunkSize        = 1420
	chunkedHeaderLen = 12
	chunkedDataLen   = ChunkSize - chunkedHeaderLen
)

var (
	magicChunked = []byte{0x1e, 0x0f}
	magicZlib    = []byte{0x78}
	magicGzip    = []byte{0x1f, 0x8b}
)

// numChunks returns the number of GELF chunks necessary to transmit
// the given compressed buffer.
func numChunks(b []byte) int {
	lenB := len(b)
	if lenB <= ChunkSize {
		return 1
	}
	return len(b)/chunkedDataLen + 1
}

// NewWriter returns a new GELFWriter. This writer can be used to send the
// output of the standard Go log functions to a central GELF server by
// passing it to log.SetOutput()
func NewWriter(addr string) (GELFWriter, error) {
	if strings.HasPrefix(addr, "http") {
		return newHTTPWriter(addr)
	}

	return newUDPWriter(addr)
}

func newHTTPWriter(addr string) (GELFWriter, error) {
	httpClient := &http.Client{
		Transport: &http.Transport{},
		Timeout:   10 * time.Second,
	}

	return HTTPWriter{
		httpClient: httpClient,
		addr:       addr,
	}, nil
}

func newUDPWriter(addr string) (GELFWriter, error) {
	var err error
	w := new(UDPWriter)
	w.CompressionLevel = flate.BestSpeed

	if w.conn, err = net.Dial("udp", addr); err != nil {
		return nil, err
	}
	if w.hostname, err = os.Hostname(); err != nil {
		return nil, err
	}

	w.Facility = path.Base(os.Args[0])

	return w, nil
}

// writes the gzip compressed byte array to the connection as a series
// of GELF chunked messages.  The header format is documented at
// https://github.com/Graylog2/graylog2-docs/wiki/GELF as:
//
//	2-byte magic (0x1e 0x0f), 8 byte id, 1 byte sequence id, 1 byte
//	total, chunk-data
func (w *UDPWriter) writeChunked(zBytes []byte) (err error) {
	b := make([]byte, 0, ChunkSize)
	buf := bytes.NewBuffer(b)
	nChunksI := numChunks(zBytes)
	if nChunksI > 255 {
		return fmt.Errorf("msg too large, would need %d chunks", nChunksI)
	}
	nChunks := uint8(nChunksI)
	// use urandom to get a unique message id
	msgId := make([]byte, 8)
	n, err := io.ReadFull(rand.Reader, msgId)
	if err != nil || n != 8 {
		return fmt.Errorf("rand.Reader: %d/%s", n, err)
	}

	bytesLeft := len(zBytes)
	for i := uint8(0); i < nChunks; i++ {
		buf.Reset()
		// manually write header.  Don't care about
		// host/network byte order, because the spec only
		// deals in individual bytes.
		buf.Write(magicChunked) //magic
		buf.Write(msgId)
		buf.WriteByte(i)
		buf.WriteByte(nChunks)
		// slice out our chunk from zBytes
		chunkLen := chunkedDataLen
		if chunkLen > bytesLeft {
			chunkLen = bytesLeft
		}
		off := int(i) * chunkedDataLen
		chunk := zBytes[off : off+chunkLen]
		buf.Write(chunk)

		// write this chunk, and make sure the write was good
		n, err := w.conn.Write(buf.Bytes())
		if err != nil {
			return fmt.Errorf("Write (chunk %d/%d): %s", i,
				nChunks, err)
		}
		if n != len(buf.Bytes()) {
			return fmt.Errorf("Write len: (chunk %d/%d) (%d/%d)",
				i, nChunks, n, len(buf.Bytes()))
		}

		bytesLeft -= chunkLen
	}

	if bytesLeft != 0 {
		return fmt.Errorf("error: %d bytes left after sending", bytesLeft)
	}
	return nil
}

type bufferedWriter struct {
	buffer io.Writer
}

func (bw bufferedWriter) Write(p []byte) (n int, err error) {
	return bw.buffer.Write(p)
}

func (bw bufferedWriter) Close() error {
	return nil
}

func (bw *bufferedWriter) Reset(w io.Writer) {
	bw.buffer = w
}

type writerCloserResetter interface {
	io.WriteCloser
	Reset(w io.Writer)
}

// WriteMessage sends the specified message to the GELF server
// specified in the call to NewWriter(). It assumes all the fields are
// filled out appropriately. In general, clients will want to use
// Write, rather than WriteMessage.
func (w *UDPWriter) WriteMessage(m *Message) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	mBytes, err := json.Marshal(m)
	if err != nil {
		return
	}

	var zBuf bytes.Buffer

	// . If compression settings have changed, a new writer is required.
	if w.zwCompressionType != w.CompressionType || w.zwCompressionLevel != w.CompressionLevel {
		w.zw = nil
	}

	switch w.CompressionType {
	case CompressGzip:
		if w.zw == nil {
			w.zw, err = gzip.NewWriterLevel(&zBuf, w.CompressionLevel)
		}
	case CompressZlib:
		if w.zw == nil {
			w.zw, err = zlib.NewWriterLevel(&zBuf, w.CompressionLevel)
		}
	case NoCompress:
		w.zw = &bufferedWriter{}
	default:
		panic(fmt.Sprintf("unknown compression type %d",
			w.CompressionType))
	}

	if err != nil {
		return
	}

	w.zw.Reset(&zBuf)

	if _, err = w.zw.Write(mBytes); err != nil {
		return
	}
	w.zw.Close()

	zBytes := zBuf.Bytes()
	if numChunks(zBytes) > 1 {
		return w.writeChunked(zBytes)
	}

	n, err := w.conn.Write(zBytes)
	if err != nil {
		return
	}
	if n != len(zBytes) {
		return fmt.Errorf("bad write (%d/%d)", n, len(zBytes))
	}

	return nil
}

/*
func (w *Writer) Alert(m string) (err error)
func (w *Writer) Close() error
func (w *Writer) Crit(m string) (err error)
func (w *Writer) Debug(m string) (err error)
func (w *Writer) Emerg(m string) (err error)
func (w *Writer) Err(m string) (err error)
func (w *Writer) Info(m string) (err error)
func (w *Writer) Notice(m string) (err error)
func (w *Writer) Warning(m string) (err error)
*/

// Write encodes the given string in a GELF message and sends it to
// the server specified in NewWriter().
func (w *UDPWriter) Write(p []byte) (n int, err error) {

	// remove trailing and leading whitespace
	p = bytes.TrimSpace(p)

	// If there are newlines in the message, use the first line
	// for the short message and set the full message to the
	// original input.  If the input has no newlines, stick the
	// whole thing in Short.
	short := p
	full := []byte("")
	if i := bytes.IndexRune(p, '\n'); i > 0 {
		short = p[:i]
		full = p
	}

	m := Message{
		Version:  "1.0",
		Host:     w.hostname,
		Short:    string(short),
		Full:     string(full),
		TimeUnix: float64(time.Now().UnixNano()/1000000) / 1000.,
		Level:    6, // info
		Facility: w.Facility,
		Extra:    map[string]interface{}{},
	}

	if err = w.WriteMessage(&m); err != nil {
		return 0, err
	}

	return len(p), nil
}

func (m *Message) MarshalJSON() ([]byte, error) {
	var err error
	var b, eb []byte

	extra := m.Extra
	b, err = json.Marshal((*innerMessage)(m))
	m.Extra = extra
	if err != nil {
		return nil, err
	}

	if len(extra) == 0 {
		return b, nil
	}

	if eb, err = json.Marshal(extra); err != nil {
		return nil, err
	}

	// merge serialized message + serialized extra map
	b[len(b)-1] = ','
	return append(b, eb[1:len(eb)]...), nil
}

func (m *Message) UnmarshalJSON(data []byte) error {
	i := make(map[string]interface{}, 16)
	if err := json.Unmarshal(data, &i); err != nil {
		return err
	}
	for k, v := range i {
		if k[0] == '_' {
			if m.Extra == nil {
				m.Extra = make(map[string]interface{}, 1)
			}
			m.Extra[k] = v
			continue
		}
		switch k {
		case "version":
			m.Version = v.(string)
		case "host":
			m.Host = v.(string)
		case "short_message":
			m.Short = v.(string)
		case "full_message":
			m.Full = v.(string)
		case "timestamp":
			m.TimeUnix = v.(float64)
		case "level":
			m.Level = int32(v.(float64))
		case "facility":
			m.Facility = v.(string)
		case "file":
			m.File = v.(string)
		case "line":
			m.Line = int(v.(float64))
		}
	}
	return nil
}

// HTTPWriter implements the GELFWriter interface, and cannot be used
// as an io.Writer
type HTTPWriter struct {
	httpClient *http.Client
	addr       string
}

func (h HTTPWriter) WriteMessage(m *Message) (err error) {
	mBytes, err := json.Marshal(m)
	if err != nil {
		return
	}

	resp, err := h.httpClient.Post(h.addr, "application/json", bytes.NewBuffer(mBytes))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 202 {
		return fmt.Errorf("got code %s, expected 202", resp.Status)
	}

	return nil
}
