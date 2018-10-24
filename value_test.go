/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"sync"
	"testing"

	"github.com/dgraph-io/badger/options"
	"github.com/dgraph-io/badger/y"
	humanize "github.com/dustin/go-humanize"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/trace"
)

func TestValueBasic(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	y.Check(err)
	defer os.RemoveAll(dir)

	kv, _ := Open(getTestOptions(dir))
	defer kv.Close()
	log := &kv.vlog

	// Use value big enough that the value log writes them even if SyncWrites is false.
	const val1 = "sampleval012345678901234567890123"
	const val2 = "samplevalb012345678901234567890123"
	require.True(t, len(val1) >= kv.opt.ValueThreshold)

	e := &Entry{
		Key:   []byte("samplekey"),
		Value: []byte(val1),
		meta:  bitValuePointer,
	}
	e2 := &Entry{
		Key:   []byte("samplekeyb"),
		Value: []byte(val2),
		meta:  bitValuePointer,
	}

	b := new(request)
	b.Entries = []*Entry{e, e2}

	log.write([]*request{b})
	require.Len(t, b.Ptrs, 2)
	t.Logf("Pointer written: %+v %+v\n", b.Ptrs[0], b.Ptrs[1])

	s := new(y.Slice)
	buf1, cb1, err1 := log.readValueBytes(b.Ptrs[0], s)
	buf2, cb2, err2 := log.readValueBytes(b.Ptrs[1], s)
	require.NoError(t, err1)
	require.NoError(t, err2)
	defer runCallback(cb1)
	defer runCallback(cb2)

	readEntries := []Entry{valueBytesToEntry(buf1), valueBytesToEntry(buf2)}
	require.EqualValues(t, []Entry{
		{
			Key:   []byte("samplekey"),
			Value: []byte(val1),
			meta:  bitValuePointer,
		},
		{
			Key:   []byte("samplekeyb"),
			Value: []byte(val2),
			meta:  bitValuePointer,
		},
	}, readEntries)

}

func TestValueGCManaged(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	N := 10000
	opt := getTestOptions(dir)
	opt.ValueLogMaxEntries = uint32(N / 10)
	opt.managedTxns = true
	db, err := Open(opt)
	require.NoError(t, err)
	defer db.Close()

	var ts uint64
	newTs := func() uint64 {
		ts += 1
		return ts
	}

	sz := 64 << 10
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])

		wg.Add(1)
		txn := db.NewTransactionAt(newTs(), true)
		require.NoError(t, txn.Set([]byte(fmt.Sprintf("key%d", i)), v))
		require.NoError(t, txn.CommitAt(newTs(), func(err error) {
			wg.Done()
			require.NoError(t, err)
		}))
	}

	for i := 0; i < N; i++ {
		wg.Add(1)
		txn := db.NewTransactionAt(newTs(), true)
		require.NoError(t, txn.Delete([]byte(fmt.Sprintf("key%d", i))))
		require.NoError(t, txn.CommitAt(newTs(), func(err error) {
			wg.Done()
			require.NoError(t, err)
		}))
	}
	wg.Wait()
	files, err := ioutil.ReadDir(dir)
	require.NoError(t, err)
	for _, fi := range files {
		t.Logf("File: %s. Size: %s\n", fi.Name(), humanize.Bytes(uint64(fi.Size())))
	}

	for i := 0; i < 100; i++ {
		// Try at max 100 times to GC even a single value log file.
		if err := db.RunValueLogGC(0.0001); err == nil {
			return // Done
		}
	}
	require.Fail(t, "Unable to GC even a single value log file.")
}

