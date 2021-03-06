// Copyright 2019-present PingCAP, Inc.
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

package raftstore

import (
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	rspb "github.com/pingcap/kvproto/pkg/raft_serverpb"
	"github.com/pingcap/log"
)

// SnapEntry represents a snapshot entry.
type SnapEntry int

// SnapEntry
const (
	SnapEntryGenerating SnapEntry = 1
	SnapEntrySending    SnapEntry = 2
	SnapEntryReceiving  SnapEntry = 3
	SnapEntryApplying   SnapEntry = 4
)

// String returns a string representation of the snapshot entry.	`
func (e SnapEntry) String() string {
	switch e {
	case SnapEntryGenerating:
		return "generating"
	case SnapEntrySending:
		return "sending"
	case SnapEntryReceiving:
		return "receiving"
	case SnapEntryApplying:
		return "applying"
	}
	return "unknown"
}

// SnapStats represents a snapshot stats.
type SnapStats struct {
	ReceivingCount int
	SendingCount   int
}

func notifyStats(router *router) {
	if router != nil {
		router.sendStore(NewMsg(MsgTypeStoreSnapshotStats, nil))
	}
}

// SnapManager represents a snapshot manager.
type SnapManager struct {
	base         string
	snapSize     *int64
	registryLock sync.RWMutex
	registry     map[SnapKey][]SnapEntry
	router       *router
	limiter      *IOLimiter
	MaxTotalSize uint64
}

// NewSnapManager returns a new SnapManager.
func NewSnapManager(path string, router *router) *SnapManager {
	return new(SnapManagerBuilder).Build(path, router)
}

func (sm *SnapManager) init() error {
	fi, err := os.Stat(sm.base)
	if os.IsNotExist(err) {
		err = os.MkdirAll(sm.base, 0600)
		if err != nil {
			return errors.WithStack(err)
		}
		return nil
	} else if err != nil {
		return errors.WithStack(err)
	}
	if !fi.IsDir() {
		return errors.Errorf("%s should be a directory", sm.base)
	}
	fis, err := ioutil.ReadDir(sm.base)
	if err != nil {
		return errors.WithStack(err)
	}
	for _, fi := range fis {
		if !fi.IsDir() {
			name := fi.Name()
			if strings.HasSuffix(name, tmpFileSuffix) {
				err = os.Remove(filepath.Join(sm.base, name))
				if err != nil {
					return errors.WithStack(err)
				}
			} else if strings.HasSuffix(name, sstFileSuffix) {
				atomic.AddInt64(sm.snapSize, fi.Size())
			}
		}
	}
	return nil
}

// ListIdleSnap lists all idle snapshots in the SnapManager.
func (sm *SnapManager) ListIdleSnap() ([]SnapKeyWithSending, error) {
	fis, err := ioutil.ReadDir(sm.base)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	results := make([]SnapKeyWithSending, 0, len(fis))
	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}
		name := fi.Name()
		if !strings.HasSuffix(name, metaFileSuffix) {
			continue
		}
		name = name[:len(name)-len(metaFileSuffix)]
		var key SnapKeyWithSending
		if strings.HasPrefix(name, snapGenPrefix) {
			key.IsSending = true
		}
		numberStrs := strings.Split(name, "_")
		if len(numberStrs) != 4 {
			return nil, errors.Errorf("failed to parse file %s", name)
		}
		key.SnapKey.RegionID, err = strconv.ParseUint(numberStrs[1], 10, 64)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		key.SnapKey.Term, err = strconv.ParseUint(numberStrs[2], 10, 64)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		key.SnapKey.Index, err = strconv.ParseUint(numberStrs[3], 10, 64)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		sm.registryLock.RLock()
		_, ok := sm.registry[key.SnapKey]
		sm.registryLock.RUnlock()
		if ok {
			// Skip those registered snapshot.
			continue
		}
		results = append(results, key)
	}
	sort.Slice(results, func(i, j int) bool {
		keyI := &results[i].SnapKey
		keyJ := &results[j].SnapKey
		if keyI.RegionID == keyJ.RegionID {
			if keyI.Term == keyJ.Term {
				if keyI.Index == keyJ.Index {
					return !results[i].IsSending
				}
				return keyI.Index < keyJ.Index
			}
			return keyI.Term < keyJ.Term
		}
		return keyI.RegionID < keyJ.RegionID
	})
	return results, nil
}

// HasRegistered checks if the snapshot key is registered.
func (sm *SnapManager) HasRegistered(key SnapKey) bool {
	sm.registryLock.RLock()
	_, ok := sm.registry[key]
	sm.registryLock.RUnlock()
	return ok
}

// GetTotalSnapSize gets the total snapshot size.
func (sm *SnapManager) GetTotalSnapSize() uint64 {
	return uint64(atomic.LoadInt64(sm.snapSize))
}

// GetSnapshotForBuilding gets the snapshot for building with the given snapshot key.
func (sm *SnapManager) GetSnapshotForBuilding(key SnapKey) (Snapshot, error) {
	if sm.GetTotalSnapSize() > sm.MaxTotalSize {
		err := sm.deleteOldIdleSnaps()
		if err != nil {
			return nil, err
		}
	}
	return NewSnapForBuilding(sm.base, key, sm.snapSize, sm, sm.limiter)
}

