package tsm1

import (
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/influxdb/influxdb/influxql"
	"github.com/influxdb/influxdb/models"
	"github.com/influxdb/influxdb/tsdb"
)

func init() {
	tsdb.RegisterEngine("tsm1dev", NewDevEngine)
}

// Ensure Engine implements the interface.
var _ tsdb.Engine = &DevEngine{}

// Engine represents a storage engine with compressed blocks.
type DevEngine struct {
	mu sync.RWMutex

	path   string
	logger *log.Logger

	WAL            *WAL
	Cache          *Cache
	Compactor      *Compactor
	CompactionPlan CompactionPlanner
	FileStore      *FileStore

	RotateFileSize    uint32
	MaxFileSize       uint32
	MaxPointsPerBlock int
}

// NewDevEngine returns a new instance of Engine.
func NewDevEngine(path string, walPath string, opt tsdb.EngineOptions) tsdb.Engine {
	w := NewWAL(walPath)
	w.LoggingEnabled = opt.Config.WALLoggingEnabled

	fs := NewFileStore(path)

	cache := NewCache(uint64(opt.Config.CacheMaxMemorySize))

	c := &Compactor{
		Dir:         path,
		MaxFileSize: maxTSMFileSize,
		FileStore:   fs,
		Cache:       cache,
	}

	e := &DevEngine{
		path:   path,
		logger: log.New(os.Stderr, "[tsm1dev] ", log.LstdFlags),

		WAL:   w,
		Cache: cache,

		FileStore: fs,
		Compactor: c,
		CompactionPlan: &DefaultPlanner{
			WAL:       w,
			FileStore: fs,
			Cache:     cache,
		},
		RotateFileSize:    DefaultRotateFileSize,
		MaxFileSize:       MaxDataFileSize,
		MaxPointsPerBlock: DefaultMaxPointsPerBlock,
	}

	return e
}

// Path returns the path the engine was opened with.
func (e *DevEngine) Path() string { return e.path }

// PerformMaintenance is for periodic maintenance of the store. A no-op for b1
func (e *DevEngine) PerformMaintenance() {
}

// Format returns the format type of this engine
func (e *DevEngine) Format() tsdb.EngineFormat {
	return tsdb.TSM1DevFormat
}

// Open opens and initializes the engine.
func (e *DevEngine) Open() error {
	if err := os.MkdirAll(e.path, 0777); err != nil {
		return err
	}

	if err := e.WAL.Open(); err != nil {
		return err
	}

	if err := e.FileStore.Open(); err != nil {
		return err
	}

	if err := e.reloadCache(); err != nil {
		return err
	}

	go e.compact()

	return nil
}

// Close closes the engine.
func (e *DevEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.WAL.Close()

	return nil
}

// SetLogOutput is a no-op.
func (e *DevEngine) SetLogOutput(w io.Writer) {}

// LoadMetadataIndex loads the shard metadata into memory.
func (e *DevEngine) LoadMetadataIndex(shard *tsdb.Shard, index *tsdb.DatabaseIndex, measurementFields map[string]*tsdb.MeasurementFields) error {
	keys := e.FileStore.Keys()
	for _, k := range keys {
		seriesKey, field := seriesAndFieldFromCompositeKey(k)
		measurement := tsdb.MeasurementFromSeriesKey(seriesKey)

		m := index.CreateMeasurementIndexIfNotExists(measurement)
		m.SetFieldName(field)

		typ, err := e.FileStore.Type(k)
		if err != nil {
			return err
		}

		mf := measurementFields[measurement]
		if mf == nil {
			mf = &tsdb.MeasurementFields{
				Fields: map[string]*tsdb.Field{},
			}
			measurementFields[measurement] = mf
		}

		switch typ {
		case BlockFloat64:
			if err := mf.CreateFieldIfNotExists(field, influxql.Float, false); err != nil {
				return err
			}
		case BlockInt64:
			if err := mf.CreateFieldIfNotExists(field, influxql.Integer, false); err != nil {
				return err
			}
		case BlockBool:
			if err := mf.CreateFieldIfNotExists(field, influxql.Boolean, false); err != nil {
				return err
			}
		case BlockString:
			if err := mf.CreateFieldIfNotExists(field, influxql.String, false); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unkown block type for: %v. got %v", k, typ)
		}

		_, tags, err := models.ParseKey(seriesKey)
		if err == nil {
			return err
		}

		s := tsdb.NewSeries(seriesKey, tags)
		s.InitializeShards()
		index.CreateSeriesIndexIfNotExists(measurement, s)
	}
	return nil
}