func TestValueGC(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20

	kv, _ := Open(opt)
	defer kv.Close()

	sz := 32 << 10
	txn := kv.NewTransaction(true)
	for i := 0; i < 100; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])
		require.NoError(t, txn.Set([]byte(fmt.Sprintf("key%d", i)), v))
		if i%20 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	for i := 0; i < 45; i++ {
		txnDelete(t, kv, []byte(fmt.Sprintf("key%d", i)))
	}

	kv.vlog.filesLock.RLock()
	lf := kv.vlog.filesMap[kv.vlog.sortedFids()[0]]
	kv.vlog.filesLock.RUnlock()

	//	lf.iterate(0, func(e Entry) bool {
	//		e.print("lf")
	//		return true
	//	})

	tr := trace.New("Test", "Test")
	defer tr.Finish()
	kv.vlog.rewrite(lf, tr)
	for i := 45; i < 100; i++ {
		key := []byte(fmt.Sprintf("key%d", i))

		require.NoError(t, kv.View(func(txn *Txn) error {
			item, err := txn.Get(key)
			require.NoError(t, err)
			val := getItemValue(t, item)
			require.NotNil(t, val)
			require.True(t, len(val) == sz, "Size found: %d", len(val))
			return nil
		}))
	}
}

func TestValueGC2(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20

	kv, _ := Open(opt)
	defer kv.Close()

	sz := 32 << 10
	txn := kv.NewTransaction(true)
	for i := 0; i < 100; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])
		require.NoError(t, txn.Set([]byte(fmt.Sprintf("key%d", i)), v))
		if i%20 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	for i := 0; i < 5; i++ {
		txnDelete(t, kv, []byte(fmt.Sprintf("key%d", i)))
	}

	for i := 5; i < 10; i++ {
		v := []byte(fmt.Sprintf("value%d", i))
		txnSet(t, kv, []byte(fmt.Sprintf("key%d", i)), v, 0)
	}

	kv.vlog.filesLock.RLock()
	lf := kv.vlog.filesMap[kv.vlog.sortedFids()[0]]
	kv.vlog.filesLock.RUnlock()

	//	lf.iterate(0, func(e Entry) bool {
	//		e.print("lf")
	//		return true
	//	})

	tr := trace.New("Test", "Test")
	defer tr.Finish()
	kv.vlog.rewrite(lf, tr)
	for i := 0; i < 5; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			_, err := txn.Get(key)
			require.Equal(t, ErrKeyNotFound, err)
			return nil
		}))
	}
	for i := 5; i < 10; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			item, err := txn.Get(key)
			require.NoError(t, err)
			val := getItemValue(t, item)
			require.NotNil(t, val)
			require.Equal(t, string(val), fmt.Sprintf("value%d", i))
			return nil
		}))
	}
	for i := 10; i < 100; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			item, err := txn.Get(key)
			require.NoError(t, err)
			val := getItemValue(t, item)
			require.NotNil(t, val)
			require.True(t, len(val) == sz, "Size found: %d", len(val))
			return nil
		}))
	}
}

