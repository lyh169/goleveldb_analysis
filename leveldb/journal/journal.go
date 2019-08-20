// Copyright 2011 The LevelDB-Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Taken from: https://code.google.com/p/leveldb-go/source/browse/leveldb/record/record.go?r=1d5ccbe03246da926391ee12d1c6caae054ff4b0
// License, authors and contributors informations can be found at bellow URLs respectively:
// 	https://code.google.com/p/leveldb-go/source/browse/LICENSE
//	https://code.google.com/p/leveldb-go/source/browse/AUTHORS
//  https://code.google.com/p/leveldb-go/source/browse/CONTRIBUTORS

// Package journal reads and writes sequences of journals. Each journal is a stream
// of bytes that completes before the next journal starts.
//
// When reading, call Next to obtain an io.Reader for the next journal. Next will
// return io.EOF when there are no more journals. It is valid to call Next
// without reading the current journal to exhaustion.
//
// When writing, call Next to obtain an io.Writer for the next journal. Calling
// Next finishes the current journal. Call Close to finish the final journal.
//
// Optionally, call Flush to finish the current journal and flush the underlying
// writer without starting a new journal. To start a new journal after flushing,
// call Next.
//
// Neither Readers or Writers are safe to use concurrently.
//
// Example code:
//	func read(r io.Reader) ([]string, error) {
//		var ss []string
//		journals := journal.NewReader(r, nil, true, true)
//		for {
//			j, err := journals.Next()
//			if err == io.EOF {
//				break
//			}
//			if err != nil {
//				return nil, err
//			}
//			s, err := ioutil.ReadAll(j)
//			if err != nil {
//				return nil, err
//			}
//			ss = append(ss, string(s))
//		}
//		return ss, nil
//	}
//
//	func write(w io.Writer, ss []string) error {
//		journals := journal.NewWriter(w)
//		for _, s := range ss {
//			j, err := journals.Next()
//			if err != nil {
//				return err
//			}
//			if _, err := j.Write([]byte(s)), err != nil {
//				return err
//			}
//		}
//		return journals.Close()
//	}
//
// The wire format is that the stream is divided into 32KiB blocks, and each
// block contains a number of tightly packed chunks. Chunks cannot cross block
// boundaries. The last block may be shorter than 32 KiB. Any unused bytes in a
// block must be zero.
//
// A journal maps to one or more chunks. Each chunk has a 7 byte header (a 4
// byte checksum, a 2 byte little-endian uint16 length, and a 1 byte chunk type)
// followed by a payload. The checksum is over the chunk type and the payload.
//
// There are four chunk types: whether the chunk is the full journal, or the
// first, middle or last chunk of a multi-chunk journal. A multi-chunk journal
// has one first chunk, zero or more middle chunks, and one last chunk.
//
// The wire format allows for limited recovery in the face of data corruption:
// on a format error (such as a checksum mismatch), the reader moves to the
// next block and looks for the next full or first chunk.
package journal

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// These constants are part of the wire format and should not be changed.
const (
	fullChunkType   = 1
	firstChunkType  = 2
	middleChunkType = 3
	lastChunkType   = 4
)

const (
	blockSize  = 32 * 1024  // 一个block固定32KB
	headerSize = 7  // header大小固定为7个字节
)

type flusher interface {
	Flush() error
}

// ErrCorrupted is the error type that generated by corrupted block or chunk.
type ErrCorrupted struct {
	Size   int
	Reason string
}

func (e *ErrCorrupted) Error() string {
	return fmt.Sprintf("leveldb/journal: block/chunk corrupted: %s (%d bytes)", e.Reason, e.Size)
}

// Dropper is the interface that wrap simple Drop method. The Drop
// method will be called when the journal reader dropping a block or chunk.
type Dropper interface {
	Drop(err error)
}

