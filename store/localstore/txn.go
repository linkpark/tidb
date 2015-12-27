// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package localstore

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/store/localstore/engine"
	"github.com/pingcap/tidb/terror"
)

var (
	_ kv.Transaction = (*dbTxn)(nil)
)

// dbTxn is not thread safe
type dbTxn struct {
	us kv.UnionStore
	// meta key value pairs for dirty key in union store
	metas map[string][]byte

	store      *dbStore // for commit
	tid        uint64
	valid      bool
	version    kv.Version          // commit version
	lockedKeys map[string]struct{} // origin version in snapshot
}

func newTxn(s *dbStore, ver kv.Version) *dbTxn {
	txn := &dbTxn{
		us:         kv.NewUnionStore(newSnapshot(s, ver)),
		store:      s,
		tid:        ver.Ver,
		valid:      true,
		version:    kv.MinVersion,
		lockedKeys: make(map[string]struct{}),
	}
	log.Debugf("[kv] Begin txn:%d", txn.tid)
	return txn
}

// Implement transaction interface
func encodeVersion(ver kv.Version) []byte {
	var b bytes.Buffer
	err := binary.Write(&b, binary.BigEndian, ver.Ver)
	if err != nil {
		log.Fatal(err)
	}
	log.Errorf("encode %v to %q", ver, b.Bytes())
	return b.Bytes()
}

func decodeVersion(val []byte) kv.Version {
	var ver kv.Version
	err := binary.Read(bytes.NewBuffer(val), binary.BigEndian, &ver.Ver)
	if err != nil {
		log.Fatal(err)
	}
	return ver
}

func (txn *dbTxn) Get(k kv.Key) ([]byte, error) {
	log.Debugf("[kv] get key:%q, txn:%d", k, txn.tid)
	metaKey := MvccEncodeVersionKey(k, kv.MetaVersion)
	metaVal, err := txn.us.GetRaw(kv.Key(metaKey))
	if err != nil {
		if terror.ErrorEqual(err, engine.ErrNotFound) {
			return nil, kv.ErrNotExist
		}
		return nil, err
	}

	var keyVer kv.Version

	for i := len(metaVal); i >= 8; i -= 8 {
		log.Error(i, len(metaVal))
		ver := decodeVersion(metaVal[i-8 : i])
		if ver.Cmp(kv.Version{Ver: txn.tid}) < 0 {
			keyVer = ver
			break
		}
	}

	if keyVer.Cmp(kv.MinVersion) == 0 {
		return nil, kv.ErrNotExist
	}

	return txn.us.GetRaw(kv.Key(MvccEncodeVersionKey(k, keyVer)))
}

func (txn *dbTxn) Set(k kv.Key, data []byte) error {
	log.Debugf("[kv] set key:%q, txn:%d", k, txn.tid)
	return txn.us.Set(k, data)
}

func (txn *dbTxn) String() string {
	return fmt.Sprintf("%d", txn.tid)
}

func (txn *dbTxn) Seek(k kv.Key) (kv.Iterator, error) {
	log.Debugf("[kv] seek key:%q, txn:%d", k, txn.tid)
	return txn.us.Seek(k)
}

func (txn *dbTxn) Delete(k kv.Key) error {
	log.Debugf("[kv] delete key:%q, txn:%d", k, txn.tid)
	return txn.us.Delete(k)
}

func (txn *dbTxn) SetOption(opt kv.Option, val interface{}) {
	txn.us.SetOption(opt, val)
}

func (txn *dbTxn) DelOption(opt kv.Option) {
	txn.us.DelOption(opt)
}

func (txn *dbTxn) doCommit() error {
	// check lazy condition pairs
	if err := txn.us.CheckLazyConditionPairs(); err != nil {
		return errors.Trace(err)
	}

	err := txn.us.WalkBuffer(func(k kv.Key, v []byte) error {
		e := txn.LockKeys(k)
		return errors.Trace(e)
	})
	if err != nil {
		return errors.Trace(err)
	}

	return txn.store.CommitTxn(txn)
}

func (txn *dbTxn) Commit() error {
	if !txn.valid {
		return errors.Trace(kv.ErrInvalidTxn)
	}
	log.Debugf("[kv] commit txn %d", txn.tid)
	defer func() {
		txn.close()
	}()

	err := txn.getMetaKeys()
	if err != nil {
		log.Error(err)
		return err
	}

	return errors.Trace(txn.doCommit())
}

func (txn *dbTxn) close() error {
	txn.us.Release()
	txn.lockedKeys = nil
	txn.valid = false
	return nil
}

func (txn *dbTxn) Rollback() error {
	if !txn.valid {
		return errors.Trace(kv.ErrInvalidTxn)
	}
	log.Warnf("[kv] Rollback txn %d", txn.tid)
	return txn.close()
}

func (txn *dbTxn) LockKeys(keys ...kv.Key) error {
	for _, key := range keys {
		txn.lockedKeys[string(key)] = struct{}{}
	}
	return nil
}

func (txn *dbTxn) getMetaKeys() error {
	// TODO: allocate once
	txn.metas = make(map[string][]byte)
	err := txn.us.WalkBuffer(func(k kv.Key, value []byte) error {
		metaKey := MvccEncodeVersionKey(kv.Key(k), kv.MetaVersion)
		metaVal, err1 := txn.us.GetRaw(kv.Key(metaKey))
		if err1 != nil {
			if terror.ErrorEqual(err1, engine.ErrNotFound) {
				txn.metas[string(metaKey)] = nil
				return nil
			}
			return err1
		}
		txn.metas[string(metaKey)] = metaVal
		return nil
	})

	return err
}