func TestValueGC3(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20

	kv, err := Open(opt)
	require.NoError(t, err)
	defer kv.Close()

	// We want to test whether an iterator can continue through a value log GC.

	valueSize := 32 << 10

	var value3 []byte
	txn := kv.NewTransaction(true)
	for i := 0; i < 100; i++ {
		v := make([]byte, valueSize) // 32K * 100 will take >=3'276'800 B.
		if i == 3 {
			value3 = v
		}
		rand.Read(v[:])
		// Keys key000, key001, key002, such that sorted order matches insertion order
		require.NoError(t, txn.Set([]byte(fmt.Sprintf("key%03d", i)), v))
		if i%20 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	// Start an iterator to keys in the first value log file
	itOpt := IteratorOptions{
		PrefetchValues: false,
		PrefetchSize:   0,
		Reverse:        false,
	}

	txn = kv.NewTransaction(true)
	it := txn.NewIterator(itOpt)
	defer it.Close()
	// Walk a few keys
	it.Rewind()
	require.True(t, it.Valid())
	item := it.Item()
	require.Equal(t, []byte("key000"), item.Key())
	it.Next()
	require.True(t, it.Valid())
	item = it.Item()
	require.Equal(t, []byte("key001"), item.Key())
	it.Next()
	require.True(t, it.Valid())
	item = it.Item()
	require.Equal(t, []byte("key002"), item.Key())

	// Like other tests, we pull out a logFile to rewrite it directly

	kv.vlog.filesLock.RLock()
	logFile := kv.vlog.filesMap[kv.vlog.sortedFids()[0]]
	kv.vlog.filesLock.RUnlock()

	tr := trace.New("Test", "Test")
	defer tr.Finish()
	kv.vlog.rewrite(logFile, tr)
	it.Next()
	require.True(t, it.Valid())
	item = it.Item()
	require.Equal(t, []byte("key003"), item.Key())

	v3, err := item.ValueCopy(nil)
	require.NoError(t, err)
	require.Equal(t, value3, v3)
}

func TestValueGC4(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20

	kv, _ := Open(opt)
	defer kv.Close()

	sz := 128 << 10 // 5 entries per value log file.
	txn := kv.NewTransaction(true)
	for i := 0; i < 24; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])
		require.NoError(t, txn.Set([]byte(fmt.Sprintf("key%d", i)), v))
		if i%3 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	for i := 0; i < 8; i++ {
		txnDelete(t, kv, []byte(fmt.Sprintf("key%d", i)))
	}

	for i := 8; i < 16; i++ {
		v := []byte(fmt.Sprintf("value%d", i))
		txnSet(t, kv, []byte(fmt.Sprintf("key%d", i)), v, 0)
	}

	kv.vlog.filesLock.RLock()
	lf0 := kv.vlog.filesMap[kv.vlog.sortedFids()[0]]
	lf1 := kv.vlog.filesMap[kv.vlog.sortedFids()[1]]
	kv.vlog.filesLock.RUnlock()

	//	lf.iterate(0, func(e Entry) bool {
	//		e.print("lf")
	//		return true
	//	})

	tr := trace.New("Test", "Test")
	defer tr.Finish()
	kv.vlog.rewrite(lf0, tr)
	kv.vlog.rewrite(lf1, tr)

	// Replay value log
	kv.vlog.Replay(valuePointer{Fid: 2}, kv.replayFunction())

	for i := 0; i < 8; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			_, err := txn.Get(key)
			require.Equal(t, ErrKeyNotFound, err)
			return nil
		}))
	}
	for i := 8; i < 16; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			item, err := txn.Get(key)
			require.NoError(t, err)
			val := getItemValue(t, item)
			require.NotNil(t, val)
			require.Equal(t, string(val), fmt.Sprintf("value%d", i))
			return nil
		}))
	}
}

func TestChecksums(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	// Set up SST with K1=V1
	opts := getTestOptions(dir)
	opts.Truncate = true
	opts.ValueLogFileSize = 100 * 1024 * 1024 // 100Mb
	kv, err := Open(opts)
	require.NoError(t, err)
	require.NoError(t, kv.Close())

	var (
		k0 = []byte("k0")
		k1 = []byte("k1")
		k2 = []byte("k2")
		k3 = []byte("k3")
		v0 = []byte("value0-012345678901234567890123012345678901234567890123")
		v1 = []byte("value1-012345678901234567890123012345678901234567890123")
		v2 = []byte("value2-012345678901234567890123012345678901234567890123")
		v3 = []byte("value3-012345678901234567890123012345678901234567890123")
	)
	// Make sure the value log would actually store the item
	require.True(t, len(v0) >= kv.opt.ValueThreshold)

	// Use a vlog with K0=V0 and a (corrupted) second transaction(k1,k2)
	buf := createVlog(t, []*Entry{
		{Key: k0, Value: v0},
		{Key: k1, Value: v1},
		{Key: k2, Value: v2},
	})
	buf[len(buf)-1]++ // Corrupt last byte
	require.NoError(t, ioutil.WriteFile(vlogFilePath(dir, 0), buf, 0777))

	// K1 should exist, but K2 shouldn't.
	kv, err = Open(opts)
	require.NoError(t, err)

	require.NoError(t, kv.View(func(txn *Txn) error {
		item, err := txn.Get(k0)
		require.NoError(t, err)
		require.Equal(t, getItemValue(t, item), v0)

		_, err = txn.Get(k1)
		require.Equal(t, ErrKeyNotFound, err)

		_, err = txn.Get(k2)
		require.Equal(t, ErrKeyNotFound, err)
		return nil
	}))

	// Write K3 at the end of the vlog.
	txnSet(t, kv, k3, v3, 0)
	require.NoError(t, kv.Close())

	// The vlog should contain K0 and K3 (K1 and k2 was lost when Badger started up
	// last due to checksum failure).
	kv, err = Open(opts)
	require.NoError(t, err)

	{
		txn := kv.NewTransaction(false)

		iter := txn.NewIterator(DefaultIteratorOptions)
		iter.Seek(k0)
		require.True(t, iter.Valid())
		it := iter.Item()
		require.Equal(t, it.Key(), k0)
		require.Equal(t, getItemValue(t, it), v0)
		iter.Next()
		require.True(t, iter.Valid())
		it = iter.Item()
		require.Equal(t, it.Key(), k3)
		require.Equal(t, getItemValue(t, it), v3)

		iter.Close()
		txn.Discard()
	}

	require.NoError(t, kv.Close())
}