// Reader reads journals from an underlying io.Reader.
type Reader struct {
	// r is the underlying reader.
	r io.Reader  // 底层reader
	// the dropper.
	dropper Dropper
	// strict flag.
	strict bool
	// checksum flag.
	checksum bool
	// seq is the sequence number of the current journal.
	seq int
	// buf[i:j] is the unread portion of the current chunk's payload.
	// The low bound, i, excludes the chunk header.
	i, j int
	// n is the number of bytes of buf that are valid. Once reading has started,
	// only the final block can have n < blockSize.
	n int
	// last is whether the current chunk is the last chunk of the journal.
	last bool  // 是否为最后一个block
	// err is any accumulated error.
	err error
	// buf is the buffer.
	buf [blockSize]byte  // 一个block的内容
}

// NewReader returns a new reader. The dropper may be nil, and if
// strict is true then corrupted or invalid chunk will halt the journal
// reader entirely.
func NewReader(r io.Reader, dropper Dropper, strict, checksum bool) *Reader {
	return &Reader{
		r:        r,
		dropper:  dropper,
		strict:   strict,
		checksum: checksum,
		last:     true,
	}
}

var errSkip = errors.New("leveldb/journal: skipped")

func (r *Reader) corrupt(n int, reason string, skip bool) error {
	if r.dropper != nil {
		r.dropper.Drop(&ErrCorrupted{n, reason})  // 抛出异常
	}
	if r.strict && !skip {
		r.err = errors.NewErrCorrupted(storage.FileDesc{}, &ErrCorrupted{n, reason})
		return r.err
	}
	return errSkip
}

// nextChunk sets r.buf[r.i:r.j] to hold the next chunk's payload, reading the
// next block into the buffer if necessary.

// checksum|length|chunkType|unprocBlock
func (r *Reader) nextChunk(first bool) error {

	// 这里的读取一个block的数据，然后再批量处理切成多个chunk的做法，值得借鉴
	// 尤其是rpc分包的时候，可以考虑使用这种逻辑。
	for {
		// 从block里面解出多个chunk
		if r.j+headerSize <= r.n {  // 可读范围有效
			checksum := binary.LittleEndian.Uint32(r.buf[r.j+0 : r.j+4])  // 读出checksum
			length := binary.LittleEndian.Uint16(r.buf[r.j+4 : r.j+6])  // 读出length
			chunkType := r.buf[r.j+6]
			unprocBlock := r.n - r.j
			if checksum == 0 && length == 0 && chunkType == 0 {
				// Drop entire block.
				r.i = r.n
				r.j = r.n  // 无效的block，整个block drop掉
				return r.corrupt(unprocBlock, "zero header", false)
			}
			if chunkType < fullChunkType || chunkType > lastChunkType {
				// Drop entire block.
				r.i = r.n
				r.j = r.n  // 无效的chunkType
				return r.corrupt(unprocBlock, fmt.Sprintf("invalid chunk type %#x", chunkType), false)
			}
			// 真正的数据开始
			r.i = r.j + headerSize
			r.j = r.j + headerSize + int(length)
			if r.j > r.n {
				// Drop entire block.
				r.i = r.n
				r.j = r.n
				return r.corrupt(unprocBlock, "chunk length overflows block", false)
			} else if r.checksum && checksum != util.NewCRC(r.buf[r.i-1:r.j]).Value() {
				// Drop entire block.
				r.i = r.n
				r.j = r.n
				return r.corrupt(unprocBlock, "checksum mismatch", false)
			}
			if first && chunkType != fullChunkType && chunkType != firstChunkType {
				chunkLength := (r.j - r.i) + headerSize
				r.i = r.j
				// Report the error, but skip it.
				return r.corrupt(chunkLength, "orphan chunk", true)
			}
			r.last = chunkType == fullChunkType || chunkType == lastChunkType
			return nil
		}

		// The last block.
		if r.n < blockSize && r.n > 0 {
			if !first {
				return r.corrupt(0, "missing chunk part", false)
			}
			r.err = io.EOF
			return r.err
		}

		// Read block.
		n, err := io.ReadFull(r.r, r.buf[:])  // 每次读一个block
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		if n == 0 {
			if !first {
				return r.corrupt(0, "missing chunk part", false)
			}
			r.err = io.EOF
			return r.err
		}
		r.i, r.j, r.n = 0, 0, n
	}
}

