// Package iceproto implements the parts of the ZeroC Ice protocol (protocol
// 1.0, encoding 1.1) that Murmur's interface needs: the data encoding,
// requests and replies over TCP on the client side, and a minimal object
// adapter so Murmur can call back into Go.
//
// It follows ZeroC's C++ implementation (InputStream.cpp, OutputStream.cpp,
// ConnectionI.cpp in Ice 3.7) rather than a general reading of the spec:
// only what Murmur sends and expects is supported. In particular class
// instances must be marshalled inline in the compact format, which is what
// Ice uses by default.
package iceproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

// Encoding versions.
var (
	Encoding10 = Version{1, 0}
	Encoding11 = Version{1, 1}
)

// Version is a protocol or encoding version.
type Version struct{ Major, Minor byte }

// Identity names an Ice object.
type Identity struct {
	Name     string
	Category string
}

func (id Identity) String() string {
	if id.Category == "" {
		return id.Name
	}
	return id.Category + "/" + id.Name
}

// Encoder writes Ice-encoded data. All values are little-endian.
type Encoder struct {
	buf []byte
}

// Bytes returns the encoded data.
func (e *Encoder) Bytes() []byte { return e.buf }

func (e *Encoder) Byte(v byte) { e.buf = append(e.buf, v) }

func (e *Encoder) Bool(v bool) {
	if v {
		e.buf = append(e.buf, 1)
	} else {
		e.buf = append(e.buf, 0)
	}
}

func (e *Encoder) Short(v int16) { e.buf = binary.LittleEndian.AppendUint16(e.buf, uint16(v)) }
func (e *Encoder) Int(v int32)   { e.buf = binary.LittleEndian.AppendUint32(e.buf, uint32(v)) }
func (e *Encoder) Long(v int64)  { e.buf = binary.LittleEndian.AppendUint64(e.buf, uint64(v)) }

func (e *Encoder) Float(v float32) {
	e.buf = binary.LittleEndian.AppendUint32(e.buf, math.Float32bits(v))
}

func (e *Encoder) Double(v float64) {
	e.buf = binary.LittleEndian.AppendUint64(e.buf, math.Float64bits(v))
}

// Size writes a size: one byte below 255, else 255 and an int.
func (e *Encoder) Size(n int) {
	if n < 255 {
		e.buf = append(e.buf, byte(n))
		return
	}
	e.buf = append(e.buf, 255)
	e.Int(int32(n))
}

// String writes a UTF-8 string. Invalid UTF-8 is replaced, since an Ice peer
// would reject it.
func (e *Encoder) String(s string) {
	if !utf8.ValidString(s) {
		s = string([]rune(s))
	}
	e.Size(len(s))
	e.buf = append(e.buf, s...)
}

func (e *Encoder) Strings(v []string) {
	e.Size(len(v))
	for _, s := range v {
		e.String(s)
	}
}

func (e *Encoder) Ints(v []int32) {
	e.Size(len(v))
	for _, n := range v {
		e.Int(n)
	}
}

// ByteSeq writes a sequence<byte>.
func (e *Encoder) ByteSeq(v []byte) {
	e.Size(len(v))
	e.buf = append(e.buf, v...)
}

// Enum writes an enumerator the encoding 1.1 way, as a size. For enums with
// fewer than 127 enumerators this is also what encoding 1.0 writes.
func (e *Encoder) Enum(v int32) { e.Size(int(v)) }

func (e *Encoder) Identity(id Identity) {
	e.String(id.Name)
	e.String(id.Category)
}

// Context writes a dictionary<string, string>.
func (e *Encoder) Context(ctx map[string]string) {
	e.Size(len(ctx))
	for k, v := range ctx {
		e.String(k)
		e.String(v)
	}
}

// StartEncaps begins an encapsulation in encoding 1.1 and returns the
// position to pass to EndEncaps.
func (e *Encoder) StartEncaps() int {
	pos := len(e.buf)
	e.Int(0) // size, patched by EndEncaps
	e.Byte(Encoding11.Major)
	e.Byte(Encoding11.Minor)
	return pos
}

// EndEncaps patches the size of the encapsulation started at pos.
func (e *Encoder) EndEncaps(pos int) {
	binary.LittleEndian.PutUint32(e.buf[pos:], uint32(len(e.buf)-pos))
}

// Endpoint is a TCP endpoint.
type Endpoint struct {
	Host      string
	Port      int32
	TimeoutMs int32 // -1 for none
	Compress  bool
}

// Proxy is an Ice object reference.
type Proxy struct {
	Identity  Identity
	Facet     string
	Mode      byte // 0 twoway
	Secure    bool
	Endpoints []Endpoint
	AdapterID string
}

const endpointTCP int16 = 1