func TestPartialAppendToValueLog(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	// Create skeleton files.
	opts := getTestOptions(dir)
	opts.Truncate = true
	opts.ValueLogFileSize = 100 * 1024 * 1024 // 100Mb
	kv, err := Open(opts)
	require.NoError(t, err)
	require.NoError(t, kv.Close())

	var (
		k0 = []byte("k0")
		k1 = []byte("k1")
		k2 = []byte("k2")
		k3 = []byte("k3")
		v0 = []byte("value0-01234567890123456789012012345678901234567890123")
		v1 = []byte("value1-01234567890123456789012012345678901234567890123")
		v2 = []byte("value2-01234567890123456789012012345678901234567890123")
		v3 = []byte("value3-01234567890123456789012012345678901234567890123")
	)
	// Values need to be long enough to actually get written to value log.
	require.True(t, len(v3) >= kv.opt.ValueThreshold)

	// Create truncated vlog to simulate a partial append.
	// k0 - single transaction, k1 and k2 in another transaction
	buf := createVlog(t, []*Entry{
		{Key: k0, Value: v0},
		{Key: k1, Value: v1},
		{Key: k2, Value: v2},
	})
	buf = buf[:len(buf)-6]
	require.NoError(t, ioutil.WriteFile(vlogFilePath(dir, 0), buf, 0777))

	// Badger should now start up
	kv, err = Open(opts)
	require.NoError(t, err)

	require.NoError(t, kv.View(func(txn *Txn) error {
		item, err := txn.Get(k0)
		require.NoError(t, err)
		require.Equal(t, v0, getItemValue(t, item))

		_, err = txn.Get(k1)
		require.Equal(t, ErrKeyNotFound, err)
		_, err = txn.Get(k2)
		require.Equal(t, ErrKeyNotFound, err)
		return nil
	}))

	// When K3 is set, it should be persisted after a restart.
	txnSet(t, kv, k3, v3, 0)
	require.NoError(t, kv.Close())
	kv, err = Open(getTestOptions(dir))
	require.NoError(t, err)
	checkKeys(t, kv, [][]byte{k3})

	// Replay value log from beginning, badger head is past k2.
	kv.vlog.Replay(valuePointer{Fid: 0}, kv.replayFunction())
	require.NoError(t, kv.Close())
}