// Next returns a reader for the next journal. It returns io.EOF if there are no
// more journals. The reader returned becomes stale after the next Next call,
// and should no longer be used. If strict is false, the reader will returns
// io.ErrUnexpectedEOF error when found corrupted journal.
func (r *Reader) Next() (io.Reader, error) {
	r.seq++
	if r.err != nil {
		return nil, r.err
	}
	r.i = r.j
	for {
		// 找到第一个chunk
		if err := r.nextChunk(true); err == nil {
			break
		} else if err != errSkip {
			return nil, err
		}
	}
	return &singleReader{r, r.seq, nil}, nil
}

// Reset resets the journal reader, allows reuse of the journal reader. Reset returns
// last accumulated error.
func (r *Reader) Reset(reader io.Reader, dropper Dropper, strict, checksum bool) error {
	r.seq++
	err := r.err
	r.r = reader
	r.dropper = dropper
	r.strict = strict
	r.checksum = checksum
	r.i = 0
	r.j = 0
	r.n = 0
	r.last = true
	r.err = nil
	return err
}

type singleReader struct {
	r   *Reader
	seq int
	err error
}

func (x *singleReader) Read(p []byte) (int, error) {
	r := x.r
	if r.seq != x.seq {
		return 0, errors.New("leveldb/journal: stale reader")
	}
	if x.err != nil {
		return 0, x.err
	}
	if r.err != nil {
		return 0, r.err
	}
	for r.i == r.j {
		if r.last {
			return 0, io.EOF
		}
		x.err = r.nextChunk(false)
		if x.err != nil {
			if x.err == errSkip {
				x.err = io.ErrUnexpectedEOF
			}
			return 0, x.err
		}
	}
	// 读出一个chunk，然后拷贝到p里面
	n := copy(p, r.buf[r.i:r.j])
	r.i += n
	return n, nil
}

func (x *singleReader) ReadByte() (byte, error) {
	r := x.r
	if r.seq != x.seq {
		return 0, errors.New("leveldb/journal: stale reader")
	}
	if x.err != nil {
		return 0, x.err
	}
	if r.err != nil {
		return 0, r.err
	}
	for r.i == r.j {
		if r.last {
			return 0, io.EOF
		}
		x.err = r.nextChunk(false)
		if x.err != nil {
			if x.err == errSkip {
				x.err = io.ErrUnexpectedEOF
			}
			return 0, x.err
		}
	}
	c := r.buf[r.i]  // 只读出一个字节
	r.i++
	return c, nil
}

// Writer writes journals to an underlying io.Writer.
type Writer struct {
	// w is the underlying writer.
	w io.Writer
	// seq is the sequence number of the current journal.
	// seq：序列号，每次操作都会累加1
	seq int
	// f is w as a flusher.
	f flusher
	// buf[i:j] is the bytes that will become the current chunk.
	// The low bound, i, includes the chunk header.
	i, j int
	// buf[:written] has already been written to w.
	// written is zero unless Flush has been called.
	written int
	// first is whether the current chunk is the first chunk of the journal.
	first bool
	// pending is whether a chunk is buffered but not yet written.
	pending bool
	// err is any accumulated error.
	err error
	// buf is the buffer.
	buf [blockSize]byte
}

// NewWriter returns a new Writer.
func NewWriter(w io.Writer) *Writer {
	f, _ := w.(flusher)
	return &Writer{
		w: w,
		f: f,
	}
}

// fillHeader fills in the header for the pending chunk.
func (w *Writer) fillHeader(last bool) {
	if w.i+headerSize > w.j || w.j > blockSize {
		panic("leveldb/journal: bad writer state")
	}
	if last {
		if w.first {
			w.buf[w.i+6] = fullChunkType
		} else {
			w.buf[w.i+6] = lastChunkType  // 最后一个包
		}
	} else {
		if w.first {
			w.buf[w.i+6] = firstChunkType
		} else {
			w.buf[w.i+6] = middleChunkType
		}
	}
	binary.LittleEndian.PutUint32(w.buf[w.i+0:w.i+4], util.NewCRC(w.buf[w.i+6:w.j]).Value())
	binary.LittleEndian.PutUint16(w.buf[w.i+4:w.i+6], uint16(w.j-w.i-headerSize))
}

