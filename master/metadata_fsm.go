// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package master

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/cubefs/cubefs/depends/tiglabs/raft"
	"github.com/cubefs/cubefs/depends/tiglabs/raft/proto"
	raftstore "github.com/cubefs/cubefs/raftstore/raftstore_db"
	"github.com/cubefs/cubefs/util/fileutil"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/stat"
)

const (
	applied = "applied"
)

type raftLeaderChangeHandler func(leader uint64)

type raftPeerChangeHandler func(confChange *proto.ConfChange) (err error)

type raftUserCmdApplyHandler func(opt uint32, key string, cmdMap map[string][]byte) (err error)

type raftApplySnapshotHandler func()

// MetadataFsm represents the finite state machine of a metadata partition
type MetadataFsm struct {
	store               *raftstore.RocksDBStore
	rs                  *raft.RaftServer
	applied             uint64
	retainLogs          uint64
	leaderChangeHandler raftLeaderChangeHandler
	peerChangeHandler   raftPeerChangeHandler
	snapshotHandler     raftApplySnapshotHandler
	UserAppCmdHandler   raftUserCmdApplyHandler
	onSnapshot          bool
}

func newMetadataFsm(store *raftstore.RocksDBStore, retainsLog uint64, rs *raft.RaftServer) (fsm *MetadataFsm) {
	fsm = new(MetadataFsm)
	fsm.store = store
	fsm.rs = rs
	fsm.retainLogs = retainsLog
	return
}

// Corresponding to the LeaderChange interface in Raft library.
func (mf *MetadataFsm) registerLeaderChangeHandler(handler raftLeaderChangeHandler) {
	mf.leaderChangeHandler = handler
}

// Corresponding to the PeerChange interface in Raft library.
func (mf *MetadataFsm) registerPeerChangeHandler(handler raftPeerChangeHandler) {
	mf.peerChangeHandler = handler
}

// Corresponding to the ApplySnapshot interface in Raft library.
func (mf *MetadataFsm) registerApplySnapshotHandler(handler raftApplySnapshotHandler) {
	mf.snapshotHandler = handler
}

// Corresponding to the ApplyRaftCmd interface in Raft library.
func (mf *MetadataFsm) registerRaftUserCmdApplyHandler(handler raftUserCmdApplyHandler) {
	mf.UserAppCmdHandler = handler
}

func (mf *MetadataFsm) restore() {
	mf.restoreApplied()
}

func (mf *MetadataFsm) restoreApplied() {

	value, err := mf.store.Get(applied)
	if err != nil {
		panic(fmt.Sprintf("Failed to restore applied err:%v", err.Error()))
	}
	byteValues := value.([]byte)
	if len(byteValues) == 0 {
		mf.applied = 0
		return
	}
	applied, err := strconv.ParseUint(string(byteValues), 10, 64)
	if err != nil {
		panic(fmt.Sprintf("Failed to restore applied,err:%v ", err.Error()))
	}
	mf.applied = applied
}

// Apply implements the interface of raft.StateMachine
func (mf *MetadataFsm) Apply(command []byte, index uint64) (resp interface{}, err error) {
	cmd := new(RaftCmd)
	if err = cmd.Unmarshal(command); err != nil {
		log.LogErrorf("action[fsmApply],unmarshal data:%v, err:%v", command, err.Error())
		panic(err)
	}

	cmdMap := make(map[string][]byte)
	if cmd.Op != opSyncBatchPut {
		cmdMap[cmd.K] = cmd.V
		cmdMap[applied] = []byte(strconv.FormatUint(uint64(index), 10))
	} else {
		nestedCmdMap := make(map[string]*RaftCmd)
		if err = json.Unmarshal(cmd.V, &nestedCmdMap); err != nil {
			log.LogErrorf("action[fsmApply],unmarshal nested cmd data:%v, err:%v", command, err.Error())
			panic(err)
		}
		for cmdK, cmd := range nestedCmdMap {
			cmdMap[cmdK] = cmd.V
		}
		cmdMap[applied] = []byte(strconv.FormatUint(uint64(index), 10))
	}

	switch cmd.Op {
	case opSyncDeleteDataNode, opSyncDeleteMetaNode, opSyncDeleteVol, opSyncDeleteDataPartition, opSyncDeleteMetaPartition,
		opSyncDeleteUserInfo, opSyncDeleteAKUser, opSyncDeleteVolUser, opSyncDeleteQuota, opSyncDeleteLcNode, opSyncDeleteLcConf, opSyncS3QosDelete:
		if err = mf.delKeyAndPutIndex(cmd.K, cmdMap); err != nil {
			panic(err)
		}
	case opSyncPutFollowerApiLimiterInfo, opSyncPutApiLimiterInfo:
		mf.UserAppCmdHandler(cmd.Op, cmd.K, cmdMap)
		//if err = mf.delKeyAndPutIndex(cmd.K, cmdMap); err != nil {
		//	panic(err)
		//}
		if err = mf.store.BatchPut(cmdMap, true); err != nil {
			panic(err)
		}
	default:
		// sync put data
		if err = mf.store.BatchPut(cmdMap, true); err != nil {
			panic(err)
		}
	}

	mf.applied = index

	if mf.applied > 0 && (mf.applied%mf.retainLogs) == 0 {
		log.LogWarnf("action[Apply],truncate raft log,retainLogs[%v],index[%v]", mf.retainLogs, mf.applied)
		mf.rs.Truncate(GroupID, mf.applied)
	}
	return
}

