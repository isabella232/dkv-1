package badger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto/z"

	"github.com/matttproud/golang_protobuf_extensions/pbutil"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dgraph-io/badger/v3"
	"github.com/flipkart-incubator/dkv/internal/stats"
	"github.com/flipkart-incubator/dkv/internal/storage"
	"github.com/flipkart-incubator/dkv/pkg/serverpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/ini.v1"
)

// DB interface represents the capabilities exposed
// by the underlying implmentation based on Badger engine.
type DB interface {
	storage.KVStore
	storage.Backupable
	storage.ChangePropagator
	storage.ChangeApplier
}

type badgerDB struct {
	db   *badger.DB
	opts *bdgrOpts
	stat *storage.Stat

	// Indicates a global mutation like backup and restore that
	// require exclusivity. Shall be manipulated using atomics.
	globalMutation uint32
}

type bdgrOpts struct {
	opts         badger.Options
	lgr          *zap.Logger
	statsCli     stats.Client
	sstDirectory string
	promRegistry prometheus.Registerer
}

// DBOption is used to configure the Badger
// storage engine.
type DBOption func(*bdgrOpts)

// WithLogger is used to inject a ZAP logger instance.
func WithLogger(lgr *zap.Logger) DBOption {
	return func(opts *bdgrOpts) {
		if lgr != nil {
			opts.lgr = lgr
			opts.opts = opts.opts.WithLogger(&zapBadgerLogger{lgr: lgr})
		}
	}
}

// WithStats is used to inject a metrics client.
func WithStats(statsCli stats.Client) DBOption {
	return func(opts *bdgrOpts) {
		if statsCli != nil {
			opts.statsCli = statsCli
		} else {
			opts.statsCli = stats.NewNoOpClient()
		}
	}
}

// WithPromStats is used to inject a prometheus metrics instance
func WithPromStats(registry prometheus.Registerer) DBOption {
	return func(opts *bdgrOpts) {
		if registry != nil {
			opts.promRegistry = registry
		} else {
			opts.promRegistry = stats.NewPromethousNoopRegistry()
		}
	}
}

// WithSyncWrites configures Badger to ensure every
// write is flushed to disk before acking back.
func WithSyncWrites() DBOption {
	return func(opts *bdgrOpts) {
		opts.opts = opts.opts.WithSyncWrites(true)
	}
}

// WithoutSyncWrites configures Badger to prevent
// flush to disk for every write.
func WithoutSyncWrites() DBOption {
	return func(opts *bdgrOpts) {
		opts.opts = opts.opts.WithSyncWrites(false)
	}
}

// WithCacheSize sets the value in bytes the amount of
// cache used for data blocks.
func WithCacheSize(size uint64) DBOption {
	return func(opts *bdgrOpts) {
		opts.opts = opts.opts.WithBlockCacheSize(int64(size))
	}
}

// WithBadgerConfig can be used to override internal Badger
// storage settings through the given .ini file.
func WithBadgerConfig(iniFile string) DBOption {
	return func(opts *bdgrOpts) {
		if iniFile = strings.TrimSpace(iniFile); iniFile != "" {
			if cfg, err := ini.Load(iniFile); err != nil {
				panic(fmt.Errorf("unable to load Badger configuration from given file: %s, error: %v", iniFile, err))
			} else {
				stOpts := badger.Options{}
				if err := cfg.StrictMapTo(&stOpts); err != nil {
					panic(fmt.Errorf("unable to parse Badger configuration from given file: %s, error: %v", iniFile, err))
				}
				opts.opts = stOpts
			}
		}
	}
}

// WithDBDir sets the respective Badger storage folders.
func WithDBDir(dir string) DBOption {
	return func(opts *bdgrOpts) {
		opts.opts = opts.opts.WithDir(dir).WithValueDir(dir)
	}
}

// WithSSTDir configures the directory to be used
// for SST Operation on Badger.
func WithSSTDir(sstDir string) DBOption {
	return func(opts *bdgrOpts) {
		opts.sstDirectory = sstDir
	}
}

// WithInMemory sets Badger storage to operate entirely
// in memory. No files are created on disk whatsoever.
func WithInMemory() DBOption {
	return func(opts *bdgrOpts) {
		opts.opts = opts.opts.WithInMemory(true)
	}
}

// WithMemTableSize sets Badger storage to use the desired mem table size.
func WithMemTableSize(size int64) DBOption {
	return func(opts *bdgrOpts) {
		opts.opts = opts.opts.WithMemTableSize(size)
	}
}