// writeBlock writes the buffered block to the underlying writer, and reserves
// space for the next chunk's header.
func (w *Writer) writeBlock() {
	_, w.err = w.w.Write(w.buf[w.written:])  // w.buf[w.written]从内存里面写入到w.w.Write
	w.i = 0
	w.j = headerSize
	w.written = 0
}

// writePending finishes the current journal and writes the buffer to the
// underlying writer.
func (w *Writer) writePending() {
	if w.err != nil {
		return
	}
	if w.pending {
		// 把最后的header写入进去
		w.fillHeader(true)
		w.pending = false
	}
	_, w.err = w.w.Write(w.buf[w.written:w.j])
	w.written = w.j
}

// Close finishes the current journal and closes the writer.
func (w *Writer) Close() error {
	w.seq++
	w.writePending()
	if w.err != nil {
		return w.err
	}
	w.err = errors.New("leveldb/journal: closed Writer")
	return nil
}

// Flush finishes the current journal, writes to the underlying writer, and
// flushes it if that writer implements interface{ Flush() error }.
// 把当前日志刷盘
func (w *Writer) Flush() error {
	w.seq++
	w.writePending()
	if w.err != nil {
		return w.err
	}
	if w.f != nil {
		w.err = w.f.Flush()
		return w.err
	}
	return nil
}

// Reset resets the journal writer, allows reuse of the journal writer. Reset
// will also closes the journal writer if not already.
func (w *Writer) Reset(writer io.Writer) (err error) {
	w.seq++
	if w.err == nil {
		w.writePending()
		err = w.err
	}
	w.w = writer
	w.f, _ = writer.(flusher)
	w.i = 0
	w.j = 0
	w.written = 0
	w.first = false
	w.pending = false
	w.err = nil
	return
}

// Next returns a writer for the next journal. The writer returned becomes stale
// after the next Close, Flush or Next call, and should no longer be used.
// 调用Next()方法获取一个singleWriter，这个singleWriter的作用就是写入一条journal记录
func (w *Writer) Next() (io.Writer, error) {
	w.seq++  // 序列号++
	if w.err != nil {
		return nil, w.err
	}
	if w.pending {
		w.fillHeader(true)
	}
	w.i = w.j
	w.j = w.j + headerSize
	// Check if there is room in the block for the header.
	// 判断当前block是否有足够的空间写入一个header
	if w.j > blockSize {
		// Fill in the rest of the block with zeroes.
		// 不够空间，直接将当前block的剩余空间置0
		for k := w.i; k < blockSize; k++ {
			w.buf[k] = 0
		}
		// 将已写入buf的信息写入文件
		w.writeBlock()
		if w.err != nil {
			return nil, w.err
		}
	}
	w.first = true  // 是否开始写第一个trunk
	w.pending = true  // 是否就绪
	return singleWriter{w, w.seq}, nil
}

type singleWriter struct {
	w   *Writer
	seq int
}

func (x singleWriter) Write(p []byte) (int, error) {
	w := x.w
	// 核对两者要写的日志序号是否一致
	if w.seq != x.seq {
		return 0, errors.New("leveldb/journal: stale writer")
	}
	if w.err != nil {
		return 0, w.err
	}
	n0 := len(p)
	for len(p) > 0 {
		// Write a block, if it is full.  // 刚好写满一个block
		if w.j == blockSize {
			// 预留了[w.i, w.i+6]之间的空间写入header
			w.fillHeader(false)  // 还没写完
			w.writeBlock()
			if w.err != nil {
				return 0, w.err
			}
			w.first = false
		}
		// Copy bytes into the buffer.
		n := copy(w.buf[w.j:], p)  // w.buf[blockSize]这里固定了32KB,直接将p拷贝进去
		w.j += n
		p = p[n:]
	}
	return n0, nil
}