func TestReadOnlyOpenWithPartialAppendToValueLog(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	// Create skeleton files.
	opts := getTestOptions(dir)
	opts.ValueLogFileSize = 100 * 1024 * 1024 // 100Mb
	kv, err := Open(opts)
	require.NoError(t, err)
	require.NoError(t, kv.Close())

	var (
		k0 = []byte("k0")
		k1 = []byte("k1")
		k2 = []byte("k2")
		v0 = []byte("value0-012345678901234567890123")
		v1 = []byte("value1-012345678901234567890123")
		v2 = []byte("value2-012345678901234567890123")
	)

	// Create truncated vlog to simulate a partial append.
	// k0 - single transaction, k1 and k2 in another transaction
	buf := createVlog(t, []*Entry{
		{Key: k0, Value: v0},
		{Key: k1, Value: v1},
		{Key: k2, Value: v2},
	})
	buf = buf[:len(buf)-6]
	require.NoError(t, ioutil.WriteFile(vlogFilePath(dir, 0), buf, 0777))

	opts.ReadOnly = true
	// Badger should fail a read-only open with values to replay
	kv, err = Open(opts)
	require.Error(t, err)
	require.Regexp(t, "Database was not properly closed, cannot open read-only|Read-only mode is not supported on Windows", err.Error())
}

func TestValueLogTrigger(t *testing.T) {
	t.Skip("Difficult to trigger compaction, so skipping. Re-enable after fixing #226")
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20
	kv, err := Open(opt)
	require.NoError(t, err)

	// Write a lot of data, so it creates some work for valug log GC.
	sz := 32 << 10
	txn := kv.NewTransaction(true)
	for i := 0; i < 100; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])
		require.NoError(t, txn.Set([]byte(fmt.Sprintf("key%d", i)), v))
		if i%20 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	for i := 0; i < 45; i++ {
		txnDelete(t, kv, []byte(fmt.Sprintf("key%d", i)))
	}

	require.NoError(t, kv.RunValueLogGC(0.5))

	require.NoError(t, kv.Close())

	err = kv.RunValueLogGC(0.5)
	require.Equal(t, ErrRejected, err, "Error should be returned after closing DB.")
}

func createVlog(t *testing.T, entries []*Entry) []byte {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	opts := getTestOptions(dir)
	opts.ValueLogFileSize = 100 * 1024 * 1024 // 100Mb
	kv, err := Open(opts)
	require.NoError(t, err)
	txnSet(t, kv, entries[0].Key, entries[0].Value, entries[0].meta)
	entries = entries[1:]
	txn := kv.NewTransaction(true)
	for _, entry := range entries {
		require.NoError(t, txn.SetWithMeta(entry.Key, entry.Value, entry.meta))
	}
	require.NoError(t, txn.Commit())
	require.NoError(t, kv.Close())

	filename := vlogFilePath(dir, 0)
	buf, err := ioutil.ReadFile(filename)
	require.NoError(t, err)
	return buf
}

func TestPenultimateLogCorruption(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	opt := getTestOptions(dir)
	opt.ValueLogLoadingMode = options.FileIO
	// Each txn generates at least two entries. 3 txns will fit each file.
	opt.ValueLogMaxEntries = 5

	db0, err := Open(opt)
	require.NoError(t, err)
	fmt.Println("DB0 openened")

	h := testHelper{db: db0, t: t}
	h.writeRange(0, 7)
	h.readRange(0, 7)

	for i := 2; i >= 0; i-- {
		fpath := vlogFilePath(dir, uint32(i))
		fi, err := os.Stat(fpath)
		require.NoError(t, err)
		require.True(t, fi.Size() > 0, "Empty file at log=%d", i)
		if i == 0 {
			err := os.Truncate(fpath, fi.Size()-1)
			require.NoError(t, err)
		}
	}
	// require.NoError(t, db0.Close())
	// Simulate a crash by not closing db0, but releasing the locks.
	if db0.dirLockGuard != nil {
		require.NoError(t, db0.dirLockGuard.release())
	}
	if db0.valueDirGuard != nil {
		require.NoError(t, db0.valueDirGuard.release())
	}

	opt.Truncate = true
	db1, err := Open(opt)
	require.NoError(t, err)
	h = testHelper{db: db1, t: t}
	h.readRange(0, 2) // Only 3 should be gone.
	h.readRange(4, 7)
	require.NoError(t, db1.Close())
}

func checkKeys(t *testing.T, kv *DB, keys [][]byte) {
	i := 0
	txn := kv.NewTransaction(false)
	iter := txn.NewIterator(IteratorOptions{})
	for iter.Seek(keys[0]); iter.Valid(); iter.Next() {
		require.Equal(t, iter.Item().Key(), keys[i])
		i++
	}
	require.Equal(t, i, len(keys))
}