// OpenDB initializes a new instance of BadgerDB with the specified
// options.
func OpenDB(dbOpts ...DBOption) (kvs DB, err error) {
	noopLgr := zap.NewNop()
	opts := &bdgrOpts{
		opts:         badger.DefaultOptions("").WithLogger(&zapBadgerLogger{lgr: noopLgr}).WithMetricsEnabled(true),
		lgr:          noopLgr,
		statsCli:     stats.NewNoOpClient(),
		promRegistry: stats.NewPromethousNoopRegistry(),
	}
	for _, dbOpt := range dbOpts {
		dbOpt(opts)
	}
	return openStore(opts)
}

func openStore(bdbOpts *bdgrOpts) (*badgerDB, error) {
	db, err := badger.Open(bdbOpts.opts)
	if err != nil {
		return nil, err
	}

	bdb := badgerDB{db, bdbOpts, storage.NewStat(bdbOpts.promRegistry, "badger"), 0}
	bdb.metricsCollector()
	return &bdb, nil
}

func (bdb *badgerDB) Close() error {
	bdb.db.Close()
	return nil
}

func (bdb *badgerDB) Put(pairs ...*serverpb.KVPair) error {
	/* todo stat computation */
	metricsPrefix := "badger.put.multi"
	metricsLabel := stats.MultiPut
	if len(pairs) == 1 {
		metricsPrefix = "badger.put.single"
		metricsLabel = stats.Put
	}
	defer bdb.opts.statsCli.Timing(metricsPrefix+".latency.ms", time.Now())
	defer stats.MeasureLatency(bdb.stat.RequestLatency.WithLabelValues(metricsLabel), time.Now())

	wb := bdb.db.NewWriteBatch()
	defer wb.Cancel()
	for _, kv := range pairs {
		if kv == nil {
			continue //skip nil entries
		}
		e := badger.NewEntry(kv.Key, kv.Value)
		if kv.ExpireTS > 0 {
			e.ExpiresAt = kv.ExpireTS
		}
		err := wb.SetEntry(e)
		if err != nil {
			bdb.opts.statsCli.Incr(metricsPrefix+".errors", 1)
		}
	}
	err := wb.Flush()
	if err != nil {
		bdb.opts.statsCli.Incr(metricsPrefix+".errors", 1)
	}
	return err
}

func (bdb *badgerDB) Delete(key []byte) error {
	defer bdb.opts.statsCli.Timing("badger.delete.latency.ms", time.Now())
	defer stats.MeasureLatency(bdb.stat.RequestLatency.WithLabelValues(stats.Delete), time.Now())

	err := bdb.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
	if err != nil {
		bdb.opts.statsCli.Incr("badger.delete.errors", 1)
		bdb.stat.ResponseError.WithLabelValues(stats.Delete).Inc()
	}
	return err
}

func (bdb *badgerDB) Get(keys ...[]byte) ([]*serverpb.KVPair, error) {
	defer bdb.opts.statsCli.Timing("badger.get.latency.ms", time.Now())
	defer stats.MeasureLatency(bdb.stat.RequestLatency.WithLabelValues(stats.Get), time.Now())

	var results []*serverpb.KVPair
	err := bdb.db.View(func(txn *badger.Txn) error {
		for _, key := range keys {
			item, err := txn.Get(key)
			switch err {
			case nil:
				value, _ := item.ValueCopy(nil)
				results = append(results, &serverpb.KVPair{Key: key, Value: value})
			case badger.ErrKeyNotFound:
				continue
			default:
				return err
			}
		}
		return nil
	})
	if err != nil {
		bdb.opts.statsCli.Incr("badger.get.errors", 1)
		bdb.stat.ResponseError.WithLabelValues(stats.Get).Inc()
	}
	return results, err
}