// ApplyMemberChange implements the interface of raft.StateMachine
func (mf *MetadataFsm) ApplyMemberChange(confChange *proto.ConfChange, index uint64) (interface{}, error) {
	var err error
	if mf.peerChangeHandler != nil {
		err = mf.peerChangeHandler(confChange)
	}
	return nil, err
}

// Snapshot implements the interface of raft.StateMachine
func (mf *MetadataFsm) Snapshot() (proto.Snapshot, error) {
	snapshot := mf.store.RocksDBSnapshot()
	iterator := mf.store.Iterator(snapshot)
	iterator.SeekToFirst()
	return &MetadataSnapshot{
		applied:  mf.applied,
		snapshot: snapshot,
		fsm:      mf,
		iterator: iterator,
	}, nil
}

// ApplySnapshot implements the interface of raft.StateMachine
func (mf *MetadataFsm) ApplySnapshot(peers []proto.Peer, iterator proto.SnapIterator) (err error) {
	log.LogWarnf("action[ApplySnapshot] reset rocksdb before applying snapshot")
	mf.onSnapshot = true
	defer func() {
		mf.onSnapshot = false
	}()
	var data []byte
	// clear recovery dir
	recoveryDir := raftstore.GetRocksDBStoreRecoveryDir(mf.store.GetDir())
	if fileutil.ExistDir(recoveryDir) {
		if err = os.RemoveAll(recoveryDir); err != nil {
			log.LogErrorf("failed to remove temp dir %v, error %v", recoveryDir, err.Error())
			return
		}
	}
	rocksdbOpened := true
	removeDir := ""
	// open temp rocksdb
	tempDb, err := raftstore.NewRocksDBStore(recoveryDir, mf.store.GetLruCacheSize(), mf.store.GetWriteBufferSize())
	if err != nil {
		log.LogErrorf("failed to open temp rocksdb %v", err.Error())
		goto errHandler
	}
	// close rocksdb
	mf.store.Close()
	rocksdbOpened = false
	// remove by rename
	removeDir, err = os.MkdirTemp("", "remove_by_rename")
	if err != nil {
		log.LogErrorf("failed to get temp dir %v", err.Error())
		goto errHandler
	}
	removeDir = fmt.Sprintf("%s/remove", removeDir)
	log.LogWarnf(fmt.Sprintf("action[ApplySnapshot] begin,applied[%v]", mf.applied))
	for err == nil {
		bgTime := stat.BeginStat()
		if data, err = iterator.Next(); err != nil {
			break
		}
		stat.EndStat("ApplySnapshot-Next", err, bgTime, 1)
		cmd := &RaftCmd{}
		if err = json.Unmarshal(data, cmd); err != nil {
			tempDb.Close()
			goto errHandler
		}
		bgTime = stat.BeginStat()
		if _, err = tempDb.Put(cmd.K, cmd.V, false); err != nil {
			tempDb.Close()
			goto errHandler
		}
		stat.EndStat("ApplySnapshot-Put", err, bgTime, 1)
	}
	if err != nil && err != io.EOF {
		tempDb.Close()
		goto errHandler
	}

	if err = tempDb.Flush(); err != nil {
		log.LogError(fmt.Sprintf("action[ApplySnapshot] Flush failed,err:%v", err.Error()))
		tempDb.Close()
		goto errHandler
	}
	tempDb.Close()
	// commit point, remove by rename
	if err = os.Rename(mf.store.GetDir(), removeDir); err != nil {
		goto errHandler
	}
	if err = os.RemoveAll(removeDir); err != nil {
		err = nil
		log.LogErrorf("failed to remove directory %v", err.Error())
	}
	// rename new dir to raft store dir
	if err = os.Rename(tempDb.GetDir(), mf.store.GetDir()); err != nil {
		goto errHandler
	}
	// finish snapshot
	err = mf.store.Open()
	if err != nil {
		log.LogErrorf("failed to open rocksdb %v", err.Error())
		return err
	}
	mf.snapshotHandler()
	log.LogWarnf(fmt.Sprintf("action[ApplySnapshot] success,applied[%v]", mf.applied))
	return nil
errHandler:
	if !rocksdbOpened {
		mf.store.Open()
	}
	log.LogError(fmt.Sprintf("action[ApplySnapshot] failed,err:%v", err.Error()))
	return err
}

// HandleFatalEvent implements the interface of raft.StateMachine
func (mf *MetadataFsm) HandleFatalEvent(err *raft.FatalError) {
	panic(err.Err)
}

// HandleLeaderChange implements the interface of raft.StateMachine
func (mf *MetadataFsm) HandleLeaderChange(leader uint64) {
	if mf.leaderChangeHandler != nil {
		go mf.leaderChangeHandler(leader)
	}
}

func (mf *MetadataFsm) delKeyAndPutIndex(key string, cmdMap map[string][]byte) (err error) {
	return mf.store.DeleteKeyAndPutIndex(key, cmdMap, true)
}

// Stop stops the RaftServer
func (mf *MetadataFsm) Stop() {
	if mf.rs != nil {
		mf.rs.Stop()
	}
}