func (sm *SnapManager) deleteOldIdleSnaps() error {
	idleSnaps, err := sm.ListIdleSnap()
	if err != nil {
		return err
	}
	type snapWithModTime struct {
		key     SnapKey
		snap    Snapshot
		modTime time.Time
	}
	snaps := make([]snapWithModTime, 0, len(idleSnaps))
	for _, idleSnap := range idleSnaps {
		if !idleSnap.IsSending {
			continue
		}
		snap, err := sm.GetSnapshotForSending(idleSnap.SnapKey)
		if err != nil {
			continue
		}
		fi, err := snap.Meta()
		if err != nil {
			return err
		}
		snaps = append(snaps, snapWithModTime{key: idleSnap.SnapKey, snap: snap, modTime: fi.ModTime()})
	}
	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].modTime.Before(snaps[j].modTime)
	})
	for sm.GetTotalSnapSize() > sm.MaxTotalSize {
		if len(snaps) == 0 {
			return errors.New("too many snapshots")
		}
		oldest := snaps[0]
		snaps = snaps[1:]
		sm.DeleteSnapshot(oldest.key, oldest.snap, false)
	}
	return nil
}

// GetSnapshotForSending gets the snapshot for sending with the given snapshot key.
func (sm *SnapManager) GetSnapshotForSending(snapKey SnapKey) (Snapshot, error) {
	return NewSnapForSending(sm.base, snapKey, sm.snapSize, sm)
}

// GetSnapshotForReceiving gets the snapshot for receiving with the given snapshot key and data.
func (sm *SnapManager) GetSnapshotForReceiving(snapKey SnapKey, data []byte) (Snapshot, error) {
	snapshotData := new(rspb.RaftSnapshotData)
	err := snapshotData.Unmarshal(data)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return NewSnapForReceiving(sm.base, snapKey, snapshotData.Meta, sm.snapSize, sm, sm.limiter)
}

// GetSnapshotForApplying gets the snapshot for applying with the given snapshot key.
func (sm *SnapManager) GetSnapshotForApplying(snapKey SnapKey) (Snapshot, error) {
	snap, err := NewSnapForApplying(sm.base, snapKey, sm.snapSize, sm)
	if err != nil {
		return nil, err
	}
	if !snap.Exists() {
		return nil, errors.Errorf("snapshot of %s not exists", snapKey)
	}
	return snap, nil
}

// Register registers a snapshot entry with the given snapshot key.
func (sm *SnapManager) Register(key SnapKey, entry SnapEntry) {
	log.S().Debugf("register key:%s, entry:%d", key, entry)
	sm.registryLock.Lock()
	defer sm.registryLock.Unlock()
	entries, ok := sm.registry[key]
	if ok {
		for _, e := range entries {
			if e == entry {
				log.S().Warnf("%s is registered more than 1 time", key)
				return
			}
		}
	}
	entries = append(entries, entry)
	sm.registry[key] = entries
	notifyStats(sm.router)
}

// Deregister deregisters a snapshot entry with the given snapshot key.
func (sm *SnapManager) Deregister(key SnapKey, entry SnapEntry) {
	log.S().Debugf("deregister key:%s, entry:%s", key, entry)
	sm.registryLock.Lock()
	defer sm.registryLock.Unlock()
	var handled bool
	entries, ok := sm.registry[key]
	if ok {
		for i, e := range entries {
			if e == entry {
				entries = append(entries[:i], entries[i+1:]...)
				handled = true
				break
			}
		}
		if handled {
			if len(entries) > 0 {
				sm.registry[key] = entries
			} else {
				delete(sm.registry, key)
			}
			notifyStats(sm.router)
			return
		}
	}
	log.S().Warnf("stale deregister key:%s, entry:%s", key, entry)
}

// Stats returns the snapshot stats of the SnapManager.
func (sm *SnapManager) Stats() SnapStats {
	sm.registryLock.RLock()
	defer sm.registryLock.RUnlock()
	var sendingCount, receivingCount int
	for _, entries := range sm.registry {
		var isSending, isReceiving bool
		for _, entry := range entries {
			switch entry {
			case SnapEntryGenerating, SnapEntrySending:
				isSending = true
			case SnapEntryReceiving, SnapEntryApplying:
				isReceiving = true
			}
		}
		if isSending {
			sendingCount++
		}
		if isReceiving {
			receivingCount++
		}
	}
	return SnapStats{SendingCount: sendingCount, ReceivingCount: receivingCount}
}

// DeleteSnapshot deletes a snapshot.
func (sm *SnapManager) DeleteSnapshot(key SnapKey, snapshot Snapshot, checkEntry bool) bool {
	sm.registryLock.Lock()
	defer sm.registryLock.Unlock()
	if checkEntry {
		if e, ok := sm.registry[key]; ok {
			if len(e) > 0 {
				log.S().Infof("skip to delete %s since it's registered more than 1, registered entries %v",
					snapshot.Path(), e)
				return false
			}
		}
	} else if _, ok := sm.registry[key]; ok {
		log.S().Infof("skip to delete %s since it's registered.", snapshot.Path())
		return false
	}
	snapshot.Delete()
	return true
}

// SnapManagerBuilder represents a snapshot manager builder.
type SnapManagerBuilder struct {
	maxTotalSize uint64
}

// MaxTotalSize returns the max total size of the SnapManagerBuilder.
func (smb *SnapManagerBuilder) MaxTotalSize(v uint64) *SnapManagerBuilder {
	smb.maxTotalSize = v
	return smb
}

// Build builds a router with the given path.
func (smb *SnapManagerBuilder) Build(path string, router *router) *SnapManager {
	var maxTotalSize uint64 = math.MaxUint64
	if smb.maxTotalSize > 0 {
		maxTotalSize = smb.maxTotalSize
	}
	return &SnapManager{
		base:         path,
		snapSize:     new(int64),
		registry:     map[SnapKey][]SnapEntry{},
		router:       router,
		limiter:      NewInfLimiter(),
		MaxTotalSize: maxTotalSize,
	}
}