func (bdb *badgerDB) CompareAndSet(key, expect, update []byte) (bool, error) {
	defer bdb.opts.statsCli.Timing("badger.cas.latency.ms", time.Now())
	defer stats.MeasureLatency(bdb.stat.RequestLatency.WithLabelValues(stats.CompareAndSet), time.Now())

	casTrxn := bdb.db.NewTransaction(true)
	defer casTrxn.Discard()

	exist, err := casTrxn.Get(key)
	switch {
	case err == badger.ErrKeyNotFound:
		if expect != nil && len(expect) > 0 {
			return false, nil
		}
	case err != nil:
		bdb.opts.statsCli.Incr("badger.cas.get.errors", 1)
		bdb.stat.ResponseError.WithLabelValues(stats.CompareAndSet).Inc()
		return false, err
	default:
		existVal, _ := exist.ValueCopy(nil)
		if !bytes.Equal(existVal, expect) {
			return false, nil
		}
	}
	err = casTrxn.Set(key, update)
	if err != nil {
		bdb.opts.statsCli.Incr("badger.cas.set.errors", 1)
		bdb.stat.ResponseError.WithLabelValues(stats.CompareAndSet).Inc()
		return false, err
	}
	err = casTrxn.Commit()
	if err == badger.ErrConflict {
		return false, nil
	}
	return err == nil, err
}

const (
	badgerSSTPrefix = "badger-snapshot-"
)

