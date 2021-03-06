package tsm1

import (
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/snappy"
)

const (
	// DefaultSegmentSize of 10MB is the size at which segment files will be rolled over
	DefaultSegmentSize = 10 * 1024 * 1024

	// FileExtension is the file extension we expect for wal segments
	WALFileExtension = "wal"

	WALFilePrefix = "_"

	defaultBufLen = 1024 << 10 // 1MB (sized for batches of 5000 points)

	float64EntryType = 1
	int64EntryType   = 2
	boolEntryType    = 3
	stringEntryType  = 4
)

// SegmentInfo represents metadata about a segment.
type SegmentInfo struct {
	name string
	id   int
}

// walEntry is a byte written to a wal segment file that indicates what the following compressed block contains
type walEntryType byte

const (
	WriteWALEntryType  walEntryType = 0x01
	DeleteWALEntryType walEntryType = 0x02
)

var ErrWALClosed = fmt.Errorf("WAL closed")

type WAL struct {
	mu sync.RWMutex

	path string

	// write variables
	currentSegmentID     int
	currentSegmentWriter *WALSegmentWriter

	// walStats provides summary statistics on wal segment files
	walStats *WALStats

	// cache and flush variables
	closing chan struct{}

	// WALOutput is the writer used by the logger.
	LogOutput io.Writer
	logger    *log.Logger

	// SegmentSize is the file size at which a segment file will be rotated
	SegmentSize int

	// LoggingEnabled specifies if detailed logs should be output
	LoggingEnabled bool
}

type SegmentStat struct {
	Path             string
	ID               int
	MinTime, MaxTime time.Time
	MinKey, MaxKey   string
}

func (s SegmentPaths) IDs() []int {
	var ids []int
	for _, s := range s {
		id, err := idFromFileName(s)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

type SegmentPaths []string

func NewWAL(path string) *WAL {
	return &WAL{
		path: path,

		// these options should be overriden by any options in the config
		LogOutput:   os.Stderr,
		SegmentSize: DefaultSegmentSize,
		logger:      log.New(os.Stderr, "[tsm1devwal] ", log.LstdFlags),
		closing:     make(chan struct{}),
		walStats: &WALStats{
			Dir:          path,
			segmentStats: map[int]*SegmentStat{},
			Ready:        make(chan struct{}),
		},
	}
}

// Path returns the path the log was initialized with.
func (l *WAL) Path() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.path
}

// Open opens and initializes the Log. Will recover from previous unclosed shutdowns
func (l *WAL) Open() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.LoggingEnabled {
		l.logger.Printf("tsm1dev WAL starting with %d segment size\n", l.SegmentSize)
		l.logger.Printf("tsm1dev WAL writing to %s\n", l.path)
	}
	if err := os.MkdirAll(l.path, 0777); err != nil {
		return err
	}

	segments, err := segmentFileNames(l.path)
	if err != nil {
		return err
	}

	if len(segments) > 0 {
		lastSegment := segments[len(segments)-1]
		id, err := idFromFileName(lastSegment)
		if err != nil {
			return err
		}

		l.currentSegmentID = id
		stat, err := os.Stat(lastSegment)
		if err != nil {
			return err
		}

		if stat.Size() == 0 {
			os.Remove(lastSegment)
		}
		if err := l.newSegmentFile(); err != nil {
			return err
		}
	}

	l.closing = make(chan struct{})

	go func() {
		if err := l.walStats.Open(); err != nil {
			l.logger.Printf("error loading WAL stats: %v", err)
		}
	}()

	<-l.walStats.Ready
	return nil
}

// WritePoints writes the given points to the WAL. Returns the WAL segment ID to
// which the points were written. If an error is returned the segment ID should
// be ignored.
func (l *WAL) WritePoints(values map[string][]Value) (int, error) {
	entry := &WriteWALEntry{
		Values: values,
	}

	id, err := l.writeToLog(entry)
	if err != nil {
		return -1, err
	}

	return id, nil
}

func (l *WAL) ClosedSegments() ([]SegmentStat, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	// Not loading files from disk so nothing to do
	if l.path == "" {
		return nil, nil
	}

	var currentPath string
	if l.currentSegmentWriter != nil {
		currentPath = l.currentSegmentWriter.Path()
	}

	return l.walStats.Stats(currentPath)
}