// WritePoints writes metadata and point data into the engine.
// Returns an error if new points are added to an existing key.
func (e *DevEngine) WritePoints(points []models.Point, measurementFieldsToSave map[string]*tsdb.MeasurementFields, seriesToCreate []*tsdb.SeriesCreate) error {
	values := map[string][]Value{}
	for _, p := range points {
		for k, v := range p.Fields() {
			key := string(p.Key()) + keyFieldSeparator + k
			values[key] = append(values[key], NewValue(p.Time(), v))
		}
	}

	id, err := e.WAL.WritePoints(values)
	if err != nil {
		return err
	}

	// Write data to cache for query purposes.
	return e.Cache.WriteMulti(values, uint64(id))
}

// DeleteSeries deletes the series from the engine.
func (e *DevEngine) DeleteSeries(seriesKeys []string) error {
	return fmt.Errorf("delete series not implemented")
}

// DeleteMeasurement deletes a measurement and all related series.
func (e *DevEngine) DeleteMeasurement(name string, seriesKeys []string) error {
	return fmt.Errorf("delete measurement not implemented")
}

// SeriesCount returns the number of series buckets on the shard.
func (e *DevEngine) SeriesCount() (n int, err error) {
	return 0, nil
}

// Begin starts a new transaction on the engine.
func (e *DevEngine) Begin(writable bool) (tsdb.Tx, error) {
	return &devTx{engine: e}, nil
}

func (e *DevEngine) WriteTo(w io.Writer) (n int64, err error) { panic("not implemented") }

func (e *DevEngine) compact() {
	for {
		if err := e.WAL.CloseSegment(); err != nil {
			e.logger.Printf("error rolling current WAL segment: %v", err)
			time.Sleep(time.Second)
			continue
		}

		tsmFiles, segments, err := e.CompactionPlan.Plan()
		if err != nil {
			e.logger.Printf("error calculating compaction plan: %v", err)
			time.Sleep(time.Second)
			continue
		}

		if len(tsmFiles) == 0 && len(segments) == 0 {
			time.Sleep(time.Second)
			continue
		}

		start := time.Now()
		e.logger.Printf("compacting %d WAL segments, %d TSM files", len(segments), len(tsmFiles))

		files, err := e.Compactor.Compact(tsmFiles, segments)
		if err != nil {
			e.logger.Printf("error compacting WAL segments: %v", err)
			time.Sleep(time.Second)
			continue
		}

		if err := e.FileStore.Replace(append(tsmFiles, segments...), files); err != nil {
			e.logger.Printf("error replacing new TSM files: %v", err)
			time.Sleep(time.Second)
			continue
		}

		// Inform cache data may be evicted.
		if len(segments) > 0 {
			ids := SegmentPaths(segments).IDs()
			e.Cache.SetCheckpoint(uint64(ids[len(ids)-1]))
		}

		e.logger.Printf("compacted %d segments, %d tsm into %d files in %s",
			len(segments), len(tsmFiles), len(files), time.Since(start))
	}
}

func (e *DevEngine) reloadCache() error {
	files, err := segmentFileNames(e.WAL.Path())
	if err != nil {
		return err
	}

	for _, fn := range files {
		id, err := idFromFileName(fn)
		if err != nil {
			return err
		}

		f, err := os.Open(fn)
		if err != nil {
			return err
		}

		r := NewWALSegmentReader(f)
		defer r.Close()

		// Iterate over each reader in order.  Later readers will overwrite earlier ones if values
		// overlap.
		for r.Next() {
			entry, err := r.Read()
			if err != nil {
				return err
			}

			switch t := entry.(type) {
			case *WriteWALEntry:
				if err := e.Cache.WriteMulti(t.Values, uint64(id)); err != nil {
					return err
				}
			case *DeleteWALEntry:
				// FIXME: Implement this
				// if err := e.Cache.Delete(t.Keys); err != nil {
				// 	return err
				// }
			}
		}
	}
	return nil
}