type testHelper struct {
	db  *DB
	t   *testing.T
	val []byte
}

func (th *testHelper) key(i int) []byte {
	return []byte(fmt.Sprintf("%d%100d", i, i))
}
func (th *testHelper) value() []byte {
	if len(th.val) > 0 {
		return th.val
	}
	th.val = make([]byte, 100)
	y.Check2(rand.Read(th.val))
	return th.val
}
func (th *testHelper) writeRange(from, to int) {
	for i := from; i < to; i++ {
		err := th.db.Update(func(txn *Txn) error {
			return txn.Set(th.key(i), th.value())
		})
		require.NoError(th.t, err)
	}
}

func (th *testHelper) readRange(from, to int) {
	for i := from; i < to; i++ {
		err := th.db.View(func(txn *Txn) error {
			item, err := txn.Get(th.key(i))
			if err != nil {
				return err
			}
			if err := item.Value(func(val []byte) error {
				if !bytes.Equal(val, th.value()) {
					th.t.Fatalf("Invalid value for key: %q", th.key(i))
				}
				return nil
			}); err != nil {
				return err
			}
			return nil
		})
		require.NoError(th.t, err)
	}
}

// Test Bug #578, which showed that if a value is moved during value log GC, an
// older version can end up at a higher level in the LSM tree than a newer
// version, causing the data to not be returned.
func TestBug578(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger")
	y.Check(err)
	defer os.RemoveAll(dir)

	opts := DefaultOptions
	opts.Dir = dir
	opts.ValueDir = dir
	opts.ValueLogMaxEntries = 64
	opts.MaxTableSize = 1 << 13

	db, err := Open(opts)
	require.NoError(t, err)

	h := testHelper{db: db, t: t}

	// Let's run this whole thing a few times.
	for j := 0; j < 10; j++ {
		t.Logf("Cycle: %d\n", j)
		h.writeRange(0, 32)
		h.writeRange(0, 10)
		h.writeRange(50, 72)
		h.writeRange(40, 72)
		h.writeRange(40, 72)

		// Run value log GC a few times.
		for i := 0; i < 5; i++ {
			db.RunValueLogGC(0.5)
		}
		h.readRange(0, 10)
	}
}

func BenchmarkReadWrite(b *testing.B) {
	rwRatio := []float32{
		0.1, 0.2, 0.5, 1.0,
	}
	valueSize := []int{
		64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384,
	}

	for _, vsz := range valueSize {
		for _, rw := range rwRatio {
			b.Run(fmt.Sprintf("%3.1f,%04d", rw, vsz), func(b *testing.B) {
				var vl valueLog
				dir, err := ioutil.TempDir("", "vlog")
				y.Check(err)
				defer os.RemoveAll(dir)
				// TODO: Fix this up.
				// err = vl.Open(nil, getTestOptions(dir))
				y.Check(err)
				defer vl.Close()
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					e := new(Entry)
					e.Key = make([]byte, 16)
					e.Value = make([]byte, vsz)
					bl := new(request)
					bl.Entries = []*Entry{e}

					var ptrs []valuePointer

					vl.write([]*request{bl})
					ptrs = append(ptrs, bl.Ptrs...)

					f := rand.Float32()
					if f < rw {
						vl.write([]*request{bl})

					} else {
						ln := len(ptrs)
						if ln == 0 {
							b.Fatalf("Zero length of ptrs")
						}
						idx := rand.Intn(ln)
						s := new(y.Slice)
						buf, cb, err := vl.readValueBytes(ptrs[idx], s)
						if err != nil {
							b.Fatalf("Benchmark Read: %v", err)
						}

						e := valueBytesToEntry(buf)
						if len(e.Key) != 16 {
							b.Fatalf("Key is invalid")
						}
						if len(e.Value) != vsz {
							b.Fatalf("Value is invalid")
						}
						cb()
					}
				}
			})
		}
	}
}