// Proxy writes a proxy in encoding 1.1, or a null proxy for nil.
func (e *Encoder) Proxy(p *Proxy) {
	if p == nil {
		e.Identity(Identity{})
		return
	}
	e.Identity(p.Identity)
	if p.Facet == "" {
		e.Size(0)
	} else {
		e.Strings([]string{p.Facet})
	}
	e.Byte(p.Mode)
	e.Bool(p.Secure)
	e.Byte(1) // protocol 1.0
	e.Byte(0)
	e.Byte(Encoding11.Major)
	e.Byte(Encoding11.Minor)
	e.Size(len(p.Endpoints))
	if len(p.Endpoints) == 0 {
		e.String(p.AdapterID)
		return
	}
	for _, ep := range p.Endpoints {
		e.Short(endpointTCP)
		pos := e.StartEncaps()
		e.String(ep.Host)
		e.Int(ep.Port)
		e.Int(ep.TimeoutMs)
		e.Bool(ep.Compress)
		e.EndEncaps(pos)
	}
}

// ErrShort is returned when the data ends before the value does.
var ErrShort = errors.New("iceproto: data too short")

// Decoder reads Ice-encoded data. Errors are sticky: after the first one
// every read returns a zero value and Err reports it.
type Decoder struct {
	buf      []byte
	pos      int
	err      error
	encoding Version
	typeIDs  []string // encoding 1.1 type ID table for class slices
}

// NewDecoder reads buf as encoding 1.1 data.
func NewDecoder(buf []byte) *Decoder {
	return &Decoder{buf: buf, encoding: Encoding11}
}

// Err returns the first error the decoder hit.
func (d *Decoder) Err() error { return d.err }

// Encoding returns the encoding the data is in.
func (d *Decoder) Encoding() Version { return d.encoding }

// Remaining returns how many bytes are left.
func (d *Decoder) Remaining() int { return len(d.buf) - d.pos }

func (d *Decoder) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

func (d *Decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || d.pos+n > len(d.buf) {
		d.fail(ErrShort)
		return nil
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b
}

func (d *Decoder) Byte() byte {
	b := d.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (d *Decoder) Bool() bool { return d.Byte() != 0 }

func (d *Decoder) Short() int16 {
	b := d.take(2)
	if b == nil {
		return 0
	}
	return int16(binary.LittleEndian.Uint16(b))
}

func (d *Decoder) Int() int32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return int32(binary.LittleEndian.Uint32(b))
}

func (d *Decoder) Long() int64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(b))
}

func (d *Decoder) Float() float32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}

func (d *Decoder) Double() float64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b))
}

// Size reads a size.
func (d *Decoder) Size() int {
	b := d.Byte()
	if b < 255 {
		return int(b)
	}
	n := d.Int()
	if n < 0 {
		d.fail(fmt.Errorf("iceproto: negative size %d", n))
		return 0
	}
	return int(n)
}

// seqSize reads a sequence size and checks the data can hold that many
// elements of at least minElem bytes each.
func (d *Decoder) seqSize(minElem int) int {
	n := d.Size()
	if d.err == nil && n*minElem > d.Remaining() {
		d.fail(ErrShort)
		return 0
	}
	return n
}

func (d *Decoder) String() string {
	return string(d.take(d.Size()))
}

func (d *Decoder) Strings() []string {
	n := d.seqSize(1)
	v := make([]string, 0, n)
	for i := 0; i < n && d.err == nil; i++ {
		v = append(v, d.String())
	}
	return v
}

func (d *Decoder) Ints() []int32 {
	n := d.seqSize(4)
	v := make([]int32, 0, n)
	for i := 0; i < n && d.err == nil; i++ {
		v = append(v, d.Int())
	}
	return v
}

// ByteSeq reads a sequence<byte>. The result does not alias the input.
func (d *Decoder) ByteSeq() []byte {
	b := d.take(d.Size())
	if b == nil {
		return []byte{}
	}
	return append([]byte(nil), b...)
}

// Enum reads an enumerator of an enum with fewer than 127 enumerators, which
// both encodings write as one byte.
func (d *Decoder) Enum() int32 {
	if d.encoding == Encoding10 {
		return int32(d.Byte())
	}
	return int32(d.Size())
}

func (d *Decoder) Identity() Identity {
	return Identity{Name: d.String(), Category: d.String()}
}

// Context reads a dictionary<string, string>.
func (d *Decoder) Context() map[string]string {
	n := d.seqSize(2)
	m := make(map[string]string, n)
	for i := 0; i < n && d.err == nil; i++ {
		k := d.String()
		m[k] = d.String()
	}
	return m
}