func (bdb *badgerDB) GetSnapshot() (io.ReadCloser, error) {
	defer bdb.opts.statsCli.Timing("badger.snapshot.get.latency.ms", time.Now())
	defer stats.MeasureLatency(bdb.stat.RequestLatency.WithLabelValues(stats.GetSnapShot), time.Now())

	sstFile, err := storage.CreateTempFile(bdb.opts.sstDirectory, badgerSSTPrefix)
	if err != nil {
		return nil, err
	}
	defer os.Remove(sstFile.Name())

	// TODO: Check if any options need to be set on stream
	strm := bdb.db.NewStream()
	w := bufio.NewWriter(sstFile)

	strm.Send = func(buf *z.Buffer) error {
		list, err := badger.BufferToKVList(buf)
		if err != nil {
			return err
		}
		for _, kv := range list.Kv {
			entry := serverpb.PutRequest{Key: kv.Key, Value: kv.Value, ExpireTS: kv.ExpiresAt}
			_, err := pbutil.WriteDelimited(w, &entry)
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := strm.Orchestrate(context.Background()); err != nil {
		return nil, err
	}

	w.Flush()
	sstFile.Close()

	return os.Open(sstFile.Name())
}

func (bdb *badgerDB) PutSnapshot(snap io.ReadCloser) error {
	defer bdb.opts.statsCli.Timing("badger.snapshot.put.latency.ms", time.Now())
	defer stats.MeasureLatency(bdb.stat.RequestLatency.WithLabelValues(stats.PutSnapShot), time.Now())

	wb := bdb.db.NewWriteBatch()
	defer wb.Cancel()

	entry := &serverpb.PutRequest{}
	for {
		entry.Reset()
		if _, err := pbutil.ReadDelimited(snap, entry); err != nil {
			if err == io.EOF {
				break
			}
		}

		kv := badger.NewEntry(entry.Key, entry.Value)
		if entry.ExpireTS > 0 {
			kv.ExpiresAt = entry.ExpireTS
		}
		if err := wb.SetEntry(kv); err != nil {
			return err
		}
	}
	return wb.Flush()
}

const backupBufSize = 64 << 20

func (bdb *badgerDB) BackupTo(file string) error {
	if err := checksForBackup(file); err != nil {
		return err
	}
	// Prevent any other backups or restores
	if err := bdb.beginGlobalMutation(); err != nil {
		return err
	}
	defer bdb.endGlobalMutation()

	bf, err := os.Create(path.Clean(file))
	if err != nil {
		return err
	}

	defer bf.Close()
	bw := bufio.NewWriterSize(bf, backupBufSize)
	if _, err = bdb.db.Backup(bw, 0); err != nil {
		return err
	}

	if err = bw.Flush(); err != nil {
		return err
	}

	return bf.Sync()
}

const (
	tempDirPrefx     = "badger-restore-"
	tempDirValPrefx  = "badger-restore-val-"
	maxPendingWrites = 256
)

func (bdb *badgerDB) RestoreFrom(file string) (st storage.KVStore, ba storage.Backupable, cp storage.ChangePropagator, ca storage.ChangeApplier, err error) {
	// Setup return vars
	st, ba, cp, ca = bdb, bdb, nil, bdb

	// Prevent any other backups or restores
	err = bdb.beginGlobalMutation()
	if err != nil {
		return
	}
	defer bdb.endGlobalMutation()

	// In any case, reopen a new DB
	defer func() {
		if !bdb.opts.opts.InMemory {
			if finalDB, openErr := openStore(bdb.opts); openErr != nil {
				err = openErr
			} else {
				st, ba, cp, ca = finalDB, finalDB, nil, finalDB
			}
		}
	}()

	// Check for the given restore file validity
	err = checksForRestore(file)
	if err != nil {
		return
	}

	// Open the given restore file
	f, err := os.Open(file)
	if err != nil {
		return
	}
	defer f.Close()

	// Create temp folder for the restored data
	restoreDir, err := storage.CreateTempFolder("", tempDirPrefx)
	if err != nil {
		return
	}

	// Create a temp badger DB pointing to the temp folder
	cloneOpts := *bdb.opts
	if !cloneOpts.opts.InMemory {
		cloneOpts.opts = cloneOpts.opts.WithDir(restoreDir).WithValueDir(restoreDir)
	}
	restoredDB, err := openStore(&cloneOpts)
	if err != nil {
		return
	}

	// Restore data in the file onto the temp badger DB
	err = restoredDB.db.Load(f, maxPendingWrites)
	if err != nil {
		return
	}

	if !cloneOpts.opts.InMemory {
		// Close the temp badger DB
		restoredDB.db.Close()

		// Move the temp folders to the actual locations
		err = storage.RenameFolder(restoreDir, bdb.opts.opts.Dir)
	} else {
		// Assign to return vars directly for diskless mode
		st, ba, cp, ca = restoredDB, restoredDB, nil, restoredDB
	}

	// Plain return due to defer function above
	return
}

const changeNumberKey = "_dkv_meta::ChangeNumber"

func (bdb *badgerDB) GetLatestAppliedChangeNumber() (uint64, error) {
	var chngNum uint64
	err := bdb.db.View(func(txn *badger.Txn) error {
		chngNumVal, err := txn.Get([]byte(changeNumberKey))
		switch {
		case err == badger.ErrKeyNotFound:
			chngNum = 0
		case err != nil:
			return err
		default:
			if err := chngNumVal.Value(func(v []byte) error {
				chngNum = binary.BigEndian.Uint64(v)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return chngNum, err
}

func (bdb *badgerDB) SaveChanges(changes []*serverpb.ChangeRecord) (uint64, error) {
	defer bdb.opts.statsCli.Timing("badger.save.changes.latency.ms", time.Now())
	defer stats.MeasureLatency(bdb.stat.RequestLatency.WithLabelValues(stats.SaveChange), time.Now())

	var appldChngNum uint64
	var lastErr error

	for _, chng := range changes {
		// Create a new badger transaction for the current change
		chngTrxn := bdb.db.NewTransaction(true)
		defer chngTrxn.Discard()

		// Load the current change number
		chngNumVal, err := chngTrxn.Get([]byte(changeNumberKey))
		var currChngNum uint64
		switch {
		case err == badger.ErrKeyNotFound:
			currChngNum = 0
		case err != nil:
			lastErr = err
		default:
			if err := chngNumVal.Value(func(v []byte) error {
				currChngNum = binary.BigEndian.Uint64(v)
				return nil
			}); err != nil {
				lastErr = err
			}
		}
		if lastErr != nil {
			break
		}

		// Loop through every transaction record of the current change and
		// apply the operation to the current badger transaction
		for _, trxnRec := range chng.Trxns {
			switch trxnRec.Type {
			case serverpb.TrxnRecord_Put:
				entry := badger.NewEntry(trxnRec.Key, trxnRec.Value)
				if trxnRec.ExpireTS > 0 {
					entry.ExpiresAt = trxnRec.ExpireTS
				}
				if lastErr = chngTrxn.SetEntry(entry); lastErr != nil {
					break
				}
			case serverpb.TrxnRecord_Delete:
				if lastErr = chngTrxn.Delete(trxnRec.Key); lastErr != nil {
					break
				}
			}
		}
		if lastErr != nil {
			break
		}

		// Increment and set the change number in the same badger transaction
		currChngNum = currChngNum + 1
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], currChngNum)
		if lastErr = chngTrxn.Set([]byte(changeNumberKey), buf[:]); lastErr != nil {
			break
		}

		// Commit the badger transaction for the current change
		if lastErr = chngTrxn.Commit(); lastErr != nil {
			break
		} else {
			appldChngNum = chng.ChangeNumber
		}
	}
	return appldChngNum, lastErr
}

func (bdb *badgerDB) GetLatestCommittedChangeNumber() (uint64, error) {
	return 0, errors.New("not implemented yet")
}

func (bdb *badgerDB) LoadChanges(fromChangeNumber uint64, maxChanges int) ([]*serverpb.ChangeRecord, error) {
	return nil, errors.New("not implemented yet")
}

type iter struct {
	itOpts  storage.IterationOptions
	txn     *badger.Txn
	it      *badger.Iterator
	iterErr error
}

func (bdbIter *iter) HasNext() bool {
	if kp, prsnt := bdbIter.itOpts.KeyPrefix(); prsnt {
		if bdbIter.it.ValidForPrefix(kp) {
			return true
		}
		if bdbIter.it.Valid() {
			bdbIter.it.Next()
			return bdbIter.HasNext()
		}
		return false
	}
	return bdbIter.it.Valid()
}

func (bdbIter *iter) Next() *serverpb.KVPair {
	defer bdbIter.it.Next()
	item := bdbIter.it.Item()
	key := item.KeyCopy(nil)
	val, err := item.ValueCopy(nil)
	if err != nil {
		bdbIter.iterErr = err
	}
	return &serverpb.KVPair{Key: key, Value: val, ExpireTS: item.ExpiresAt()}
}

func (bdbIter *iter) Err() error {
	return bdbIter.iterErr
}

func (bdbIter *iter) Close() error {
	bdbIter.it.Close()
	bdbIter.txn.Discard()
	return nil
}

func (bdb *badgerDB) newIter(itOpts storage.IterationOptions) *iter {
	txn := bdb.db.NewTransaction(false)
	it := txn.NewIterator(badger.DefaultIteratorOptions)

	if sk, prsnt := itOpts.StartKey(); prsnt {
		it.Seek(sk)
	} else {
		it.Rewind()
	}
	return &iter{itOpts, txn, it, nil}
}

func (bdb *badgerDB) Iterate(iterOpts storage.IterationOptions) storage.Iterator {
	return bdb.newIter(iterOpts)
}

var errGlobalMutation = errors.New("Another global keyspace mutation is in progress")

func (bdb *badgerDB) hasGlobalMutation() bool {
	return atomic.LoadUint32(&bdb.globalMutation) == 1
}

func (bdb *badgerDB) beginGlobalMutation() error {
	if atomic.CompareAndSwapUint32(&bdb.globalMutation, 0, 1) {
		return nil
	}
	return errGlobalMutation
}

func (bdb *badgerDB) endGlobalMutation() error {
	if atomic.CompareAndSwapUint32(&bdb.globalMutation, 1, 0) {
		return nil
	}
	return errGlobalMutation
}

func checksForBackup(bckpPath string) error {
	if len(strings.TrimSpace(bckpPath)) == 0 {
		return errors.New("valid path must be provided")
	}

	_, err := os.Stat(bckpPath)
	if err == nil {
		return errors.New("require a new file for badger backup")
	}
	_, err = os.Stat(path.Dir(bckpPath))
	return err
}

func checksForRestore(rstrPath string) error {
	switch fi, err := os.Stat(rstrPath); {
	case err != nil:
		return err
	case fi.IsDir():
		return errors.New("require a file for badger restore")
	default:
		return nil
	}
}

type zapBadgerLogger struct {
	lgr *zap.Logger
}

func (blgr *zapBadgerLogger) Errorf(msg string, args ...interface{}) {
	if ce := blgr.lgr.Check(zap.ErrorLevel, msg); ce != nil {
		blgr.log(ce, args...)
	}
}

func (blgr *zapBadgerLogger) Warningf(msg string, args ...interface{}) {
	if ce := blgr.lgr.Check(zap.WarnLevel, msg); ce != nil {
		blgr.log(ce, args...)
	}
}

func (blgr *zapBadgerLogger) Infof(msg string, args ...interface{}) {
	if ce := blgr.lgr.Check(zap.InfoLevel, msg); ce != nil {
		blgr.log(ce, args...)
	}
}

func (blgr *zapBadgerLogger) Debugf(msg string, args ...interface{}) {
	if ce := blgr.lgr.Check(zap.DebugLevel, msg); ce != nil {
		blgr.log(ce, args...)
	}
}

func (blgr *zapBadgerLogger) log(ce *zapcore.CheckedEntry, args ...interface{}) {
	flds := make([]zap.Field, len(args))
	for i, arg := range args {
		flds[i] = zap.Any(strconv.Itoa(i), arg)
	}
	ce.Write(flds...)
}