func (l *WAL) writeToLog(entry WALEntry) (int, error) {
	l.mu.RLock()
	// Make sure the log has not been closed
	select {
	case <-l.closing:
		l.mu.RUnlock()
		return -1, ErrWALClosed
	default:
	}

	l.mu.RUnlock()

	if err := l.rollSegment(); err != nil {
		return -1, fmt.Errorf("error rolling WAL segment: %v", err)
	}

	l.mu.RLock()
	// Update segment stats
	if l.currentSegmentWriter != nil {
		l.walStats.Update(l.currentSegmentWriter.Path(), entry)
	}
	defer l.mu.RUnlock()

	if err := l.currentSegmentWriter.Write(entry); err != nil {
		return -1, fmt.Errorf("error writing WAL entry: %v", err)
	}

	return l.currentSegmentID, l.currentSegmentWriter.Sync()
}

// rollSegment closes the current segment and opens a new one if the current segment is over
// the max segment size.
func (l *WAL) rollSegment() error {
	l.mu.RLock()

	if l.currentSegmentWriter == nil || l.currentSegmentWriter.Size() > DefaultSegmentSize {
		l.mu.RUnlock()
		l.mu.Lock()
		defer l.mu.Unlock()

		if err := l.newSegmentFile(); err != nil {
			// A drop database or RP call could trigger this error if writes were in-flight
			// when the drop statement executes.
			return fmt.Errorf("error opening new segment file for wal: %v", err)
		}
		return nil
	}
	l.mu.RUnlock()
	return nil
}

// CloseSegment closes the current segment if it is non-empty and opens a new one.
func (l *WAL) CloseSegment() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.currentSegmentWriter == nil || l.currentSegmentWriter.Size() > 0 {
		if err := l.newSegmentFile(); err != nil {
			// A drop database or RP call could trigger this error if writes were in-flight
			// when the drop statement executes.
			return fmt.Errorf("error opening new segment file for wal: %v", err)
		}
		return nil
	}
	return nil
}

// Delete deletes the given keys, returning the segment ID for the operation.
func (l *WAL) Delete(keys []string) (int, error) {
	entry := &DeleteWALEntry{
		Keys: keys,
	}

	id, err := l.writeToLog(entry)
	if err != nil {
		return -1, err
	}
	return id, nil
}

// Close will finish any flush that is currently in process and close file handles
func (l *WAL) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Close, but don't set to nil so future goroutines can still be signaled
	close(l.closing)

	if l.currentSegmentWriter != nil {
		l.currentSegmentWriter.Close()
	}

	return nil
}