// Encaps reads an encapsulation and returns a decoder for its contents.
func (d *Decoder) Encaps() *Decoder {
	size := d.Int()
	if d.err != nil {
		return &Decoder{err: d.err}
	}
	if size < 6 {
		d.fail(fmt.Errorf("iceproto: encapsulation size %d", size))
		return &Decoder{err: d.err}
	}
	v := Version{d.Byte(), d.Byte()}
	body := d.take(int(size) - 6)
	if d.err != nil {
		return &Decoder{err: d.err}
	}
	if v != Encoding10 && v != Encoding11 {
		return &Decoder{err: fmt.Errorf("iceproto: unsupported encoding %d.%d", v.Major, v.Minor)}
	}
	return &Decoder{buf: body, encoding: v}
}

// Proxy reads a proxy, returning nil for a null proxy. Endpoints other than
// TCP are skipped.
func (d *Decoder) Proxy() *Proxy {
	id := d.Identity()
	if d.err != nil || id.Name == "" {
		return nil
	}
	p := &Proxy{Identity: id}
	if facets := d.Strings(); len(facets) > 0 {
		p.Facet = facets[0]
	}
	p.Mode = d.Byte()
	p.Secure = d.Bool()
	if d.encoding != Encoding10 {
		d.take(4) // protocol and encoding versions
	}
	n := d.seqSize(1)
	if n == 0 {
		p.AdapterID = d.String()
		return p
	}
	for i := 0; i < n && d.err == nil; i++ {
		typ := d.Short()
		enc := d.Encaps()
		if typ != endpointTCP {
			continue
		}
		ep := Endpoint{Host: enc.String(), Port: enc.Int(), TimeoutMs: enc.Int(), Compress: enc.Bool()}
		if enc.err != nil {
			d.fail(enc.err)
		}
		p.Endpoints = append(p.Endpoints, ep)
	}
	return p
}

// Slice flags of encoding 1.1.
const (
	flagTypeIDString     = 1 << 0
	flagTypeIDIndex      = 1 << 1
	flagTypeIDCompact    = 1<<0 | 1<<1
	flagOptionalMembers  = 1 << 2
	flagIndirectionTable = 1 << 3
	flagSliceSize        = 1 << 4
	flagLastSlice        = 1 << 5
)

// ExceptionTypeID reads a user exception and returns the type ID of its
// most derived slice, e.g. "::MumbleServer::InvalidSessionException".
// Murmur's exceptions carry no data, so nothing else is decoded.
func (d *Decoder) ExceptionTypeID() string {
	if d.encoding == Encoding10 {
		d.Bool() // usesClasses
		return d.String()
	}
	d.Byte() // slice flags
	return d.String()
}

// Instance reads a class instance marshalled inline in the compact format.
// It returns false for a null reference. read is called with the decoder
// positioned on the members of the instance's single slice, and typeID is
// the instance's type ID. Instances shared by reference and classes with
// more than one slice are not supported, and Murmur sends neither.
func (d *Decoder) Instance(read func(typeID string)) bool {
	if d.encoding == Encoding10 {
		d.fail(errors.New("iceproto: classes in encoding 1.0 are not supported"))
		return false
	}
	switch idx := d.Size(); {
	case d.err != nil:
		return false
	case idx == 0:
		return false
	case idx != 1:
		d.fail(fmt.Errorf("iceproto: shared class instance (index %d) not supported", idx))
		return false
	}

	flags := d.Byte()
	var typeID string
	switch {
	case flags&flagTypeIDCompact == flagTypeIDCompact:
		d.fail(errors.New("iceproto: compact type IDs not supported"))
		return false
	case flags&flagTypeIDString != 0:
		typeID = d.String()
		d.typeIDs = append(d.typeIDs, typeID)
	case flags&flagTypeIDIndex != 0:
		i := d.Size()
		if i < 1 || i > len(d.typeIDs) {
			d.fail(fmt.Errorf("iceproto: type ID index %d out of range", i))
			return false
		}
		typeID = d.typeIDs[i-1]
	}
	if flags&flagSliceSize != 0 {
		d.Int()
	}
	if flags&flagIndirectionTable != 0 {
		d.fail(errors.New("iceproto: indirection tables not supported"))
		return false
	}
	if flags&flagLastSlice == 0 {
		d.fail(fmt.Errorf("iceproto: %s has more than one slice", typeID))
		return false
	}
	if d.err != nil {
		return false
	}
	read(typeID)
	if flags&flagOptionalMembers != 0 {
		d.skipOptionals()
	}
	return d.err == nil
}

// skipOptionals skips tagged members up to the end marker.
func (d *Decoder) skipOptionals() {
	for d.err == nil {
		v := d.Byte()
		if v == 0xFF {
			return
		}
		format := v & 0x07
		if v>>3 == 30 {
			d.Size() // the tag itself
		}
		switch format {
		case 0:
			d.take(1)
		case 1:
			d.take(2)
		case 2:
			d.take(4)
		case 3:
			d.take(8)
		case 4:
			d.Size()
		case 5:
			d.take(d.Size())
		case 6:
			d.take(int(d.Int()))
		default:
			d.fail(errors.New("iceproto: optional class members not supported"))
		}
	}
}