type devTx struct {
	engine *DevEngine
}

// Cursor returns a cursor for all cached and TSM-based data.
func (t *devTx) Cursor(series string, fields []string, dec *tsdb.FieldCodec, ascending bool) tsdb.Cursor {
	return &devCursor{
		cache:     t.engine.Cache.Values(SeriesFieldKey(series, fields[0]), ascending),
		ascending: ascending,
	}
}
func (t *devTx) Rollback() error                          { return nil }
func (t *devTx) Size() int64                              { panic("not implemented") }
func (t *devTx) Commit() error                            { panic("not implemented") }
func (t *devTx) WriteTo(w io.Writer) (n int64, err error) { panic("not implemented") }

// devCursor is a cursor that combines both TSM and cached data.
type devCursor struct {
	cache         Values
	position      int
	cacheKeyBuf   int64
	cacheValueBuf interface{}

	tsmKeyBuf   int64
	tsmValueBuf interface{}

	ascending bool
}

// SeekTo positions the cursor at the timestamp specified by seek and returns the
// timestamp and value.
func (c *devCursor) SeekTo(seek int64) (int64, interface{}) {
	// Seek to position in cache index.
	c.position = sort.Search(len(c.cache), func(i int) bool {
		if c.ascending {
			return c.cache[i].Time().UnixNano() >= seek
		}
		return c.cache[i].Time().UnixNano() <= seek
	})

	if len(c.cache) == 0 {
		c.cacheKeyBuf = tsdb.EOF
	}

	if c.position < len(c.cache) {
		c.cacheKeyBuf = c.cache[c.position].Time().UnixNano()
		c.cacheValueBuf = c.cache[c.position].Value()
	} else {
		c.cacheKeyBuf = tsdb.EOF
	}

	// TODO: Get the first block from tsm files for the given 'seek'
	// Seek to position to tsm block.
	c.tsmKeyBuf = tsdb.EOF

	return c.read()
}

// Next returns the next value from the cursor.
func (c *devCursor) Next() (int64, interface{}) {
	return c.read()
}

// Ascending returns whether the cursor returns data in time-ascending order.
func (c *devCursor) Ascending() bool { return c.ascending }

// read returns the next value for the cursor.
func (c *devCursor) read() (int64, interface{}) {
	var key int64
	var value interface{}

	// Determine where the next datum should come from -- the cache or the TSM files.

	switch {
	// No more data in cache or in TSM files.
	case c.cacheKeyBuf == tsdb.EOF && c.tsmKeyBuf == tsdb.EOF:
		key = tsdb.EOF

	// Both cache and tsm files have the same key, cache takes precedence.
	case c.cacheKeyBuf == c.tsmKeyBuf:
		key = c.cacheKeyBuf
		value = c.cacheValueBuf
		c.cacheKeyBuf, c.cacheValueBuf = c.nextCache()
		c.tsmKeyBuf, c.tsmValueBuf = c.nextTSM()

	// Buffered cache key precedes that in TSM file.
	case c.ascending && (c.cacheKeyBuf != tsdb.EOF && (c.cacheKeyBuf < c.tsmKeyBuf || c.tsmKeyBuf == tsdb.EOF)),
		!c.ascending && (c.cacheKeyBuf != tsdb.EOF && (c.cacheKeyBuf > c.tsmKeyBuf || c.tsmKeyBuf == tsdb.EOF)):
		key = c.cacheKeyBuf
		value = c.cacheValueBuf
		c.cacheKeyBuf, c.cacheValueBuf = c.nextCache()

	// Buffered TSM key precedes that in cache.
	default:
		key = c.tsmKeyBuf
		value = c.tsmValueBuf
		c.tsmKeyBuf, c.tsmValueBuf = c.nextTSM()
	}

	return key, value
}

// nextCache returns the next value from the cache.
func (c *devCursor) nextCache() (int64, interface{}) {
	c.position++
	if c.position >= len(c.cache) {
		return tsdb.EOF, nil
	}
	return c.cache[c.position].UnixNano(), c.cache[c.position].Value()
}

// nextTSM returns the next value from the TSM files.
func (c *devCursor) nextTSM() (int64, interface{}) {
	return tsdb.EOF, nil
}