// segmentFileNames will return all files that are WAL segment files in sorted order by ascending ID
func segmentFileNames(dir string) ([]string, error) {
	names, err := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%s*.%s", WALFilePrefix, WALFileExtension)))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// newSegmentFile will close the current segment file and open a new one, updating bookkeeping info on the log
func (l *WAL) newSegmentFile() error {
	l.currentSegmentID++
	if l.currentSegmentWriter != nil {
		if err := l.currentSegmentWriter.Close(); err != nil {
			return err
		}
	}

	fileName := filepath.Join(l.path, fmt.Sprintf("%s%05d.%s", WALFilePrefix, l.currentSegmentID, WALFileExtension))
	fd, err := os.OpenFile(fileName, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	l.currentSegmentWriter = NewWALSegmentWriter(fd)

	return nil
}

type WALStats struct {
	mu sync.RWMutex

	Dir string

	minKey, maxKey   string
	minTime, maxTime time.Time
	segmentStats     map[int]*SegmentStat

	Ready chan struct{}
}

// Open loads summary statistics from each WAL segment.
func (w *WALStats) Open() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	close(w.Ready)

	files, err := segmentFileNames(w.Dir)
	if err != nil {
		return err
	}

	// Channel for loaded stats
	statsCh := make(chan *SegmentStat)

	// Channel for loaded stats
	errorsCh := make(chan error)
	for _, fn := range files {
		// Calculate each segments stats in a separate goroutine
		go func(fn string) {
			id, err := idFromFileName(fn)
			if err != nil {
				errorsCh <- err
				return
			}

			f, err := os.Open(fn)
			if err != nil {
				f.Close()
				errorsCh <- err
				return
			}

			r := NewWALSegmentReader(f)
			defer r.Close()

			var minKey, maxKey string
			var minTime, maxTime time.Time
			for r.Next() {
				entry, err := r.Read()
				if err != nil {
					errorsCh <- err
					return
				}

				// If we have a WriteWALEntry, scan each value to keep track of the key and
				// time range.
				if we, ok := entry.(*WriteWALEntry); ok {
					eMinKey, eMaxKey, eMinTime, eMaxTime := w.ranges(we)
					if minKey == "" || eMinKey < minKey {
						minKey = eMinKey
					}
					if maxKey == "" || eMaxKey > minKey {
						maxKey = eMaxKey
					}
					if minTime.IsZero() || eMinTime.Before(minTime) {
						minTime = eMinTime
					}
					if maxTime.IsZero() || eMaxTime.After(maxTime) {
						maxTime = eMaxTime
					}
				}
			}

			statsCh <- &SegmentStat{
				Path:    fn,
				ID:      id,
				MinTime: minTime,
				MaxTime: maxTime,
				MinKey:  minKey,
				MaxKey:  maxKey,
			}
		}(fn)
	}

	for i := 0; i < len(files); i++ {
		select {
		case err := <-errorsCh:
			return err
		case stat := <-statsCh:
			w.segmentStats[stat.ID] = stat
		}
	}

	return nil
}

func (w *WALStats) Update(path string, entry WALEntry) {
	if we, ok := entry.(*WriteWALEntry); ok {
		minKey, maxKey, minTime, maxTime := w.ranges(we)
		w.updateSegmentStats(path, minKey, maxKey, minTime, maxTime)
	}
}

func (w *WALStats) Stats(currentPath string) ([]SegmentStat, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	files, err := segmentFileNames(w.Dir)
	if err != nil {
		return nil, err
	}

	var stats []SegmentStat
	for _, fn := range files {
		// Skip the current path
		if fn == currentPath {
			continue
		}

		id, err := idFromFileName(fn)
		if err != nil {
			return nil, err
		}

		if stat, ok := w.segmentStats[id]; ok {
			stats = append(stats, *stat)
		}
	}

	return stats, nil
}

func (w *WALStats) ranges(we *WriteWALEntry) (minKey, maxKey string, minTime, maxTime time.Time) {
	for k, v := range we.Values {
		if k < minKey || minKey == "" {
			minKey = k
		}
		if k > maxKey || maxKey == "" {
			maxKey = k
		}

		for _, vv := range v {
			if minTime.IsZero() || vv.Time().Before(minTime) {
				minTime = vv.Time()
			}
			if maxTime.IsZero() || vv.Time().After(maxTime) {
				maxTime = vv.Time()
			}
		}
	}
	return
}

func (w *WALStats) updateSegmentStats(path, minKey, maxKey string, minTime, maxTime time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()

	id, err := idFromFileName(path)
	if err != nil {
		return
	}

	stat := w.segmentStats[id]
	if stat == nil {
		stat = &SegmentStat{
			Path: path,
			ID:   id,
		}
		w.segmentStats[id] = stat
	}

	// Update our current segment stats
	if stat.MinKey == "" || minKey < stat.MinKey {
		stat.MinKey = minKey
	}
	if stat.MaxKey == "" || maxKey > stat.MaxKey {
		stat.MaxKey = maxKey
	}
	if stat.MinTime.IsZero() || minTime.Before(stat.MinTime) {
		stat.MinTime = minTime
	}
	if stat.MaxTime.IsZero() || maxTime.After(stat.MaxTime) {
		stat.MaxTime = maxTime
	}
}

// WALEntry is record stored in each WAL segment.  Each entry has a type
// and an opaque, type dependent byte slice data attribute.
type WALEntry interface {
	Type() walEntryType
	Encode(dst []byte) ([]byte, error)
	MarshalBinary() ([]byte, error)
	UnmarshalBinary(b []byte) error
}

// WriteWALEntry represents a write of points.
type WriteWALEntry struct {
	Values map[string][]Value
}

// Encode converts the WriteWALEntry into a byte stream using dst if it
// is large enough.  If dst is too small, the slice will be grown to fit the
// encoded entry.
func (w *WriteWALEntry) Encode(dst []byte) ([]byte, error) {
	// The entries values are encode as follows:
	//
	// For each key and slice of values, first a 1 byte type for the []Values
	// slice is written.  Following the type, the length and key bytes are written.
	// Following the key, a 4 byte count followed by each value as a 8 byte time
	// and N byte value.  The value is dependent on the type being encoded.  float64,
	// int64, use 8 bytes, bool uses 1 byte, and string is similar to the key encoding.
	//
	// This structure is then repeated for each key an value slices.
	//
	// ┌────────────────────────────────────────────────────────────────────┐
	// │                           WriteWALEntry                            │
	// ├──────┬─────────┬────────┬───────┬─────────┬─────────┬───┬──────┬───┤
	// │ Type │ Key Len │   Key  │ Count │  Time   │  Value  │...│ Type │...│
	// │1 byte│ 4 bytes │ N bytes│4 bytes│ 8 bytes │ N bytes │   │1 byte│   │
	// └──────┴─────────┴────────┴───────┴─────────┴─────────┴───┴──────┴───┘
	var n int

	for k, v := range w.Values {

		switch v[0].Value().(type) {
		case float64:
			dst[n] = float64EntryType
		case int64:
			dst[n] = int64EntryType
		case bool:
			dst[n] = boolEntryType
		case string:
			dst[n] = stringEntryType
		default:
			return nil, fmt.Errorf("unsupported value type: %#v", v[0].Value())
		}
		n++

		// Make sure we have enough space in our buf before copying.  If not,
		// grow the buf.
		if len(k)+2+len(v)*8+4 > len(dst)-n {
			grow := make([]byte, len(dst)*2)
			dst = append(dst, grow...)
		}

		n += copy(dst[n:], u16tob(uint16(len(k))))
		n += copy(dst[n:], []byte(k))

		n += copy(dst[n:], u32tob(uint32(len(v))))

		for _, vv := range v {
			n += copy(dst[n:], u64tob(uint64(vv.Time().UnixNano())))
			switch t := vv.Value().(type) {
			case float64:
				n += copy(dst[n:], u64tob(uint64(math.Float64bits(t))))
			case int64:
				n += copy(dst[n:], u64tob(uint64(t)))
			case bool:
				if t {
					n += copy(dst[n:], []byte{1})
				} else {
					n += copy(dst[n:], []byte{0})
				}
			case string:
				n += copy(dst[n:], u32tob(uint32(len(t))))
				n += copy(dst[n:], []byte(t))
			}
		}
	}

	return dst[:n], nil
}

func (w *WriteWALEntry) MarshalBinary() ([]byte, error) {
	// Temp buffer to write marshaled points into
	b := make([]byte, defaultBufLen)
	return w.Encode(b)
}

func (w *WriteWALEntry) UnmarshalBinary(b []byte) error {
	var i int
	for i < len(b) {
		typ := b[i]
		i++

		length := int(btou16(b[i : i+2]))
		i += 2
		k := string(b[i : i+length])
		i += length

		nvals := int(btou32(b[i : i+4]))
		i += 4

		var values []Value
		switch typ {
		case float64EntryType:
			values = getFloat64Values(nvals)
		case int64EntryType:
			values = getInt64Values(nvals)
		case boolEntryType:
			values = getBoolValues(nvals)
		case stringEntryType:
			values = getStringValues(nvals)
		default:
			return fmt.Errorf("unsupported value type: %#v", typ)
		}

		for j := 0; j < nvals; j++ {
			t := time.Unix(0, int64(btou64(b[i:i+8])))
			i += 8

			switch typ {
			case float64EntryType:
				v := math.Float64frombits((btou64(b[i : i+8])))
				i += 8
				if fv, ok := values[j].(*FloatValue); ok {
					fv.time = t
					fv.value = v
				}
			case int64EntryType:
				v := int64(btou64(b[i : i+8]))
				i += 8
				if fv, ok := values[j].(*Int64Value); ok {
					fv.time = t
					fv.value = v
				}
			case boolEntryType:
				v := b[i]
				i += 1
				if fv, ok := values[j].(*BoolValue); ok {
					fv.time = t
					if v == 1 {
						fv.value = true
					} else {
						fv.value = false
					}
				}
			case stringEntryType:
				length := int(btou32(b[i : i+4]))
				i += 4
				v := string(b[i : i+length])
				i += length
				if fv, ok := values[j].(*StringValue); ok {
					fv.time = t
					fv.value = v
				}
			default:
				return fmt.Errorf("unsupported value type: %#v", typ)
			}
		}
		w.Values[k] = values
	}
	return nil
}

func (w *WriteWALEntry) Type() walEntryType {
	return WriteWALEntryType
}

// DeleteWALEntry represents the deletion of multiple series.
type DeleteWALEntry struct {
	Keys []string
}

func (w *DeleteWALEntry) MarshalBinary() ([]byte, error) {
	b := make([]byte, defaultBufLen)
	return w.Encode(b)
}

func (w *DeleteWALEntry) UnmarshalBinary(b []byte) error {
	w.Keys = strings.Split(string(b), "\n")
	return nil
}

func (w *DeleteWALEntry) Encode(dst []byte) ([]byte, error) {
	var n int
	for _, k := range w.Keys {
		if len(dst)+1 > len(dst)-n {
			grow := make([]byte, defaultBufLen)
			dst = append(dst, grow...)
		}

		n += copy(dst[n:], k)
		n += copy(dst[n:], "\n")
	}

	// We return n-1 to strip off the last newline so that unmarshalling the value
	// does not produce an empty string
	return []byte(dst[:n-1]), nil
}

func (w *DeleteWALEntry) Type() walEntryType {
	return DeleteWALEntryType
}

// WALSegmentWriter writes WAL segments.
type WALSegmentWriter struct {
	mu   sync.RWMutex
	w    io.WriteCloser
	size int
}

func NewWALSegmentWriter(w io.WriteCloser) *WALSegmentWriter {
	return &WALSegmentWriter{
		w: w,
	}
}

func (w *WALSegmentWriter) Path() string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if f, ok := w.w.(*os.File); ok {
		return f.Name()
	}
	return ""
}

