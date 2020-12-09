/*
Copyright 2019-2020 vChain, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package database

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/codenotary/immudb/embedded/store"

	"github.com/codenotary/immudb/pkg/api/schema"
	"github.com/codenotary/immudb/pkg/logger"
	"github.com/golang/protobuf/ptypes/empty"
)

//Db database instance
type Db struct {
	Store *store.ImmuStore

	tx *store.Tx

	Logger  logger.Logger
	options *DbOptions
}

// OpenDb Opens an existing Database from disk
func OpenDb(op *DbOptions, log logger.Logger) (*Db, error) {
	var err error

	db := &Db{
		Logger:  log,
		options: op,
	}

	dbDir := filepath.Join(op.GetDbRootPath(), op.GetDbName())

	_, dbErr := os.Stat(dbDir)
	if os.IsNotExist(dbErr) {
		return nil, fmt.Errorf("Missing database directories")
	}

	indexOptions := store.DefaultIndexOptions().WithRenewSnapRootAfter(0)

	db.Store, err = store.Open(dbDir, store.DefaultOptions().WithIndexOptions(indexOptions))
	if err != nil {
		return nil, logErr(db.Logger, "Unable to open store: %s", err)
	}

	db.tx = db.Store.NewTx()

	return db, nil
}

// NewDb Creates a new Database along with it's directories and files
func NewDb(op *DbOptions, log logger.Logger) (*Db, error) {
	var err error

	db := &Db{
		Logger:  log,
		options: op,
	}

	dbDir := filepath.Join(op.GetDbRootPath(), op.GetDbName())

	if _, dbErr := os.Stat(dbDir); dbErr == nil {
		return nil, fmt.Errorf("Database directories already exist")
	}

	if err = os.MkdirAll(dbDir, os.ModePerm); err != nil {
		return nil, logErr(db.Logger, "Unable to create data folder: %s", err)
	}

	indexOptions := store.DefaultIndexOptions().WithRenewSnapRootAfter(0)
	storeOpts := store.DefaultOptions().WithIndexOptions(indexOptions).WithMaxLinearProofLen(0)

	db.Store, err = store.Open(dbDir, storeOpts)
	if err != nil {
		return nil, logErr(db.Logger, "Unable to open store: %s", err)
	}

	db.tx = db.Store.NewTx()

	return db, nil
}

//Set ...
func (d *Db) Set(kv *schema.KeyValue) (*schema.Root, error) {
	if kv == nil {
		return nil, store.ErrIllegalArguments
	}

	id, _, alh, err := d.Store.Commit([]*store.KV{{Key: kv.Key, Value: kv.Value}})
	if err != nil {
		return nil, fmt.Errorf("unexpected error %v during %s", err, "Set")
	}

	return &schema.Root{
		Payload: &schema.RootIndex{
			Index: id,
			Root:  alh[:],
		},
	}, nil
}

//Get ...
func (d *Db) Get(k *schema.Key) (*schema.Item, error) {
	return d.GetSince(k, 0)
}

func (d *Db) waitForIndexing(ts uint64) error {
	for {
		its, err := d.Store.IndexInfo()
		if err != nil {
			return err
		}

		if its >= ts {
			break
		}

		time.Sleep(time.Duration(5) * time.Millisecond)
	}
	return nil
}

func (d *Db) GetSince(k *schema.Key, ts uint64) (*schema.Item, error) {
	err := d.waitForIndexing(ts)
	if err != nil {
		return nil, err
	}

	snapshot, err := d.Store.Snapshot()
	if err != nil {
		return nil, err
	}
	defer snapshot.Close()

	_, id, err := snapshot.Get(k.Key)
	if err != nil {
		return nil, err
	}

	d.Store.ReadTx(id, d.tx)

	val, err := d.Store.ReadValue(d.tx, k.Key)
	if err != nil {
		return nil, err
	}

	return &schema.Item{Key: k.Key, Value: val, Index: id}, err
}

// CurrentRoot ...
func (d *Db) CurrentRoot() (*schema.Root, error) {
	id, alh := d.Store.Alh()

	return &schema.Root{Payload: &schema.RootIndex{Index: id, Root: alh[:]}}, nil
}

//SafeSet ...
func (d *Db) SafeSet(opts *schema.SafeSetOptions) (*schema.Proof, error) {
	//return d.Store.SafeSet(*opts)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "SafeSet")
}

//SafeGet ...
func (d *Db) SafeGet(opts *schema.SafeGetOptions) (*schema.SafeItem, error) {
	if opts == nil {
		return nil, store.ErrIllegalArguments
	}

	// get value of key
	it, err := d.Get(&schema.Key{Key: opts.Key})
	if err != nil {
		return nil, err
	}

	// key-value inclusion proof
	err = d.Store.ReadTx(it.Index, d.tx)
	if err != nil {
		return nil, err
	}

	inclusionProof, err := d.tx.Proof(opts.Key)
	if err != nil {
		return nil, err
	}

	proof := &schema.Proof{
		InclusionProof: inclusionProofTo(inclusionProof),
		DualProof:      nil,
	}

	rootTx := d.Store.NewTx()

	err = d.Store.ReadTx(opts.RootIndex.Index, rootTx)
	if err != nil {
		return nil, err
	}

	var sourceTx, targetTx *store.Tx

	if opts.RootIndex.Index <= it.Index {
		sourceTx = rootTx
		targetTx = d.tx
	} else {
		sourceTx = d.tx
		targetTx = rootTx
	}

	dualProof, err := d.Store.DualProof(sourceTx, targetTx)
	if err != nil {
		return nil, err
	}

	proof.DualProof = dualProofTo(dualProof)

	return &schema.SafeItem{Item: it, Proof: proof}, nil
}

// SetBatch ...
func (d *Db) SetBatch(kvl *schema.KVList) (*schema.Root, error) {
	if kvl == nil {
		return nil, store.ErrIllegalArguments
	}

	entries := make([]*store.KV, len(kvl.KVs))

	id, _, alh, err := d.Store.Commit(entries)
	if err != nil {
		return nil, err
	}

	return &schema.Root{
		Payload: &schema.RootIndex{
			Index: id,
			Root:  alh[:],
		},
	}, nil
}

//GetBatch ...
func (d *Db) GetBatch(kl *schema.KeyList) (*schema.ItemList, error) {
	list := &schema.ItemList{}
	for _, key := range kl.Keys {
		item, err := d.Get(key)
		if err == nil || err == store.ErrKeyNotFound {
			if item != nil {
				list.Items = append(list.Items, item)
			}
		} else {
			return nil, err
		}
	}
	return list, nil
}

// ExecAllOps ...
func (d *Db) ExecAllOps(operations *schema.Ops) (*schema.Root, error) {
	//return d.Store.ExecAllOps(operations)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "ExecAllOps")
}

//Size ...
func (d *Db) Size() (uint64, error) {
	return d.Store.TxCount(), nil
}

//Count ...
func (d *Db) Count(prefix *schema.KeyPrefix) (*schema.ItemsCount, error) {
	//return d.Store.Count(*prefix)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "Count")
}

// CountAll ...
func (d *Db) CountAll() *schema.ItemsCount {
	//return &schema.ItemsCount{Count: d.Store.CountAll()}
	return nil
}

// Consistency ...
func (d *Db) Consistency(index *schema.Index) (*schema.DualProof, error) {
	return nil, fmt.Errorf("Functionality not yet supported: %s", "Consistency")
}

// ByIndex ...
func (d *Db) ByIndex(index *schema.Index) (*schema.Tx, error) {
	//return d.Store.ByIndex(*index)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "ByIndex")
}

//BySafeIndex ...
func (d *Db) BySafeIndex(sio *schema.SafeIndexOptions) (*schema.VerifiedTx, error) {
	//return d.Store.BySafeIndex(*sio)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "BySafeIndex")
}

//History ...
func (d *Db) History(options *schema.HistoryOptions) (*schema.ItemList, error) {
	snapshot, err := d.Store.Snapshot()
	if err != nil {
		return nil, err
	}

	limit := math.MaxInt64
	if options.Limit > 0 {
		limit = int(options.Offset + options.Limit)
	}

	tss, err := snapshot.GetTs(options.Key, int64(limit))
	if err != nil {
		return nil, err
	}

	list := &schema.ItemList{}

	for i := int(options.Offset); i < len(tss); i++ {
		ts := tss[i]

		err = d.Store.ReadTx(ts, d.tx)
		if err != nil {
			return nil, err
		}

		val, err := d.Store.ReadValue(d.tx, options.Key)
		if err != nil {
			return nil, err
		}

		item := &schema.Item{Key: options.Key, Value: val, Index: ts}

		if options.Reverse {
			list.Items = append([]*schema.Item{item}, list.Items...)
		} else {
			list.Items = append(list.Items, item)
		}
	}

	return list, nil
}

//Health ...
func (d *Db) Health(*empty.Empty) (*schema.HealthResponse, error) {
	return &schema.HealthResponse{Status: true, Version: fmt.Sprintf("%d", store.Version)}, nil
}

//ZAdd ...
func (d *Db) ZAdd(opts *schema.ZAddOptions) (*schema.Root, error) {
	//return d.Store.ZAdd(*opts)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "ZAdd")
}

// ZScan ...
func (d *Db) ZScan(opts *schema.ZScanOptions) (*schema.ZItemList, error) {
	//return d.Store.ZScan(*opts)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "ZScan")
}

//SafeZAdd ...
func (d *Db) SafeZAdd(opts *schema.SafeZAddOptions) (*schema.Proof, error) {
	//return d.Store.SafeZAdd(*opts)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "SafeZAdd")
}

//Scan ...
func (d *Db) Scan(opts *schema.ScanOptions) (*schema.ItemList, error) {
	//return d.Store.Scan(*opts)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "Scan")
}

//IScan ...
func (d *Db) IScan(opts *schema.IScanOptions) (*schema.Page, error) {
	//return d.Store.IScan(*opts)
	return nil, fmt.Errorf("Functionality not yet supported: %s", "IScan")
}

//Dump ...
func (d *Db) Dump(in *empty.Empty, stream schema.ImmuService_DumpServer) error {
	/*
		kvChan := make(chan *pb.KVList)
		done := make(chan bool)

		retrieveLists := func() {
			for {
				list, more := <-kvChan
				if more {
					stream.Send(list)
				} else {
					done <- true
					return
				}
			}
		}

		go retrieveLists()
		err := d.Store.Dump(kvChan)
		<-done

		d.Logger.Debugf("Dump stream complete")
		return err
	*/
	return fmt.Errorf("Functionality not yet supported: %s", "Dump")
}

func logErr(log logger.Logger, formattedMessage string, err error) error {
	if err != nil {
		log.Errorf(formattedMessage, err)
	}
	return err
}

// PrintTree ...
func (d *Db) PrintTree() *schema.Tree {
	//return d.Store.GetTree()
	return nil
}