func (w *WALSegmentWriter) Write(e WALEntry) error {
	bytes := make([]byte, defaultBufLen)

	b, err := e.Encode(bytes)
	if err != nil {
		return err
	}

	compressed := snappy.Encode(b, b)

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.w.Write([]byte{byte(e.Type())}); err != nil {
		return err
	}

	if _, err = w.w.Write(u32tob(uint32(len(compressed)))); err != nil {
		return err
	}

	if _, err = w.w.Write(compressed); err != nil {
		return err
	}

	// 5 is the 1 byte type + 4 byte uint32 length
	w.size += len(compressed) + 5

	return nil
}

// Sync flushes the file systems in-memory copy of recently written data to disk.
func (w *WALSegmentWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if f, ok := w.w.(*os.File); ok {
		return f.Sync()
	}
	return nil
}

func (w *WALSegmentWriter) Size() int {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.size
}

func (w *WALSegmentWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.w.Close()
}

// WALSegmentReader reads WAL segments.
type WALSegmentReader struct {
	r     io.ReadCloser
	entry WALEntry
	err   error
}

func NewWALSegmentReader(r io.ReadCloser) *WALSegmentReader {
	return &WALSegmentReader{
		r: r,
	}
}

// Next indicates if there is a value to read
func (r *WALSegmentReader) Next() bool {
	b := getBuf(defaultBufLen)
	defer putBuf(b)

	// read the type and the length of the entry
	_, err := io.ReadFull(r.r, b[:5])
	if err == io.EOF {
		return false
	}

	if err != nil {
		r.err = err
		// We return true here because we want the client code to call read which
		// will return the this error to be handled.
		return true
	}

	entryType := b[0]
	length := btou32(b[1:5])

	// read the compressed block and decompress it
	if int(length) > len(b) {
		b = make([]byte, length)
	}

	_, err = io.ReadFull(r.r, b[:length])
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		r.err = err
		return true
	}

	if err != nil {
		r.err = err
		return true
	}

	data, err := snappy.Decode(nil, b[:length])
	if err != nil {
		r.err = err
		return true
	}

	// and marshal it and send it to the cache
	switch walEntryType(entryType) {
	case WriteWALEntryType:
		r.entry = &WriteWALEntry{
			Values: map[string][]Value{},
		}
	case DeleteWALEntryType:
		r.entry = &DeleteWALEntry{}
	default:
		r.err = fmt.Errorf("unknown wal entry type: %v", entryType)
		return true
	}
	r.err = r.entry.UnmarshalBinary(data)

	return true
}

func (r *WALSegmentReader) Read() (WALEntry, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.entry, nil
}

func (r *WALSegmentReader) Error() error {
	return r.err
}

func (r *WALSegmentReader) Close() error {
	return r.r.Close()
}

// idFromFileName parses the segment file ID from its name
func idFromFileName(name string) (int, error) {
	parts := strings.Split(filepath.Base(name), ".")
	if len(parts) != 2 {
		return 0, fmt.Errorf("file %s has wrong name format to have an id", name)
	}

	id, err := strconv.ParseUint(parts[0][1:], 10, 32)

	return int(id), err
}
